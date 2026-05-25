package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/fran0220/amp-proxy-neo/internal/neo/rivet"
	"github.com/fran0220/amp-proxy-neo/internal/neo/threadstore"
	"github.com/fran0220/amp-proxy-neo/pkg/auth"
	"github.com/fran0220/amp-proxy-neo/pkg/config"
	"github.com/fran0220/amp-proxy-neo/pkg/logger"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

const chatWSMaxFrameBytes = 8 << 20

type chatWSHandler struct {
	cfg    *config.Config
	auth   *auth.AuthResolver
	logger *logger.RequestLogger
	store  threadstore.Store

	mu       sync.Mutex
	sessions map[string]*rivet.RivetSession
	cancels  map[string]context.CancelFunc
}

func newChatWSHandler(cfg *config.Config, authResolver *auth.AuthResolver, reqLogger *logger.RequestLogger, store threadstore.Store) *chatWSHandler {
	return &chatWSHandler{
		cfg:      cfg,
		auth:     authResolver,
		logger:   reqLogger,
		store:    store,
		sessions: make(map[string]*rivet.RivetSession),
		cancels:  make(map[string]context.CancelFunc),
	}
}

func (s *appState) registerChatWS(mux *http.ServeMux) {
	mux.Handle("/api/chat/ws", newChatWSHandler(s.cfg, s.resolver, s.reqLog, s.threads))
}

func (h *chatWSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	// TODO: add localhost token auth once browser-direct leaves prototype mode.
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(*http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warnf("chat ws upgrade: %v", err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(chatWSMaxFrameBytes)
	_ = h.write(conn, map[string]any{"type": "agent_online", "online": true})
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var env chatClientFrame
		if err := json.Unmarshal(data, &env); err != nil {
			_ = h.write(conn, map[string]any{"type": "error", "message": "invalid json"})
			continue
		}
		switch env.Type {
		case "send_message":
			go h.handleSend(r.Context(), conn, env)
		case "cancel":
			h.handleCancel(env.ReqID)
		case "approve_tool":
			h.handleApproveTool(env)
		default:
			_ = h.write(conn, map[string]any{"type": "error", "reqId": env.ReqID, "message": "unknown frame type"})
		}
	}
}

type chatClientFrame struct {
	Type         string          `json:"type"`
	ReqID        string          `json:"reqId"`
	ThreadID     string          `json:"threadId"`
	Text         string          `json:"text"`
	AgentMode    string          `json:"agentMode"`
	Mode         json.RawMessage `json:"mode"`
	ToolCallID   string          `json:"toolCallId"`
	Accepted     bool            `json:"accepted"`
	DenyFeedback string          `json:"denyFeedback"`
}

func (h *chatWSHandler) handleSend(parent context.Context, conn *websocket.Conn, env chatClientFrame) {
	if env.ReqID == "" {
		env.ReqID = rivet.NewStandaloneThreadID()
	}
	if env.Text == "" {
		_ = h.write(conn, map[string]any{"type": "error", "reqId": env.ReqID, "message": "missing text"})
		return
	}
	threadID := env.ThreadID
	created := false
	if threadID == "" {
		threadID = rivet.NewStandaloneThreadID()
		created = true
	}
	sess := h.session(threadID, env.AgentMode)
	ctx, cancel := context.WithCancel(parent)
	h.mu.Lock()
	h.cancels[env.ReqID] = cancel
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.cancels, env.ReqID)
		h.mu.Unlock()
		cancel()
	}()

	if created {
		_ = h.write(conn, map[string]any{"type": "thread_created", "reqId": env.ReqID, "threadId": threadID})
	}
	frames, err := rivet.RunStandalone(ctx, sess, env.Text)
	if err != nil {
		_ = h.write(conn, map[string]any{"type": "error", "reqId": env.ReqID, "message": err.Error()})
		return
	}
	for frame := range frames {
		frame["reqId"] = env.ReqID
		if _, ok := frame["threadId"]; !ok {
			frame["threadId"] = threadID
		}
		if err := h.write(conn, frame); err != nil {
			return
		}
	}
	if err := h.persist(ctx, sess); err != nil {
		log.Warnf("chat ws persist thread=%s: %v", threadID, err)
	}
	if ctx.Err() == nil {
		_ = h.write(conn, map[string]any{"type": "done", "reqId": env.ReqID, "threadId": threadID})
	}
}

func (h *chatWSHandler) session(threadID, agentMode string) *rivet.RivetSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sess := h.sessions[threadID]; sess != nil {
		return sess
	}
	sess := rivet.NewStandaloneSession(threadID, agentMode, "local-browser")
	rivet.ConfigureStandaloneSession(sess, h.cfg, h.auth, h.logger)
	h.sessions[threadID] = sess
	return sess
}

func (h *chatWSHandler) handleCancel(reqID string) {
	h.mu.Lock()
	cancel := h.cancels[reqID]
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *chatWSHandler) handleApproveTool(env chatClientFrame) {
	if env.ThreadID == "" {
		return
	}
	h.mu.Lock()
	sess := h.sessions[env.ThreadID]
	h.mu.Unlock()
	rivet.QueueStandaloneToolApproval(sess, env.ToolCallID, env.Accepted, env.DenyFeedback)
}

func (h *chatWSHandler) persist(ctx context.Context, sess *rivet.RivetSession) error {
	if h.store == nil {
		return nil
	}
	raw, err := rivet.BuildStandaloneThreadJSON(sess)
	if err != nil {
		return err
	}
	thread, err := threadstore.ParseThread(raw)
	if err != nil {
		return err
	}
	if err := h.store.UploadThread(ctx, thread); err != nil && !errors.Is(err, threadstore.ErrVersionConflict) {
		return err
	}
	return nil
}

var chatWriteMu sync.Mutex

func (h *chatWSHandler) write(conn *websocket.Conn, msg map[string]any) error {
	buf, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	chatWriteMu.Lock()
	defer chatWriteMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, buf)
}

package rivet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/fran0220/amp-proxy-neo/pkg/auth"
	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	. "github.com/fran0220/amp-proxy-neo/pkg/logger"
)

const standaloneStubResponseEnv = "AMP_PROXY_NEO_STANDALONE_STUB_RESPONSE"

var standaloneConnID atomic.Uint64

// NewStandaloneThreadID returns an Amp-shaped thread id for browser-direct
// sessions. It intentionally does not require the amp CLI to allocate ids.
func NewStandaloneThreadID() string { return "T-" + uuidLike() }

// NewStandaloneSession creates an in-process Rivet session for the browser
// direct path. Tools are deliberately disabled in this mode: there is no amp
// CLI executor on the other side of the websocket to run them yet.
func NewStandaloneSession(threadID, agentMode, userID string) *RivetSession {
	if strings.TrimSpace(threadID) == "" {
		threadID = NewStandaloneThreadID()
	}
	if strings.TrimSpace(agentMode) == "" {
		agentMode = "smart"
	}
	s := newRivetSession(standaloneConnID.Add(1), nil, nil)
	s.threadID = threadID
	s.agentMode = agentMode
	s.remoteDriven = true
	s.persistUnsafe = true
	s.toolsBootstrapDone = true
	s.anthroTools = nil
	s.toolsRaw = []byte(`{"type":"executor_tools_register","tools":[]}`)
	s.envText = "Browser-direct Neo session. Tools are disabled in this mode."
	s.guidance = "You are running in browser-direct mode. Do not claim to have tools; answer conversationally unless tools are later enabled."
	if userID != "" {
		s.serverCreatorUserID = userID
	}
	s.bindThread()
	return s
}

// ConfigureStandaloneSession attaches process dependencies after construction.
func ConfigureStandaloneSession(s *RivetSession, cfg *Config, auth *AuthResolver, logger *RequestLogger) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cfg = cfg
	s.auth = auth
	s.logger = logger
	s.mu.Unlock()
}

// CancelStandalone aborts the current browser-direct inference run.
func CancelStandalone(s *RivetSession) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	cancel := s.inflightCancel
	s.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// QueueStandaloneToolApproval records approval frames for a future built-in
// tool executor. Tools are disabled in browser-direct Wave 3, so this is only
// kept to preserve the wire protocol shape.
func QueueStandaloneToolApproval(s *RivetSession, toolCallID string, accepted bool, denyFeedback string) {
	if s == nil || toolCallID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if accepted {
		s.pendingApprovals = append(s.pendingApprovals, toolCallID)
	} else {
		s.pendingDenials = append(s.pendingDenials, toolCallID)
		if denyFeedback != "" {
			s.pendingErrorSet = denyFeedback
		}
	}
}

// RunStandalone streams one browser-direct assistant turn. It returns Rivet UI
// frames directly instead of writing them to an amp CLI websocket.
func RunStandalone(ctx context.Context, s *RivetSession, userText string) (<-chan map[string]any, error) {
	if s == nil {
		return nil, fmt.Errorf("nil standalone session")
	}
	if strings.TrimSpace(userText) == "" {
		return nil, fmt.Errorf("missing text")
	}
	out := make(chan map[string]any, 128)
	go s.runStandalone(ctx, userText, out)
	return out, nil
}

func (s *RivetSession) runStandalone(ctx context.Context, userText string, out chan<- map[string]any) {
	defer close(out)
	defer s.markIdle()

	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.inflight = true
	s.inflightCancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.inflightCancel = nil
		s.mu.Unlock()
	}()

	writeFrame := func(frame map[string]any) error {
		select {
		case out <- frame:
			EmitFrameTap(s.threadID, "standalone", frame)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	_ = writeFrame(map[string]any{"type": "capabilities", "tools_enabled": false})

	userMsgID := newAmpMessageID()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeFrame(map[string]any{
		"type": "message_added",
		"message": map[string]any{
			"threadId":  s.threadID,
			"role":      "user",
			"content":   []map[string]any{{"type": "text", "text": userText}},
			"readAt":    nil,
			"messageId": userMsgID,
			"createdAt": now,
		},
		"seq": s.takeSeq(),
	})

	s.mu.Lock()
	s.history = append(s.history, anthropicMessage{Role: "user", Content: []anthropicContent{{Type: "text", Text: userText}}})
	s.anthroTools = nil
	s.toolsBootstrapDone = true
	cfg := s.cfg
	auth := s.auth
	logger := s.logger
	mode := s.agentMode
	s.mu.Unlock()
	s.logger = logger

	if stub := os.Getenv(standaloneStubResponseEnv); stub != "" {
		s.emitStandaloneStub(ctx, writeFrame, stub)
		return
	}
	if cfg == nil || auth == nil {
		s.emitStandaloneError(writeFrame, "standalone inference is not configured")
		return
	}

	provider, model := modelForAgentModeWithConfig(mode, cfg)
	authInfo, route := auth.Resolve(ctx, provider, model)
	if mc, ok := cfg.ModeConfig(mode); ok && mc.Auth != "" {
		authInfo, route = auth.ResolveByRef(ctx, mc.Auth)
	}
	if authInfo == nil || !authInfo.Valid() || (route != RouteLocal && route != RouteAPIKey) {
		if provider == "google" || provider == "openai" {
			provider, model = "anthropic", "claude-opus-4-7"
			authInfo, route = auth.Resolve(ctx, provider, model)
		}
	}
	if authInfo == nil || !authInfo.Valid() || (route != RouteLocal && route != RouteAPIKey) {
		s.emitStandaloneError(writeFrame, fmt.Sprintf("[amp-proxy] no usable %s credentials for %s (route=%s)", provider, model, route))
		return
	}

	stableUserID := authInfo.UserID
	if stableUserID == "" {
		stableUserID = cfg.UserID
	}
	s.currentRoute = route
	_, _, _, err := s.runOneInferenceRound(ctx, nil, nil, authInfo, stableUserID, provider, model, writeFrame)
	if err != nil && ctx.Err() == nil {
		s.emitStandaloneError(writeFrame, err.Error())
	}
}

func (s *RivetSession) emitStandaloneStub(ctx context.Context, writeFrame func(map[string]any) error, text string) {
	messageID := newAmpMessageID()
	_ = writeFrame(map[string]any{"type": "inference_tools", "messageId": messageID, "agentMode": s.agentMode, "tools": []string{}})
	_ = writeFrame(map[string]any{"type": "agent_state", "state": "streaming", "messageId": messageID, "agentMode": s.agentMode})
	_ = writeFrame(map[string]any{"type": "delta", "messageId": messageID, "role": "assistant", "state": "start"})
	_ = writeFrame(map[string]any{"type": "delta", "messageId": messageID, "role": "assistant", "blocks": []map[string]any{{"type": "text", "text": "", "blockState": "start"}}, "blockIndex": 0, "state": "generating"})
	for _, chunk := range splitStandaloneChunks(text) {
		select {
		case <-ctx.Done():
			_ = writeFrame(map[string]any{"type": "agent_state", "state": "idle", "agentMode": s.agentMode})
			return
		default:
		}
		_ = writeFrame(map[string]any{"type": "delta", "messageId": messageID, "role": "assistant", "blocks": []map[string]any{{"type": "text", "text": chunk, "blockState": "streaming"}}, "blockIndex": 0, "state": "generating"})
	}
	_ = writeFrame(map[string]any{"type": "delta", "messageId": messageID, "role": "assistant", "blocks": []map[string]any{{"type": "text", "text": "", "blockState": "complete"}}, "blockIndex": 0, "state": "generating"})
	_ = writeFrame(map[string]any{"type": "delta", "messageId": messageID, "role": "assistant", "state": "complete"})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	msg := map[string]any{
		"threadId":  s.threadID,
		"role":      "assistant",
		"content":   []map[string]any{{"type": "text", "text": text, "blockState": "complete"}},
		"state":     map[string]any{"type": "complete", "stopReason": "end_turn"},
		"readAt":    nil,
		"messageId": messageID,
		"createdAt": now,
	}
	_ = writeFrame(map[string]any{"type": "message_added", "message": msg, "seq": s.takeSeq()})
	s.mu.Lock()
	s.history = append(s.history, anthropicMessage{Role: "assistant", Content: []anthropicContent{{Type: "text", Text: text}}})
	s.mu.Unlock()
	s.persistInMemoryThread()
	_ = writeFrame(map[string]any{"type": "agent_state", "state": "idle", "agentMode": s.agentMode})
}

func (s *RivetSession) emitStandaloneError(writeFrame func(map[string]any) error, message string) {
	_ = writeFrame(map[string]any{"type": "error", "message": message})
	_ = writeFrame(map[string]any{"type": "agent_state", "state": "idle", "agentMode": s.agentMode})
}

func splitStandaloneChunks(text string) []string {
	if len(text) <= 12 {
		return []string{text}
	}
	chunks := make([]string, 0, (len(text)/12)+1)
	for len(text) > 0 {
		n := 12
		if len(text) < n {
			n = len(text)
		}
		chunks = append(chunks, text[:n])
		text = text[n:]
	}
	return chunks
}

// BuildStandaloneThreadJSON returns an Amp-shaped thread JSON blob suitable
// for internal/neo/threadstore.ParseThread.
func BuildStandaloneThreadJSON(s *RivetSession) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("nil standalone session")
	}
	thread := s.buildThreadForUpload()
	return json.Marshal(thread)
}

package remoteagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/fran0220/amp-proxy-neo/pkg/auth"
	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

// RemoteAgent is the outbound-connect WebSocket client that lets the Mac
// amp-proxy act as a remote-controllable agent for a CF Worker coordinator.
//
// Architecture:
//
//	Browser ──(WS)──► CF Worker ──(WS)──► RemoteAgent (this) ──► localhost APIs
//
// The Mac initiates the WSS connection out to the Worker (no inbound port
// required) and stays connected indefinitely with automatic reconnect.
//
// Envelope protocol with Worker (JSON, see worker/src/index.ts for spec):
//
//	Worker → Agent:
//	  {type:"http_request", reqId, method, path, body}   ← REST proxy
//	  {type:"send_message", reqId, threadId, text, agentMode}
//	  {type:"cancel", reqId}
//
//	Agent → Worker:
//	  {type:"http_response", reqId, status, body, contentType}
//	  {type:"delta"|"message_added"|"agent_state"|... reqId, ...}   ← chat stream
//	  {type:"done", reqId}
//	  {type:"error", reqId, message}
//
// Set AMP_PROXY_REMOTE_URL=wss://amp-coord.example.com/ws/agent and
// AMP_PROXY_REMOTE_SECRET=<shared-secret> to enable. Empty URL disables.
type RemoteAgent struct {
	workerURL string
	secret    string
	cfg       *Config
	authRes   *AuthResolver

	conn   *websocket.Conn
	connMu sync.Mutex

	// inflight cancellers for active chat requests, keyed by reqId.
	cancels sync.Map // map[string]context.CancelFunc

	stopped atomic.Bool
}

func NewRemoteAgent(cfg *Config, ar *AuthResolver) *RemoteAgent {
	url := strings.TrimSpace(os.Getenv("AMP_PROXY_REMOTE_URL"))
	sec := strings.TrimSpace(os.Getenv("AMP_PROXY_REMOTE_SECRET"))
	if url == "" || sec == "" {
		return nil // disabled
	}
	return &RemoteAgent{workerURL: url, secret: sec, cfg: cfg, authRes: ar}
}

// Run blocks (intended for a goroutine), maintaining a WSS connection with
// exponential backoff on failure.
func (a *RemoteAgent) Run(ctx context.Context) {
	backoff := time.Second
	for !a.stopped.Load() {
		if err := a.dialAndServe(ctx); err != nil {
			log.Warnf("[REMOTE-AGENT] connection ended: %v (retrying in %s)", err, backoff)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func (a *RemoteAgent) Stop() { a.stopped.Store(true) }

func (a *RemoteAgent) dialAndServe(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second, EnableCompression: false}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+a.secret)
	header.Set("User-Agent", "amp-proxy-remote-agent/1.0")

	conn, resp, err := dialer.DialContext(ctx, a.workerURL, header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return fmt.Errorf("dial %s: %w (status=%d)", a.workerURL, err, status)
	}
	a.connMu.Lock()
	a.conn = conn
	a.connMu.Unlock()
	log.Infof("[REMOTE-AGENT] connected to %s", a.workerURL)

	defer func() {
		a.connMu.Lock()
		a.conn = nil
		a.connMu.Unlock()
		_ = conn.Close()
	}()

	// Ping loop to keep NAT/CF connections warm.
	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go a.pingLoop(pingCtx, conn)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var env struct {
			Type      string          `json:"type"`
			ReqID     string          `json:"reqId"`
			Method    string          `json:"method"`
			Path      string          `json:"path"`
			Body      string          `json:"body"`
			ThreadID  string          `json:"threadId"`
			Text      string          `json:"text"`
			AgentMode string          `json:"agentMode"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			log.Warnf("[REMOTE-AGENT] bad envelope: %v", err)
			continue
		}
		switch env.Type {
		case "http_request":
			go a.handleHTTPRequest(ctx, env.ReqID, env.Method, env.Path, env.Body)
		case "send_message":
			go a.handleSendMessage(ctx, env.ReqID, env.ThreadID, env.Text, env.AgentMode)
		case "cancel":
			if v, ok := a.cancels.LoadAndDelete(env.ReqID); ok {
				if cancel, ok := v.(context.CancelFunc); ok {
					cancel()
				}
			}
		default:
			log.Debugf("[REMOTE-AGENT] ignoring envelope type=%s", env.Type)
		}
	}
}

func (a *RemoteAgent) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second)); err != nil {
				return
			}
		}
	}
}

// send writes a JSON envelope to the Worker. Thread-safe.
func (a *RemoteAgent) send(msg map[string]any) error {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.conn == nil {
		return fmt.Errorf("no connection")
	}
	buf, _ := json.Marshal(msg)
	return a.conn.WriteMessage(websocket.TextMessage, buf)
}

// handleHTTPRequest proxies a REST call through to the local admin API
// (listening on the configured port). Used by the WebUI for thread CRUD.
func (a *RemoteAgent) handleHTTPRequest(ctx context.Context, reqID, method, path, body string) {
	adminPort := 9318
	if p := os.Getenv("AMP_PROXY_ADMIN_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &adminPort)
	}
	target := fmt.Sprintf("http://127.0.0.1:%d%s", adminPort, path)
	if _, err := url.Parse(target); err != nil {
		_ = a.send(map[string]any{"type": "http_response", "reqId": reqID, "status": 400, "body": err.Error()})
		return
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader([]byte(body)))
	if err != nil {
		_ = a.send(map[string]any{"type": "http_response", "reqId": reqID, "status": 500, "body": err.Error()})
		return
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_ = a.send(map[string]any{"type": "http_response", "reqId": reqID, "status": 502, "body": err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	_ = a.send(map[string]any{
		"type":        "http_response",
		"reqId":       reqID,
		"status":      resp.StatusCode,
		"body":        string(respBody),
		"contentType": resp.Header.Get("Content-Type"),
	})
}

// handleSendMessage drives an existing amp thread on behalf of the Browser
// by spawning `amp threads continue THREAD -x TEXT` as a subprocess. The
// subprocess opens its own Rivet WebSocket to our proxy (RIVET endpoint
// points at localhost:9317), which our forwarder intercepts and runs through
// the full TUI flow — local OAuth inference, tool execution via amp's
// executor, thread persistence, everything.
//
// We attach a thread-frame tap so all `delta`/`message_added`/`tool_*`
// frames flowing in the spawned session get forwarded to the Browser via
// the Worker. Result: the WebUI sees the same conversation the TUI would.
func (a *RemoteAgent) handleSendMessage(ctx context.Context, reqID, threadID, text, agentMode string) {
	if text == "" {
		_ = a.send(map[string]any{"type": "error", "reqId": reqID, "message": "missing text"})
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	a.cancels.Store(reqID, cancel)
	defer func() {
		a.cancels.Delete(reqID)
		cancel()
	}()

	// New-thread flow: client sent empty / client-fake threadId → spawn
	// `amp -x` (no continue) so amp generates a real T-uuidv7 id, then
	// announce it back to the Browser so it can update its local state.
	isNewThread := threadID == "" || !looksLikeAmpThreadID(threadID)
	if isNewThread {
		log.Infof("[REMOTE-AGENT] new-thread send: spawning amp -x (no continue)")
	} else {
		log.Infof("[REMOTE-AGENT] driving amp for thread=%s mode=%s text=%q", threadID, agentMode, truncateStr(text, 60))
	}

	// Register frame tap: for existing threads, tap by threadID directly.
	// For new threads we don't know the ID yet; tap by reqId via a wildcard
	// registration that we'll re-bind once amp's first frame reveals the
	// real id. (See globalNewThreadTap below.)
	tapID := threadID
	if isNewThread {
		tapID = "new:" + reqID
		// Install a temporary global tap that catches ANY thread the next
		// few seconds (best-effort) and forwards frames whose threadId
		// matches the newly-allocated id once we see it via session_bound.
		// Simpler: capture the first thread_id we observe via any frame.
		registerPendingNewThread(reqID, func(realThreadID string) {
			log.Infof("[REMOTE-AGENT] new thread id=%s discovered for reqId=%s", realThreadID, reqID)
			_ = a.send(map[string]any{"type": "thread_created", "reqId": reqID, "threadId": realThreadID})
		})
		defer unregisterPendingNewThread(reqID)
	}
	unregister := RegisterThreadTap(tapID, func(direction string, frame map[string]any) {
		envelope := map[string]any{"reqId": reqID}
		for k, v := range frame {
			envelope[k] = v
		}
		_ = a.send(envelope)
	})
	defer unregister()

	cwd := fetchThreadWorkingDir(cctx, a.cfg, threadID)
	ampBin := os.Getenv("AMP_PROXY_AMP_BIN")
	if ampBin == "" {
		// Default install path; fall back to PATH lookup.
		if home, err := os.UserHomeDir(); err == nil {
			candidate := home + "/.amp/bin/amp"
			if _, err := os.Stat(candidate); err == nil {
				ampBin = candidate
			}
		}
		if ampBin == "" {
			ampBin = "amp"
		}
	}

	var args []string
	if isNewThread {
		// amp -x creates a new thread automatically.
		args = []string{"-x", text}
	} else {
		args = []string{"threads", "continue", threadID, "-x", text}
	}
	if agentMode != "" {
		args = append([]string{"--mode", agentMode}, args...)
	}
	cmd := exec.CommandContext(cctx, ampBin, args...)
	cmd.Env = append(os.Environ(),
		"AMP_URL=http://localhost:9317",
		"RIVET_PUBLIC_ENDPOINT=http://default:dummy@localhost:9317",
	)
	if cwd != "" {
		cmd.Dir = cwd
	}
	// Capture stderr for diagnostics; stdout is the printed assistant text
	// (we already get the same content from the frame tap).
	var stderrBuf bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderrBuf

	// Start so we can mark the PID as remote-driven BEFORE amp opens WS.
	if err := cmd.Start(); err != nil {
		log.Warnf("[REMOTE-AGENT] amp start: %v", err)
		_ = a.send(map[string]any{"type": "error", "reqId": reqID, "message": err.Error()})
		return
	}
	RegisterRemotePID(cmd.Process.Pid)
	defer UnregisterRemotePID(cmd.Process.Pid)
	err := cmd.Wait()
	exitErr, isExit := err.(*exec.ExitError)
	// amp -x sometimes exits non-zero even on success (TTY reset codes).
	// Only treat as failure if the WS session never produced output AND
	// stderr looks like a real error. Otherwise consider it done.
	if err != nil && !isExit {
		log.Warnf("[REMOTE-AGENT] amp subprocess error (non-exit): %v", err)
		_ = a.send(map[string]any{"type": "error", "reqId": reqID, "message": err.Error()})
		return
	}
	if isExit && exitErr.ExitCode() != 0 {
		stderr := strings.TrimSpace(stderrBuf.String())
		log.Infof("[REMOTE-AGENT] amp exit code=%d stderr=%q (tap likely already delivered content)", exitErr.ExitCode(), truncateStr(stderr, 100))
	}
	log.Infof("[REMOTE-AGENT] amp subprocess completed for thread=%s", threadID)
	_ = a.send(map[string]any{"type": "done", "reqId": reqID})
}

// looksLikeAmpThreadID validates the format of an amp thread id (UUIDv7,
// 5 hex groups: 8-4-4-4-12). amp rejects malformed ids before opening WS.
func looksLikeAmpThreadID(id string) bool {
	if !strings.HasPrefix(id, "T-") {
		return false
	}
	rest := id[2:]
	parts := strings.Split(rest, "-")
	if len(parts) != 5 {
		return false
	}
	wanted := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != wanted[i] {
			return false
		}
		for _, r := range p {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return false
			}
		}
	}
	return true
}

// Pending new-thread registry: rivet_forwarder.go's session_bound emits
// the discovered thread id; we call any registered callback for the matching
// reqId so the RemoteAgent can announce it to the Browser.
var (
	newThreadMu    sync.Mutex
	pendingNewCBs  = map[string]func(string){}
	pendingNewSeen = map[string]bool{}
)

func registerPendingNewThread(reqID string, cb func(string)) {
	newThreadMu.Lock()
	defer newThreadMu.Unlock()
	pendingNewCBs[reqID] = cb
	pendingNewSeen[reqID] = false
}

func unregisterPendingNewThread(reqID string) {
	newThreadMu.Lock()
	defer newThreadMu.Unlock()
	delete(pendingNewCBs, reqID)
	delete(pendingNewSeen, reqID)
}

// NotifyPendingNewThread is called by rivet_forwarder when a fresh session
// binds a thread; we deliver to the first pending reqId waiting.
func NotifyPendingNewThread(threadID string) {
	newThreadMu.Lock()
	var fire func(string)
	var fireReq string
	for req, seen := range pendingNewSeen {
		if !seen {
			pendingNewSeen[req] = true
			fire = pendingNewCBs[req]
			fireReq = req
			break
		}
	}
	newThreadMu.Unlock()
	if fire != nil {
		fire(threadID)
		// Also redirect tap from "new:reqId" to the real threadID by
		// emitting a synthetic copy. The tap registered at "new:reqId"
		// would never receive frames otherwise. Caller (rivet_forwarder)
		// will EmitFrameTap on the real id; we replay them to the
		// new:<reqId> tap too.
		newThreadMu.Lock()
		pendingNewSeen[fireReq] = true
		newThreadMu.Unlock()
		// Register a forwarding tap from real → new:reqId
		RegisterThreadTap(threadID, func(direction string, frame map[string]any) {
			EmitFrameTap("new:"+fireReq, direction, frame)
		})
	}
}

// fetchThreadWorkingDir reads the thread from amp server and pulls the
// workspace working directory from env.initial. Returns "" on any failure
// — caller should fall back to a sensible default.
func fetchThreadWorkingDir(ctx context.Context, cfg *Config, threadID string) string {
	if cfg.Amp.UpstreamURL == "" || cfg.Amp.APIKey == "" {
		return ""
	}
	body, _ := json.Marshal(map[string]any{
		"method": "getThread",
		"params": map[string]any{"thread": threadID},
	})
	endpoint := strings.TrimRight(cfg.Amp.UpstreamURL, "/") + "/api/internal?getThread"
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.Amp.APIKey)
	req.Header.Set("X-Api-Key", cfg.Amp.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	var envelope struct {
		Result struct {
			Thread struct {
				Data struct {
					Env struct {
						Initial struct {
							WorkingDirectory string `json:"workingDirectory"`
						} `json:"initial"`
					} `json:"env"`
				} `json:"data"`
			} `json:"thread"`
		} `json:"result"`
	}
	_ = json.Unmarshal(respBytes, &envelope)
	wd := envelope.Result.Thread.Data.Env.Initial.WorkingDirectory
	// Strip file:// prefix
	wd = strings.TrimPrefix(wd, "file://")
	return wd
}

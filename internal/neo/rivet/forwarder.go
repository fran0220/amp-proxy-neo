package rivet

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/fran0220/amp-proxy-neo/pkg/auth"
	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	. "github.com/fran0220/amp-proxy-neo/pkg/logger"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func getenvImpl(k string) string { return os.Getenv(k) }

// RivetGateway accepts WebSocket connections from the Amp client (which
// thinks it's talking to the real Rivet gateway at actors.ampcode.com) and
// transparently forwards frames to the real upstream, logging each frame so we
// can identify which RPCs carry the LLM inference payload.
//
// The Rivet WS protocol smuggles routing/auth via Sec-WebSocket-Protocol
// subprotocol entries:
//
//	rivet
//	rivet_target.<actorTarget>
//	rivet_actor.<actorId>
//	rivet_encoding.<bare|json>
//	rivet_conn_params.<base64-json>
//	rivet_token.<jwt>
//
// We forward all subprotocols verbatim.
type RivetGateway struct {
	upstreamHost   string // e.g. "actors.ampcode.com"
	upstreamScheme string // "wss"
	upstreamAuth   string // pre-built "Basic <b64>" for actors.ampcode.com Rivet gateway
	upstreamToken  string // raw Rivet ACL token (the password from RIVET_PUBLIC_ENDPOINT)
	dialer         *websocket.Dialer
	httpProxy      *httputil.ReverseProxy
	authResolver   *AuthResolver
	cfg            *Config
	logger         *RequestLogger
	// injectLocal toggles local inference injection. When false the forwarder
	// is a pure transparent WS proxy (server-side LLM inference). Defaults to
	// true; set AMP_PROXY_RIVET_PASSTHROUGH=1 to disable.
	injectLocal bool
	frameSeq    uint64
	mu          sync.Mutex
}

// rivetPathPrefixes are HTTP paths that the Amp Neo CLI's Rivet client hits
// on its gateway (actors.ampcode.com). These are forwarded to actors, not to
// ampcode.com. Notably we do NOT include /auth/sign-in or /auth/sign-out —
// those are also web-UI paths on ampcode.com (for /threads/* navigation) and
// must stay on the main API. The Rivet CLI doesn't actually need
// /auth/sign-in routed to actors for normal operation.
var rivetPathPrefixes = []string{
	"/metadata",
	"/actors",
}

// New creates a Rivet gateway using the configured upstream host/credentials.
// cfg+authResolver are used by the local inference orchestrator to call the
// user's configured model providers.
func New(cfg *Config, authResolver *AuthResolver, logger *RequestLogger) *RivetGateway {
	upstreamHost, upstreamUser, upstreamPass := resolveUpstream(cfg)
	dialer := *websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{}
	dialer.HandshakeTimeout = 20 * time.Second
	dialer.EnableCompression = false

	auth := ""
	if upstreamUser != "" || upstreamPass != "" {
		token := base64.StdEncoding.EncodeToString([]byte(upstreamUser + ":" + upstreamPass))
		auth = "Basic " + token
	}

	upstreamURL := &url.URL{
		Scheme: "https",
		Host:   upstreamHost,
	}
	httpProxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	origDirector := httpProxy.Director
	httpProxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = upstreamHost
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		// Rewrite rvt-token query param if present (Rivet's per-request ACL).
		if upstreamPass != "" {
			q := req.URL.Query()
			if q.Get("rvt-token") != "" {
				q.Set("rvt-token", upstreamPass)
				req.URL.RawQuery = q.Encode()
			}
		}
	}
	httpProxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
			log.Warnf("[RIVET-HTTP] %s %s → %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
		}
		return nil
	}
	httpProxy.FlushInterval = -1

	inject := true
	if v := strings.ToLower(strings.TrimSpace(getenv("AMP_PROXY_RIVET_PASSTHROUGH"))); v == "1" || v == "true" || v == "yes" {
		inject = false
	}

	return &RivetGateway{
		upstreamHost:   upstreamHost,
		upstreamScheme: "wss",
		upstreamAuth:   auth,
		upstreamToken:  upstreamPass,
		dialer:         &dialer,
		httpProxy:      httpProxy,
		authResolver:   authResolver,
		cfg:            cfg,
		logger:         logger,
		injectLocal:    inject,
	}
}

// resolveUpstream determines the actors gateway host + ACL credentials.
// Priority: explicit config → AMP_PROXY_RIVET_ENDPOINT env → original
// RIVET_PUBLIC_ENDPOINT env (parsed). Defaults host to actors.ampcode.com.
func resolveUpstream(cfg *Config) (host, user, pass string) {
	host = cfg.Amp.RivetHost
	user = cfg.Amp.RivetUser
	pass = cfg.Amp.RivetPass

	candidates := []string{
		os.Getenv("AMP_PROXY_RIVET_ENDPOINT"),
		os.Getenv("RIVET_PUBLIC_ENDPOINT"),
	}
	for _, raw := range candidates {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if host == "" && u.Host != "" {
			host = u.Host
		}
		if (user == "" && pass == "") && u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		if host != "" && (user != "" || pass != "") {
			break
		}
	}
	if host == "" {
		host = "actors.ampcode.com"
	}
	return host, user, pass
}

// getenv is a tiny indirection so tests can swap it out; uses os.Getenv at
// package level.
var getenv = func(k string) string { return getenvImpl(k) }

// rewriteRivetToken substitutes the rvt-token query parameter (set by the
// client from RIVET_PUBLIC_ENDPOINT's basic-auth password) with the real
// upstream credential. Used for both WS dial URL and HTTP forwarded requests.
func (f *RivetGateway) rewriteRivetToken(u *url.URL) {
	if f.upstreamToken == "" {
		return
	}
	q := u.Query()
	if q.Get("rvt-token") == "" {
		return
	}
	q.Set("rvt-token", f.upstreamToken)
	u.RawQuery = q.Encode()
}

// IsRivetPath reports whether an HTTP path should be routed to the Rivet
// gateway upstream rather than the Amp API upstream.
func (f *RivetGateway) IsRivetPath(path string) bool {
	for _, p := range rivetPathPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") || strings.HasPrefix(path, p+"?") {
			return true
		}
	}
	return false
}

// HandleHTTP forwards an HTTP request to the Rivet gateway upstream.
func (f *RivetGateway) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	log.Infof("[RIVET-HTTP] %s %s", r.Method, r.URL.Path)
	f.httpProxy.ServeHTTP(w, r)
}

// CanHandle returns true if this WS upgrade looks like an Amp Neo Rivet
// gateway connection (carries the "rivet" subprotocol).
func (f *RivetGateway) CanHandle(r *http.Request) bool {
	if !isWebSocketUpgrade(r) {
		return false
	}
	for _, sp := range websocket.Subprotocols(r) {
		if sp == "rivet" || strings.HasPrefix(sp, "rivet_") {
			return true
		}
	}
	return false
}

func (f *RivetGateway) nextSeq() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frameSeq++
	return f.frameSeq
}

// Handle accepts the client WS, opens upstream, pipes frames both ways.
func (f *RivetGateway) Handle(w http.ResponseWriter, r *http.Request) {
	subprotocols := websocket.Subprotocols(r)
	connID := f.nextSeq()
	log.Infof("[RIVET %d] incoming WS path=%s host=%s subprotocols=%v", connID, r.URL.Path, r.Host, subprotocols)

	upstreamURL := url.URL{
		Scheme:   f.upstreamScheme,
		Host:     f.upstreamHost,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}
	f.rewriteRivetToken(&upstreamURL)

	reqHeader := http.Header{}
	for k, vs := range r.Header {
		switch strings.ToLower(k) {
		case "upgrade", "connection", "sec-websocket-version", "sec-websocket-key",
			"sec-websocket-extensions", "sec-websocket-protocol", "host", "authorization":
			continue
		}
		for _, v := range vs {
			reqHeader.Add(k, v)
		}
	}
	if reqHeader.Get("Origin") == "" {
		reqHeader.Set("Origin", "https://"+f.upstreamHost)
	}
	if f.upstreamAuth != "" {
		reqHeader.Set("Authorization", f.upstreamAuth)
	}

	dialer := *f.dialer
	dialer.Subprotocols = subprotocols

	upstream, resp, err := dialer.Dial(upstreamURL.String(), reqHeader)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		log.Errorf("[RIVET %d] upstream dial failed: %v (status=%d)", connID, err, status)
		http.Error(w, "rivet upstream dial failed", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	negotiated := upstream.Subprotocol()
	log.Infof("[RIVET %d] upstream connected subprotocol=%q", connID, negotiated)

	clientUpgrader := websocket.Upgrader{
		ReadBufferSize:    8192,
		WriteBufferSize:   8192,
		CheckOrigin:       func(*http.Request) bool { return true },
		Subprotocols:      []string{negotiated},
		EnableCompression: false,
	}
	client, err := clientUpgrader.Upgrade(w, r, http.Header{})
	if err != nil {
		log.Errorf("[RIVET %d] client upgrade failed: %v", connID, err)
		return
	}
	defer client.Close()

	log.Infof("[RIVET %d] bidi pipe established", connID)

	// Per-connection session state + serialization for client writes (since
	// both the inference orchestrator and the upstream pipe may write
	// concurrently).
	session := newRivetSession(connID, f.logger)
	threadID, agentMode := extractRivetSessionInfo(subprotocols)
	session.threadID = threadID
	if agentMode == "" {
		agentMode = "smart"
	}
	session.agentMode = agentMode
	log.Infof("[RIVET %d] session bound: thread=%s agentMode=%s", connID, threadID, agentMode)
	session.bindThread()
	// Notify any pending RemoteAgent "new thread" request that's waiting
	// to learn the freshly-allocated thread id.
	notifyPendingNewThread(threadID)
	var clientWriteMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(2)

	closeBoth := func(reason string) {
		_ = client.Close()
		_ = upstream.Close()
		log.Infof("[RIVET %d] closing: %s", connID, reason)
	}

	// Client -> upstream: capture context, intercept LLM trigger.
	go func() {
		defer wg.Done()
		f.pipeClient(connID, client, upstream, session, &clientWriteMu, closeBoth)
	}()
	// Upstream -> client: pass through (server still sends control frames we
	// rely on — agent_state startup, executor_connected, plugin_message hooks).
	go func() {
		defer wg.Done()
		f.pipeUpstream(connID, upstream, client, &clientWriteMu, closeBoth)
	}()

	wg.Wait()
	log.Infof("[RIVET %d] closed", connID)
}

// pipeClient forwards client->upstream frames, but inspects each one. If the
// session decides to handle the frame locally (e.g. client_append_user_msg
// triggers local inference), the frame is suppressed and the orchestrator
// fires in a background goroutine.
func (f *RivetGateway) pipeClient(
	connID uint64,
	client, upstream *websocket.Conn,
	session *RivetSession,
	clientWriteMu *sync.Mutex,
	closeBoth func(string),
) {
	for {
		msgType, data, err := client.ReadMessage()
		if err != nil {
			closeBoth(fmt.Sprintf("> read end (%v)", err))
			return
		}

		if msgType == websocket.TextMessage && f.injectLocal {
			// Plugin hook replies belong to our injected requests; consume locally.
			if session.tryDeliverPluginReply(data) {
				f.logFrame(connID, ">", msgType, data)
				log.Infof("[RIVET %d] captured plugin reply", connID)
				continue
			}
			// Tool execution results from the local executor belong to us.
			if session.tryDeliverToolResult(data) {
				f.logFrame(connID, ">", msgType, data)
				log.Infof("[RIVET %d] captured tool result", connID)
				continue
			}
			// Suppress the executor's tool_lease_ack / guidance_discovery for
			// the same toolCallId — these are housekeeping replies that the
			// real server consumes silently. We do the same.
			if t := gjson.GetBytes(data, "type").String(); t == "executor_tool_lease_ack" || t == "executor_guidance_discovery" {
				id := gjson.GetBytes(data, "toolCallId").String()
				if session.isPendingTool(id) {
					f.logFrame(connID, ">", msgType, data)
					log.Infof("[RIVET %d] swallowed %s for %s", connID, t, id)
					continue
				}
			}
			suppress, shouldRun, kind, userText, userMsgID := session.observeClient(data)
			f.logFrame(connID, ">", msgType, data)

			// Drain any pending tool-approval/denial bridges accumulated by
			// observeClient and emit responses to the executor.
			if approvals := session.takePendingApprovals(); len(approvals) > 0 {
				for _, id := range approvals {
					resp, _ := json.Marshal(map[string]any{
						"type":       "executor_tool_approval_response",
						"toolCallId": id,
						"accepted":   true,
					})
					clientWriteMu.Lock()
					_ = client.WriteMessage(websocket.TextMessage, resp)
					clientWriteMu.Unlock()
				}
			}
			if denials := session.takePendingDenials(); len(denials) > 0 {
				for _, id := range denials {
					resp, _ := json.Marshal(map[string]any{
						"type":       "executor_tool_approval_response",
						"toolCallId": id,
						"accepted":   false,
						"input":      map[string]any{"denyFeedback": "Denied by amp-proxy permissions policy"},
					})
					clientWriteMu.Lock()
					_ = client.WriteMessage(websocket.TextMessage, resp)
					clientWriteMu.Unlock()
				}
			}
			if errMsg := session.takePendingErrorSet(); errMsg != "" {
				errFrame, _ := json.Marshal(map[string]any{
					"type":  "error_set",
					"error": map[string]any{"message": errMsg, "source": "executor"},
					"seq":   session.takeSeq(),
				})
				clientWriteMu.Lock()
				_ = client.WriteMessage(websocket.TextMessage, errFrame)
				clientWriteMu.Unlock()
			}

			if suppress {
				if shouldRun {
					log.Infof("[RIVET %d] %s -> local inference (msgId=%s)", connID, kind, userMsgID)
					go func(text, msgID string) {
						if err := session.runLocalInference(context.Background(), client, clientWriteMu, f.authResolver, f.cfg, text, msgID); err != nil {
							log.Errorf("[RIVET %d] local inference failed: %v", connID, err)
						}
					}(userText, userMsgID)
				} else {
					log.Infof("[RIVET %d] suppressed dup/in-flight %s msgId=%s", connID, kind, userMsgID)
				}
				continue
			}
		} else {
			f.logFrame(connID, ">", msgType, data)
		}

		if err := upstream.WriteMessage(msgType, data); err != nil {
			closeBoth(fmt.Sprintf("> upstream write (%v)", err))
			return
		}
	}
}

// pipeUpstream forwards upstream->client frames verbatim, serialized through
// clientWriteMu so it cannot interleave with inference-injected frames.
func (f *RivetGateway) pipeUpstream(
	connID uint64,
	upstream, client *websocket.Conn,
	clientWriteMu *sync.Mutex,
	closeBoth func(string),
) {
	for {
		msgType, data, err := upstream.ReadMessage()
		if err != nil {
			closeBoth(fmt.Sprintf("< read end (%v)", err))
			return
		}
		f.logFrame(connID, "<", msgType, data)
		clientWriteMu.Lock()
		err = client.WriteMessage(msgType, data)
		clientWriteMu.Unlock()
		if err != nil {
			closeBoth(fmt.Sprintf("< client write (%v)", err))
			return
		}
	}
}

// extractRivetSessionInfo pulls the threadId and agentMode out of the JWT
// inside rivet_conn_params.<urlencoded JSON>. JWT payload shape:
//
//	{"tid":"T-...","oid":"user_...","am":"smart","sub":...,"iss":"amp-workers","aud":"amp-dtw",...}
func extractRivetSessionInfo(subprotocols []string) (threadID, agentMode string) {
	for _, sp := range subprotocols {
		const prefix = "rivet_conn_params."
		if !strings.HasPrefix(sp, prefix) {
			continue
		}
		raw, err := url.QueryUnescape(strings.TrimPrefix(sp, prefix))
		if err != nil {
			continue
		}
		token := gjson.Get(raw, "wsToken").String()
		if token == "" {
			continue
		}
		parts := strings.SplitN(token, ".", 3)
		if len(parts) < 2 {
			continue
		}
		payload, err := base64URLDecode(parts[1])
		if err != nil {
			continue
		}
		threadID = gjson.GetBytes(payload, "tid").String()
		agentMode = gjson.GetBytes(payload, "am").String()
		if threadID != "" {
			return
		}
	}
	return
}

func base64URLDecode(s string) ([]byte, error) {
	// RFC 4648 base64url without padding
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	return base64.StdEncoding.DecodeString(s)
}

func (f *RivetGateway) logFrame(connID uint64, direction string, msgType int, data []byte) {
	dumpWSFrame(connID, direction, msgType, data)
	tname := "?"
	switch msgType {
	case websocket.TextMessage:
		tname = "txt"
	case websocket.BinaryMessage:
		tname = "bin"
	case websocket.PingMessage:
		tname = "ping"
	case websocket.PongMessage:
		tname = "pong"
	case websocket.CloseMessage:
		tname = "close"
	}
	preview := ""
	if msgType == websocket.TextMessage {
		preview = string(data)
		if strings.Contains(preview, `"executor_tools_register"`) {
			_ = os.WriteFile("/tmp/amp-tools-register.json", data, 0644)
		}
		if len(preview) > 256 {
			preview = preview[:256] + "...(truncated)"
		}
	} else {
		n := len(data)
		head := n
		if head > 64 {
			head = 64
		}
		preview = hex.EncodeToString(data[:head])
		if n > head {
			preview += fmt.Sprintf("...(%dB total)", n)
		}
	}
	log.Infof("[RIVET %d %s] %s len=%d %s", connID, direction, tname, len(data), preview)
}

// Helper for io.Discard reads (not currently used)
var _ = io.Discard

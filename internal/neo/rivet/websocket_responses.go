package rivet

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	. "github.com/fran0220/amp-proxy-neo/pkg/auth"
	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	. "github.com/fran0220/amp-proxy-neo/pkg/logger"
	. "github.com/fran0220/amp-proxy-neo/pkg/provider"
	. "github.com/fran0220/amp-proxy-neo/pkg/retry"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// WebSocketResponsesHandler proxies OpenAI Responses API WebSocket connections
// through local credentials or API keys instead of the Amp upstream.
// When the upstream supports WebSocket (e.g. api.openai.com), it proxies WS↔WS.
// When the upstream only supports HTTP (e.g. NewAPI proxies), it bridges WS↔HTTP SSE.
type WebSocketResponsesHandler struct {
	cfg          *Config
	authResolver *AuthResolver
	logger       *RequestLogger
	retryer      *Retryer
	client       *http.Client
}

func NewWebSocketResponsesHandler(cfg *Config, authResolver *AuthResolver, logger *RequestLogger) *WebSocketResponsesHandler {
	return &WebSocketResponsesHandler{
		cfg:          cfg,
		authResolver: authResolver,
		logger:       logger,
		retryer:      NewRetryer(cfg.Retry.MaxAttempts, cfg.Retry.InitialDelay),
		client:       &http.Client{},
	}
}

// CanHandle checks whether this handler can serve the WebSocket request locally
// (i.e. non-amp auth is available for OpenAI). Returns false when the caller
// should fall back to the upstream proxy.
func (h *WebSocketResponsesHandler) CanHandle(r *http.Request) bool {
	auth, route := h.authResolver.Resolve(r.Context(), "openai", "gpt-5.4")
	return route != RouteAmp && auth != nil && auth.Valid()
}

// Handle upgrades the client connection and routes to either WS↔WS proxy
// or WS↔HTTP SSE bridge depending on whether the upstream has a custom base URL.
func (h *WebSocketResponsesHandler) Handle(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	auth, resolvedRoute := h.authResolver.Resolve(r.Context(), "openai", "gpt-5.4")
	if resolvedRoute == RouteAmp || auth == nil || !auth.Valid() {
		http.Error(w, `{"error":"no local auth available for openai websocket"}`, http.StatusServiceUnavailable)
		return
	}

	if auth.Source == "codex-file" || auth.BaseURL != "" {
		// Codex backend or custom base URL — bridge WS↔HTTP SSE
		h.handleWSToHTTP(w, r, auth, resolvedRoute, start)
	} else {
		// Direct OpenAI API — proxy WS↔WS
		h.handleWSToWS(w, r, auth, resolvedRoute, start)
	}
}

// handleWSToHTTP bridges a client WebSocket connection to an HTTP SSE upstream.
// Client sends WS message (response.create) → proxy sends HTTP POST with stream=true →
// proxy reads SSE events → sends each event as a WS text message back to client.
func (h *WebSocketResponsesHandler) handleWSToHTTP(w http.ResponseWriter, r *http.Request, auth *ProviderAuth, resolvedRoute string, start time.Time) {
	clientConn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorf("[WS-HTTP-BRIDGE] upgrade failed: %v", err)
		return
	}
	defer clientConn.Close()

	routeLabel := resolvedRoute + "/" + auth.Source
	log.Infof("[WS-HTTP-BRIDGE] client connected (%s), base=%s", routeLabel, auth.BaseURL)

	var model string

	// Read messages from the client; each one triggers an HTTP POST.
	for {
		msgType, msg, readErr := clientConn.ReadMessage()
		if readErr != nil {
			if !websocket.IsCloseError(readErr, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Debugf("[WS-HTTP-BRIDGE] client read error: %v", readErr)
			}
			break
		}
		if msgType != websocket.TextMessage {
			continue
		}

		wsType := gjson.GetBytes(msg, "type").String()
		log.Infof("[WS-HTTP-BRIDGE] recv WS type=%s len=%d", wsType, len(msg))
		if wsType != "response.create" {
			log.Infof("[WS-HTTP-BRIDGE] ignoring WS message type=%s body=%.200s", wsType, string(msg))
			continue
		}

		// Extract the response body from the WS envelope.
		responseObj := gjson.GetBytes(msg, "response").Raw
		if responseObj == "" {
			log.Warnf("[WS-HTTP-BRIDGE] no 'response' field in message")
			continue
		}

		body := []byte(responseObj)
		// Ensure stream is enabled
		body, _ = sjson.SetBytes(body, "stream", true)

		m := gjson.GetBytes(body, "model").String()
		if m != "" {
			model = m
		}

		// Apply model redirect if configured
		if target, redirected := h.cfg.ResolveModelRedirect(model); redirected {
			log.Infof("[WS-HTTP-BRIDGE] redirect %s -> %s", model, target)
			model = target
			body, _ = sjson.SetBytes(body, "model", model)
		}

		log.Infof("[WS-HTTP-BRIDGE] response.create model=%s", model)

		// Re-resolve auth for the actual model
		actualAuth, actualRoute := h.authResolver.Resolve(r.Context(), "openai", model)
		if actualRoute == RouteAmp || actualAuth == nil || !actualAuth.Valid() {
			log.Warnf("[WS-HTTP-BRIDGE] no auth for model=%s, skipping", model)
			continue
		}

		if actualAuth.Source == "codex-file" {
			// Codex backend: use "priority" (not "fast"), strip unsupported params
			body, _ = sjson.SetBytes(body, "service_tier", "priority")
			body, _ = sjson.SetBytes(body, "store", false)
			body, _ = sjson.DeleteBytes(body, "previous_response_id")
			body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
			body, _ = sjson.DeleteBytes(body, "safety_identifier")
			body, _ = sjson.DeleteBytes(body, "max_output_tokens")
			body, _ = sjson.DeleteBytes(body, "stream_options")
			if !gjson.GetBytes(body, "instructions").Exists() {
				body, _ = sjson.SetBytes(body, "instructions", "")
			}
		} else {
			if !gjson.GetBytes(body, "service_tier").Exists() {
				body, _ = sjson.SetBytes(body, "service_tier", "fast")
			}
		}

		// Send HTTP POST and stream SSE events back as WS messages
		if bridgeErr := h.bridgeHTTPToWS(r.Context(), clientConn, body, actualAuth, model, routeLabel); bridgeErr != nil {
			log.Errorf("[WS-HTTP-BRIDGE] bridge error: %v", bridgeErr)
			break
		}
	}

	if model == "" {
		model = "openai-ws-bridge"
	}
	h.logger.LogRequest(model, "openai", routeLabel, "/v1/responses", start)
	log.Infof("[WS-HTTP-BRIDGE] disconnected: model=%s duration=%s", model, time.Since(start).Round(time.Millisecond))
}

// bridgeHTTPToWS sends an HTTP POST to the upstream and converts SSE events to WS messages.
func (h *WebSocketResponsesHandler) bridgeHTTPToWS(ctx context.Context, clientConn *websocket.Conn, body []byte, auth *ProviderAuth, model, routeLabel string) error {
	var upstreamURL string
	isCodex := auth.Source == "codex-file"
	if isCodex {
		upstreamURL = CodexBaseURL + "/responses"
	} else {
		upstreamURL = BuildOpenAIResponsesURL(auth.BaseURL)
	}

	resp, err := h.retryer.Do(ctx, h.client, func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(body))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Authorization", "Bearer "+auth.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Connection", "keep-alive")
		if isCodex {
			req.Header.Set("Originator", CodexOriginator)
			req.Header.Set("Version", CodexClientVersion)
			req.Header.Set("User-Agent", CodexUserAgent)
			if auth.Email != "" {
				req.Header.Set("Chatgpt-Account-Id", auth.Email)
			}
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		// Send error as a WS message so the client sees it
		errMsg := string(respBody)
		log.Errorf("[WS-HTTP-BRIDGE] HTTP %d: %s", resp.StatusCode, errMsg)
		_ = clientConn.WriteMessage(websocket.TextMessage, respBody)
		return nil
	}

	// Parse SSE stream: each event is "event: <type>\ndata: <json>\n\n"
	// Convert each to a WS text message containing just the data JSON.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(nil, 10*1024*1024)

	var lastUsageMsg []byte
	var outputItems [][]byte

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			dataJSON := line[len("data: "):]

			// The WS protocol sends raw event JSON (with "type" field already in data).
			wsMsg := []byte(dataJSON)
			eventType := gjson.GetBytes(wsMsg, "type").String()

			// Collect completed output items
			if eventType == "response.output_item.done" {
				item := gjson.GetBytes(wsMsg, "item").Raw
				if item != "" {
					outputItems = append(outputItems, []byte(item))
				}
			}

			// Patch response.completed if output is empty (Codex backend returns empty output[])
			if eventType == "response.completed" && len(outputItems) > 0 {
				outputArr := gjson.GetBytes(wsMsg, "response.output")
				if outputArr.Exists() && outputArr.IsArray() && len(outputArr.Array()) == 0 {
					var buf bytes.Buffer
					buf.WriteByte('[')
					for i, item := range outputItems {
						if i > 0 {
							buf.WriteByte(',')
						}
						buf.Write(item)
					}
					buf.WriteByte(']')
					if patched, patchErr := sjson.SetRawBytes(wsMsg, "response.output", buf.Bytes()); patchErr == nil {
						wsMsg = patched
						log.Infof("[WS-HTTP-BRIDGE] patched response.completed with %d output items", len(outputItems))
					}
				}
				lastUsageMsg = make([]byte, len(wsMsg))
				copy(lastUsageMsg, wsMsg)
			}

			if err := clientConn.WriteMessage(websocket.TextMessage, wsMsg); err != nil {
				return err
			}

			continue
		}

		// Empty line = event boundary, ignore
	}

	if scanErr := scanner.Err(); scanErr != nil {
		log.Warnf("[WS-HTTP-BRIDGE] SSE scan error: %v", scanErr)
	}

	// Record usage
	if lastUsageMsg != nil {
		usage := ParseOpenAIUsage([]byte(gjson.GetBytes(lastUsageMsg, "response.usage").Raw))
		h.logger.RecordResult(model, 200, usage, 0, "", "", "")
	}

	return nil
}

// handleWSToWS proxies WebSocket connections directly to an upstream WS endpoint
// (for direct OpenAI API access without custom base URL).
func (h *WebSocketResponsesHandler) handleWSToWS(w http.ResponseWriter, r *http.Request, auth *ProviderAuth, resolvedRoute string, start time.Time) {
	clientConn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorf("[WS-RESPONSES] upgrade failed: %v", err)
		return
	}
	defer clientConn.Close()

	upstreamURL := h.buildUpstreamURL(auth)

	reqHeader := http.Header{}
	reqHeader.Set("Authorization", "Bearer "+auth.Token)
	for _, hdr := range []string{"OpenAI-Beta", "User-Agent"} {
		if v := r.Header.Get(hdr); v != "" {
			reqHeader.Set(hdr, v)
		}
	}

	upstreamConn, _, err := websocket.DefaultDialer.Dial(upstreamURL, reqHeader)
	if err != nil {
		log.Errorf("[WS-RESPONSES] upstream dial %s failed: %v", upstreamURL, err)
		clientConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream connection failed"))
		return
	}
	defer upstreamConn.Close()

	routeLabel := resolvedRoute + "/" + auth.Source
	log.Infof("[WS-RESPONSES] connected: client <-> %s (%s)", upstreamURL, routeLabel)

	var model string
	var modelOnce sync.Once

	var closeOnce sync.Once
	done := make(chan struct{})
	closeDone := func() { closeOnce.Do(func() { close(done) }) }

	// Client → Upstream
	go func() {
		defer closeDone()
		for {
			msgType, msg, readErr := clientConn.ReadMessage()
			if readErr != nil {
				if !websocket.IsCloseError(readErr, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					log.Debugf("[WS-RESPONSES] client read error: %v", readErr)
				}
				return
			}
			if msgType == websocket.TextMessage {
				modelOnce.Do(func() {
					if m := gjson.GetBytes(msg, "response.model").String(); m != "" {
						model = m
					}
				})
				// Apply model redirect and inject fast mode for response.create messages
				if gjson.GetBytes(msg, "type").String() == "response.create" {
					if m := gjson.GetBytes(msg, "response.model").String(); m != "" {
						if target, redirected := h.cfg.ResolveModelRedirect(m); redirected {
							log.Infof("[WS-RESPONSES] redirect %s -> %s", m, target)
							msg, _ = sjson.SetBytes(msg, "response.model", target)
						}
					}
					if !gjson.GetBytes(msg, "response.service_tier").Exists() {
						msg, _ = sjson.SetBytes(msg, "response.service_tier", "fast")
					}
				}
			}
			if writeErr := upstreamConn.WriteMessage(msgType, msg); writeErr != nil {
				log.Debugf("[WS-RESPONSES] upstream write error: %v", writeErr)
				return
			}
		}
	}()

	// Upstream → Client
	go func() {
		defer closeDone()
		for {
			msgType, msg, readErr := upstreamConn.ReadMessage()
			if readErr != nil {
				if !websocket.IsCloseError(readErr, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					log.Debugf("[WS-RESPONSES] upstream read error: %v", readErr)
				}
				return
			}
			if msgType == websocket.TextMessage {
				h.maybeRecordUsage(msg, &model, routeLabel, start)
			}
			if writeErr := clientConn.WriteMessage(msgType, msg); writeErr != nil {
				log.Debugf("[WS-RESPONSES] client write error: %v", writeErr)
				return
			}
		}
	}()

	<-done

	deadline := time.Now().Add(2 * time.Second)
	clientConn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
	upstreamConn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)

	if model == "" {
		model = "openai-ws"
	}
	h.logger.LogRequest(model, "openai", routeLabel, "/v1/responses", start)
	log.Infof("[WS-RESPONSES] disconnected: model=%s duration=%s", model, time.Since(start).Round(time.Millisecond))
}

// buildUpstreamURL constructs the wss:// URL for the upstream Responses API.
func (h *WebSocketResponsesHandler) buildUpstreamURL(auth *ProviderAuth) string {
	base := auth.BaseURL
	if base == "" {
		base = h.cfg.OpenAI.BaseURL
	}
	return httpToWS(ResolveOpenAIBaseURL(base)) + "/v1/responses"
}

// maybeRecordUsage checks for a response.completed event and records token usage.
func (h *WebSocketResponsesHandler) maybeRecordUsage(msg []byte, model *string, routeLabel string, start time.Time) {
	eventType := gjson.GetBytes(msg, "type").String()
	if eventType != "response.completed" {
		return
	}

	if *model == "" {
		if m := gjson.GetBytes(msg, "response.model").String(); m != "" {
			*model = m
		}
	}

	usage := ParseOpenAIUsage([]byte(gjson.GetBytes(msg, "response.usage").Raw))
	m := *model
	if m == "" {
		m = "openai-ws"
	}
	h.logger.RecordResult(m, 200, usage, 0, "", "", "")
}

// httpToWS converts an http(s) URL to ws(s).
func httpToWS(rawURL string) string {
	parsed, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil {
		rawURL = strings.TrimRight(rawURL, "/")
		rawURL = strings.Replace(rawURL, "https://", "wss://", 1)
		rawURL = strings.Replace(rawURL, "http://", "ws://", 1)
		return rawURL
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "ws", "wss":
		// already websocket scheme
	default:
		parsed.Scheme = "wss"
	}
	return parsed.String()
}

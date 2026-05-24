package rivet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// ampMessageIDAlphabet is the character set observed in amp's M-XXXXXXX
// messageIds. Length 22 (22*log2(58) ≈ 128 bits of entropy).
const ampMessageIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func newAmpMessageID() string {
	return "M-" + newAmpMessageIDRaw()
}

func newAmpMessageIDRaw() string {
	b := make([]byte, 22)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = ampMessageIDAlphabet[int(b[i])%len(ampMessageIDAlphabet)]
	}
	return string(b)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// thinkingBudgetForEffort maps amp's reasoning.effort string to an Anthropic
// extended-thinking token budget. Returns 0 to leave thinking disabled.
func thinkingBudgetForEffort(eff string) int {
	switch strings.ToLower(eff) {
	case "low":
		return 0
	case "medium":
		return 4096
	case "high":
		return 12288
	case "xhigh":
		return 32768
	default:
		return 0
	}
}

// openaiReasoningEffort maps amp's reasoning.effort to OpenAI's accepted
// values for the Responses API. xhigh is collapsed to high (OpenAI's max).
func openaiReasoningEffort(eff string) string {
	switch strings.ToLower(eff) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh":
		return "high"
	default:
		return ""
	}
}

// uuidLike returns a UUID-v4-shaped string without importing google/uuid here.
func uuidLike() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	dst := 0
	for i, x := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[dst] = '-'
			dst++
		}
		out[dst] = hex[x>>4]
		out[dst+1] = hex[x&0x0f]
		dst += 2
	}
	return string(out)
}

// shouldRouteLocal reports whether this agentMode should be served by our
// local OAuth inference path. Modes routed to Claude get local OAuth
// (Pro/Max subscription savings). OpenAI/Gemini-backed modes pass through
// to amp's server so the user's BYOK config there handles billing.
//
// Override via AMP_PROXY_LOCAL_MODES env var (comma list, e.g.
// "smart,large,rush" to also intercept rush locally).
func shouldRouteLocal(mode string) bool {
	if override := strings.TrimSpace(os.Getenv("AMP_PROXY_LOCAL_MODES")); override != "" {
		for _, m := range strings.Split(override, ",") {
			if strings.EqualFold(strings.TrimSpace(m), mode) {
				return true
			}
		}
		return false
	}
	switch strings.ToLower(mode) {
	case "smart", "large", "agg", "agg-man", "":
		return true // Claude-backed → local OAuth
	default:
		return false // rush/deep/frontier/nostromo → amp BYOK
	}
}

// modelOverrideForRole reads ~/.config/amp/settings.json `internal.model`
// to let users force a specific model for a given role. Format:
//
//	"internal.model": {
//	  "smart": "anthropic/claude-opus-4-7",
//	  "rush":  "openai/gpt-5.4",
//	  "oracle": "openai/gpt-5.5"
//	}
//
// Returns ("","") if not configured for this mode.
func modelOverrideForRole(mode string) (provider, model string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	data, err := os.ReadFile(home + "/.config/amp/settings.json")
	if err != nil {
		return "", ""
	}
	val := gjson.GetBytes(data, "internal\\.model."+mode).String()
	if val == "" {
		return "", ""
	}
	parts := strings.SplitN(val, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// modelForAgentMode picks the (provider, model) for an Amp Neo agent mode.
// Mapping extracted from amp binary's Wm dict (Wm0=Smart, Wm0.RUSH, etc.):
//
//	smart    → anthropic claude-opus-4-7         (high reasoning)
//	rush     → openai    gpt-5.5                 (none reasoning, fast/cheap)
//	large    → anthropic claude-opus-4-6         (1M context variant)
//	deep     → openai    gpt-5.5                 (medium reasoning)
//	frontier → google    gemini-3.5-flash        (medium reasoning, vision)
//	agg      → anthropic claude-opus-4-6         (server-only, hidden mode)
//	nostromo → openai    amp-nostromo            (internal test mode — best-effort fallback)
//
// Unknown modes fall back to smart/opus 4.7 so we never block on dispatch.
func modelForAgentMode(mode string) (provider, model string) {
	// User-configured override wins.
	if p, m := modelOverrideForRole(mode); p != "" {
		return p, m
	}
	switch strings.ToLower(mode) {
	case "rush":
		return "openai", "gpt-5.5"
	case "large":
		return "anthropic", "claude-opus-4-6"
	case "deep":
		return "openai", "gpt-5.5"
	case "frontier":
		// Amp's frontier = Vertex AI Gemini 3.5 Flash. Vertex needs
		// project-bound Google Cloud creds which most users don't have.
		// Try Gemini provider (Google AI Studio) first; if no creds the
		// auth resolver falls through to amp. As a usable fallback, the
		// runtime route logic will substitute opus-4-7 when google has no
		// auth — controlled by resolveRoute returning error.
		return "google", "gemini-2.5-flash"
	case "agg", "agg-man":
		return "anthropic", "claude-opus-4-6"
	case "nostromo":
		// Internal test mode — proxy doesn't have AMP_NOSTROMO; fallback to gpt-5.5.
		return "openai", "gpt-5.5"
	case "smart", "":
		return "anthropic", "claude-opus-4-7"
	default:
		return "anthropic", "claude-opus-4-7"
	}
}

// threadStore is process-global per-thread state so that multi-turn
// conversations across separate amp invocations (each opens its own WS)
// can share message history. Keyed by threadID.
var (
	threadStoreMu sync.Mutex
	threadStore   = map[string]*threadState{}
)

type threadState struct {
	mu      sync.Mutex
	history []anthropicMessage
	nextSeq int
}

func loadOrCreateThread(id string) *threadState {
	threadStoreMu.Lock()
	defer threadStoreMu.Unlock()
	if ts, ok := threadStore[id]; ok {
		return ts
	}
	ts := &threadState{nextSeq: 1}
	threadStore[id] = ts
	return ts
}

// RivetSession tracks state for one client↔proxy WebSocket connection so the
// inference orchestrator can rebuild what the server-side actor would have:
// the thread id, the rolling chat history, and the latest tool/guidance
// snapshots that act as the system prompt scaffolding.
type RivetSession struct {
	connID          uint64
	threadID        string
	agentMode       string // smart | rush | large | deep — chosen by amp UI, drives model
	reasoningEffort string // low | medium | high | xhigh, from client_update_thread_settings

	logger *RequestLogger // logs each upstream LLM call (Anthropic/OpenAI/Gemini) for stats
	cfg    *Config
	auth   *AuthResolver

	// currentRoute is set at the start of runLocalInference to the resolved
	// auth route (RouteLocal / RouteAPIKey). Used by round handlers for
	// stats labeling. Not mutex-protected: only mutated by the single
	// runLocalInference goroutine, read by the round it spawned.
	currentRoute string

	mu               sync.Mutex
	history          []anthropicMessage // {role, content[]} for next Anthropic call
	toolsRaw         []byte             // last executor_tools_register payload (full frame)
	anthroTools      []map[string]any   // Anthropic-shaped tools (cached translation)
	guidance         string             // last executor_guidance_snapshot text
	envText          string             // last executor_environment_snapshot summary
	skillsText       string             // markdown list of installed skill name+desc, for system prompt
	threadTitle      string             // cached after first generation; empty means not yet titled
	pendingApprovals []string           // toolCallIds that need approval bridge from observeClient
	pendingDenials   []string           // toolCallIds we should reject
	pendingErrorSet  string             // error_set message to emit to client
	artifactCount    int                // running tally of executor artifacts (for diagnostics)
	inflight         bool               // true while runLocalInference is executing
	lastMsgID        string             // dedup repeat client_append_user_msg
	nextSeq          int                // monotonically increasing per-thread message sequence number used in message_added frames

	// pluginHooks tracks plugin_message request IDs we injected so that the
	// corresponding executor_plugin_message replies from amp's local plugin
	// system can be captured and not leaked upstream.
	pluginHooks map[string]chan []byte

	// pendingToolResults — toolCallId → channel that pipeClient signals when
	// amp's executor returns executor_tool_result for that call.
	pendingToolResults map[string]chan []byte

	// toolsBootstrapReady is a one-shot channel closed when amp finishes
	// sending executor_tools_register + executor_tools_bootstrap_complete.
	toolsBootstrapReady chan struct{}
	toolsBootstrapDone  bool

	// hydrated is set after the first attempt to fetch thread history from
	// the Amp server, so we only call getThread once per session.
	hydrated bool

	// remoteDriven=true when this WS session was opened by an amp subprocess
	// spawned by RemoteAgent (the WebUI flow). For those sessions we ALWAYS
	// route inference locally, regardless of agentMode — browser users don't
	// have amp-side BYOK to fall back to.
	remoteDriven bool

	// inflightCancel cancels the context of the currently-running
	// runLocalInference call. Set when inference starts, cleared when it
	// returns. Used by client_cancel to abort the upstream LLM HTTP
	// request and any pending tool waits.
	inflightCancel context.CancelFunc

	// persistUnsafe disables uploadThread for this session when hydration
	// failed (e.g. network) — uploading partial history would clobber the
	// real server-side thread.
	persistUnsafe bool

	// serverPriorLen records the message count the server had BEFORE this
	// session's first inference. Used as a sanity floor: we never upload
	// fewer messages than what the server already had.
	serverPriorLen int

	// serverV records the thread version observed from the server during
	// hydration. uploadThread must send v >= serverV+1 or the server
	// silently rejects the message diff (keeps the v bump but drops the
	// new content).
	serverV int

	// serverMeta and serverCreatorUserID are carried verbatim from the
	// hydrated thread so that uploadThread preserves required fields the
	// server validates against (uploads without them get the messages
	// silently discarded).
	serverMeta          json.RawMessage
	serverCreatorUserID string
	serverEnv           json.RawMessage
	serverCreated       int64
}

func (s *RivetSession) takeSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSeq++
	return s.nextSeq
}

// routeLabelFor maps an internal route constant to the value used by
// the admin dashboard ("local" / "apikey" / "amp"). Defaults to "local"
// for the Rivet path since we never proxy upstream when running local
// inference.
func routeLabelFor(route string) string {
	switch route {
	case RouteAPIKey:
		return RouteAPIKey
	case RouteAmp:
		return RouteAmp
	default:
		return RouteLocal
	}
}

// anthropicMessage is the on-the-wire shape sent to api.anthropic.com.
type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

// anthropicContent supports text, thinking, tool_use, tool_result, and image
// block shapes. Serialized selectively via MarshalJSON for Anthropic's wire format.
type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// tool_use:
	ToolUseID string `json:"-"`
	ToolName  string `json:"-"`
	ToolInput any    `json:"-"`

	// tool_result:
	ToolContent string `json:"-"`

	// thinking — required to be replayed unchanged in next turn for
	// extended-thinking with tool use.
	ThinkingText      string `json:"-"`
	ThinkingSignature string `json:"-"`

	// image (Anthropic format: {type: "image", source: {type: "base64", media_type, data}}):
	ImageSourceType string `json:"-"`
	ImageMediaType  string `json:"-"`
	ImageData       string `json:"-"`
	ImageURL        string `json:"-"`
}

func (c anthropicContent) MarshalJSON() ([]byte, error) {
	switch c.Type {
	case "tool_use":
		m := map[string]any{"type": "tool_use", "id": c.ToolUseID, "name": c.ToolName, "input": c.ToolInput}
		return json.Marshal(m)
	case "tool_result":
		m := map[string]any{"type": "tool_result", "tool_use_id": c.ToolUseID, "content": c.ToolContent}
		return json.Marshal(m)
	case "thinking":
		m := map[string]any{"type": "thinking", "thinking": c.ThinkingText, "signature": c.ThinkingSignature}
		return json.Marshal(m)
	case "image":
		source := map[string]any{"type": c.ImageSourceType, "media_type": c.ImageMediaType, "data": c.ImageData}
		if c.ImageSourceType == "url" && c.ImageURL != "" {
			source = map[string]any{"type": "url", "url": c.ImageURL}
		}
		return json.Marshal(map[string]any{"type": "image", "source": source})
	default:
		return json.Marshal(map[string]any{"type": c.Type, "text": c.Text})
	}
}

func newRivetSession(connID uint64, logger *RequestLogger) *RivetSession {
	return &RivetSession{
		connID:             connID,
		logger:             logger,
		pluginHooks:        make(map[string]chan []byte),
		pendingToolResults: make(map[string]chan []byte),
		// Server's first message_added (user) has seq=2 in observed traces;
		// pre-seed so takeSeq returns 2 first.
		nextSeq: 1,
	}
}

// bindThread connects this session to the persistent threadState for its
// threadID, hydrating history and seq counter from prior connections (if any).
// Called once threadID is known (after subprotocols are parsed).
func (s *RivetSession) bindThread() {
	if s.threadID == "" {
		return
	}
	ts := loadOrCreateThread(s.threadID)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	s.history = append([]anthropicMessage(nil), ts.history...)
	if ts.nextSeq > s.nextSeq {
		s.nextSeq = ts.nextSeq
	}
}

// persistThread writes the in-memory session history back to threadStore so
// the next connection for this threadID can resume.
func (s *RivetSession) persistThread() {
	if s.threadID == "" {
		return
	}
	ts := loadOrCreateThread(s.threadID)
	s.mu.Lock()
	history := append([]anthropicMessage(nil), s.history...)
	seq := s.nextSeq
	s.mu.Unlock()
	ts.mu.Lock()
	ts.history = history
	ts.nextSeq = seq
	ts.mu.Unlock()
}

// registerPluginHook reserves a reply slot for an outgoing plugin_message
// request id and returns the channel that pipeClient will signal when amp's
// executor sends executor_plugin_message with that id.
func (s *RivetSession) registerPluginHook(id string) chan []byte {
	ch := make(chan []byte, 1)
	s.mu.Lock()
	s.pluginHooks[id] = ch
	s.mu.Unlock()
	return ch
}

func (s *RivetSession) unregisterPluginHook(id string) {
	s.mu.Lock()
	delete(s.pluginHooks, id)
	s.mu.Unlock()
}

// tryDeliverPluginReply checks if an executor_plugin_message reply matches a
// pending hook we injected. If so, the reply is delivered to the waiter and
// the caller should suppress the frame (not forward upstream).
func (s *RivetSession) tryDeliverPluginReply(data []byte) bool {
	if gjson.GetBytes(data, "type").String() != "executor_plugin_message" {
		return false
	}
	if gjson.GetBytes(data, "message.type").String() != "response" {
		return false
	}
	id := gjson.GetBytes(data, "message.id").String()
	if id == "" {
		return false
	}
	s.mu.Lock()
	ch, ok := s.pluginHooks[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- data:
	default:
	}
	return true
}

// observeClient inspects each client->server frame for context we need to do
// inference, and returns true if the frame should be SUPPRESSED (not forwarded
// to the Rivet upstream). The forwarder calls handleInference asynchronously
// when a frame is suppressed. shouldRun is true when caller should kick off
// local inference (false for dup retries while one is already in flight).
func (s *RivetSession) observeClient(data []byte) (suppress, shouldRun bool, kind, userText, userMsgID string) {
	msgType := gjson.GetBytes(data, "type").String()
	switch msgType {
	case "client_append_user_msg":
		// Provider routing decision: if this agentMode targets a model we
		// want amp's server to handle (e.g. user has BYOK for OpenAI/Gemini
		// configured server-side and prefers amp's BYOK billing over our
		// own local creds), do NOT suppress — let the frame go upstream
		// and amp server runs inference. Default: Claude modes (smart/
		// large/agg) → local OAuth (saves Pro/Max subscription); others →
		// upstream BYOK.
		mode := strings.ToLower(gjson.GetBytes(data, "agentMode").String())
		if mode == "" {
			mode = s.agentMode
		}
		// Frame's agentMode wins over JWT-bound mode (frame reflects user's
		// most recent picker selection). Update so dispatch matches.
		if mode != "" && mode != s.agentMode {
			s.mu.Lock()
			s.agentMode = mode
			s.mu.Unlock()
		}
		if !shouldRouteLocal(mode) && !s.remoteDriven {
			log.Infof("[RIVET %d] mode=%s → passthrough to amp (BYOK)", s.connID, mode)
			return false, false, msgType, "", ""
		}
		if s.remoteDriven && !shouldRouteLocal(mode) {
			log.Infof("[RIVET %d] mode=%s remote-driven → forcing local route", s.connID, mode)
		}
		msgID := gjson.GetBytes(data, "messageId").String()
		text := joinTextBlocks(gjson.GetBytes(data, "content"))
		content := buildUserContent(gjson.GetBytes(data, "content"))
		s.mu.Lock()
		dup := msgID != "" && msgID == s.lastMsgID
		if !dup {
			s.history = append(s.history, anthropicMessage{
				Role:    "user",
				Content: content,
			})
			s.lastMsgID = msgID
		}
		canRun := !s.inflight && !dup
		if canRun {
			s.inflight = true
		}
		s.mu.Unlock()
		return true, canRun, msgType, text, msgID
	case "executor_connect":
		// First frame from amp. Check the pid against the RemoteAgent
		// registry to tell whether this amp instance was spawned by us
		// for a Browser-driven conversation. If yes, override the default
		// passthrough routing so rush/deep/frontier go through our local
		// inference (Browser has no amp BYOK to fall back to).
		pid := int(gjson.GetBytes(data, "capabilities.pid").Int())
		if isRemotePID(pid) {
			s.mu.Lock()
			s.remoteDriven = true
			s.mu.Unlock()
			log.Infof("[RIVET %d] session is remote-driven (browser, pid=%d) → forcing local routing", s.connID, pid)
		}
	case "executor_guidance_snapshot":
		// Concatenate guidance file contents into a single text block.
		var b strings.Builder
		gjson.GetBytes(data, "files").ForEach(func(_, file gjson.Result) bool {
			if c := file.Get("content").String(); c != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n---\n\n")
				}
				b.WriteString(c)
			}
			return true
		})
		s.mu.Lock()
		s.guidance = b.String()
		s.mu.Unlock()
	case "executor_environment_snapshot":
		env := gjson.GetBytes(data, "environment")
		s.mu.Lock()
		s.envText = env.String()
		s.mu.Unlock()
	case "executor_tools_register":
		// Cache the raw tools payload and pre-translate to Anthropic shape.
		s.mu.Lock()
		s.toolsRaw = append(s.toolsRaw[:0], data...)
		s.anthroTools = translateToolsToAnthropic(data)
		s.mu.Unlock()
	case "client_update_thread_settings":
		// Capture reasoning.effort so we can forward to LLM (Anthropic
		// thinking budget / OpenAI reasoning effort).
		eff := gjson.GetBytes(data, "settings.reasoning\\.effort").String()
		if eff != "" {
			s.mu.Lock()
			s.reasoningEffort = eff
			s.mu.Unlock()
		}
	case "executor_tools_bootstrap_complete":
		// Signal any queued inference to start now that tools are registered.
		s.mu.Lock()
		ready := s.toolsBootstrapReady
		s.toolsBootstrapReady = nil
		s.toolsBootstrapDone = true
		s.mu.Unlock()
		if ready != nil {
			close(ready)
		}
	case "client_retry":
		// Re-run the last assistant turn: drop the last assistant message
		// (and any tool_result user messages immediately after it) from
		// history, then trigger inference again. The last user message
		// (before the dropped assistant turn) stays as the new prompt.
		s.mu.Lock()
		// Walk back from end, trim until we hit the last user message
		// that is *not* a tool_result turn.
		for len(s.history) > 0 {
			last := s.history[len(s.history)-1]
			isUserText := last.Role == "user" && len(last.Content) > 0 && last.Content[0].Type == "text"
			if isUserText {
				break
			}
			s.history = s.history[:len(s.history)-1]
		}
		dup := false
		if len(s.history) == 0 {
			s.mu.Unlock()
			return true, false, msgType, "", ""
		}
		userMsg := s.history[len(s.history)-1]
		_ = dup
		text := ""
		if len(userMsg.Content) > 0 {
			text = userMsg.Content[0].Text
		}
		canRun := !s.inflight
		if canRun {
			s.inflight = true
		}
		s.mu.Unlock()
		log.Infof("[RIVET] client_retry → re-running with text=%q", truncateStr(text, 80))
		return true, canRun, "client_retry", text, s.lastMsgID
	case "client_edit_message":
		// Edit a previous user message. The frame carries messageId + new
		// content. Find the message in history, replace content, drop
		// everything after it, then re-run inference.
		editID := gjson.GetBytes(data, "messageId").String()
		newText := joinTextBlocks(gjson.GetBytes(data, "content"))
		if newText == "" || editID == "" {
			return true, false, msgType, "", ""
		}
		// We don't track WS-frame messageId per history entry; treat as
		// "edit the last user message" (the most common UI gesture).
		s.mu.Lock()
		for len(s.history) > 0 {
			last := s.history[len(s.history)-1]
			if last.Role == "user" && len(last.Content) > 0 && last.Content[0].Type == "text" {
				break
			}
			s.history = s.history[:len(s.history)-1]
		}
		if len(s.history) > 0 {
			s.history[len(s.history)-1] = anthropicMessage{
				Role:    "user",
				Content: []anthropicContent{{Type: "text", Text: newText}},
			}
		}
		canRun := !s.inflight
		if canRun {
			s.inflight = true
		}
		s.lastMsgID = editID
		s.mu.Unlock()
		log.Infof("[RIVET] client_edit_message → re-running with edited text")
		return true, canRun, "client_edit_message", newText, editID
	case "client_cancel":
		// Abort the in-flight inference (cancels upstream HTTP request and
		// any pending tool waits). The frame is also suppressed from
		// going upstream — we don't have a real server-side inference
		// running anyway.
		s.mu.Lock()
		cancel := s.inflightCancel
		s.inflightCancel = nil
		s.inflight = false
		s.mu.Unlock()
		if cancel != nil {
			log.Infof("[RIVET] client_cancel → aborting local inference")
			cancel()
		}
		return true, false, msgType, "", ""
	case "executor_tools_unregister":
		// Tools removed mid-session — drop them from cache so next inference
		// doesn't advertise tools that no longer exist.
		names := map[string]bool{}
		gjson.GetBytes(data, "toolNames").ForEach(func(_, v gjson.Result) bool {
			names[v.String()] = true
			return true
		})
		if len(names) > 0 {
			s.mu.Lock()
			filtered := s.anthroTools[:0]
			for _, t := range s.anthroTools {
				if n, _ := t["name"].(string); !names[n] {
					filtered = append(filtered, t)
				}
			}
			s.anthroTools = filtered
			s.mu.Unlock()
		}
	case "executor_environment_update":
		// Env can change mid-session (cwd, files). Refresh cache.
		env := gjson.GetBytes(data, "environment")
		s.mu.Lock()
		s.envText = env.String()
		s.mu.Unlock()
	case "executor_tool_approval_request":
		// Decide via permissions config (~/.config/amp/settings.json,
		// `amp.permissions` array). action ∈ {allow, ask, deny}:
		//   allow → push approval (auto-yes)
		//   deny  → push rejection
		//   ask   → don't auto-respond; let amp's normal flow show prompt
		toolCallID := gjson.GetBytes(data, "toolCallId").String()
		toolName := gjson.GetBytes(data, "toolName").String()
		var argsMap map[string]any
		if a := gjson.GetBytes(data, "args"); a.Exists() {
			_ = json.Unmarshal([]byte(a.Raw), &argsMap)
		}
		if toolCallID == "" {
			break
		}
		action := LoadPermissions().Decide(toolName, argsMap)
		switch action {
		case "allow":
			s.mu.Lock()
			s.pendingApprovals = append(s.pendingApprovals, toolCallID)
			s.mu.Unlock()
			log.Infof("[PERMISSIONS] auto-allow %s %s", toolName, toolCallID)
		case "deny":
			s.mu.Lock()
			s.pendingDenials = append(s.pendingDenials, toolCallID)
			s.mu.Unlock()
			log.Infof("[PERMISSIONS] auto-deny %s %s", toolName, toolCallID)
		default: // "ask"
			log.Infof("[PERMISSIONS] ask %s %s — leaving for user", toolName, toolCallID)
		}
	case "queued_message_added", "queued_message_removed", "queued_message_dequeued", "queued_messages":
		// Pass-through frames for amp's message queue. We don't proactively
		// process queued messages — user sends them via client_append_user_msg
		// which we already handle. Just track count for visibility.
		// (Future: process queue ourselves when inflight=false.)
	case "executor_status":
		// executor reports lifecycle status (e.g. tool deps loading). Log
		// but don't block on it.
		status := gjson.GetBytes(data, "status").String()
		log.Debugf("[RIVET %d] executor_status: %s", s.connID, status)
	case "executor_error":
		// executor encountered an error (e.g. tool crashed). Surface to UI
		// via an error_set frame so user sees feedback.
		msg := gjson.GetBytes(data, "message").String()
		if msg == "" {
			msg = gjson.GetBytes(data, "error").String()
		}
		log.Warnf("[RIVET %d] executor_error: %s", s.connID, msg)
		s.mu.Lock()
		s.pendingErrorSet = msg
		s.mu.Unlock()
	case "executor_artifact_upsert":
		// Capture artifact for later persistence. We don't render it but
		// uploadThread should carry it forward (TODO: artifacts in thread).
		s.mu.Lock()
		s.artifactCount++
		s.mu.Unlock()
		log.Debugf("[RIVET %d] artifact upsert", s.connID)
	case "executor_artifact_delete":
		s.mu.Lock()
		if s.artifactCount > 0 {
			s.artifactCount--
		}
		s.mu.Unlock()
	case "client_filesystem_read_directory", "client_filesystem_read_file":
		// Server-initiated FS access. In injection mode no server, so reply
		// with executor-side equivalents. amp client's own executor handles
		// these via the existing _result frames it sends; just pass through.
	case "executor_skill_snapshot":
		// Snapshot of installed skills; cache name+description for system prompt.
		var b strings.Builder
		gjson.GetBytes(data, "skills").ForEach(func(_, sk gjson.Result) bool {
			name := sk.Get("name").String()
			desc := sk.Get("description").String()
			if name == "" {
				return true
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("- ")
			b.WriteString(name)
			if desc != "" {
				b.WriteString(": ")
				b.WriteString(desc)
			}
			return true
		})
		s.mu.Lock()
		s.skillsText = b.String()
		s.mu.Unlock()
	}
	return false, false, msgType, "", ""
}

// waitForToolsBootstrap blocks until executor_tools_bootstrap_complete arrives
// from amp or the deadline elapses. Returns true if tools are ready.
func (s *RivetSession) waitForToolsBootstrap(ctx context.Context, timeout time.Duration) bool {
	s.mu.Lock()
	if s.toolsBootstrapDone {
		s.mu.Unlock()
		return true
	}
	ch := s.toolsBootstrapReady
	if ch == nil {
		ch = make(chan struct{})
		s.toolsBootstrapReady = ch
	}
	s.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	case <-ctx.Done():
		return false
	}
}

// translateToolsToAnthropic converts the executor_tools_register payload
// (amp's `{name, description, inputSchema, source, meta}` per tool) into the
// shape Anthropic expects: `{name, description, input_schema}`.
func translateToolsToAnthropic(data []byte) []map[string]any {
	var out []map[string]any
	gjson.GetBytes(data, "tools").ForEach(func(_, t gjson.Result) bool {
		name := t.Get("name").String()
		if name == "" {
			return true
		}
		entry := map[string]any{
			"name":        name,
			"description": t.Get("description").String(),
		}
		if schema := t.Get("inputSchema"); schema.Exists() {
			var schemaObj any
			if err := json.Unmarshal([]byte(schema.Raw), &schemaObj); err == nil {
				entry["input_schema"] = schemaObj
			}
		}
		out = append(out, entry)
		return true
	})
	return out
}

// tryDeliverToolResult intercepts executor_tool_result frames whose
// toolCallId matches a pending tool we issued. Returns true to indicate the
// frame should be suppressed (kept local).
func (s *RivetSession) tryDeliverToolResult(data []byte) bool {
	if gjson.GetBytes(data, "type").String() != "executor_tool_result" {
		return false
	}
	id := gjson.GetBytes(data, "toolCallId").String()
	if id == "" {
		return false
	}
	s.mu.Lock()
	ch, ok := s.pendingToolResults[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- data:
	default:
	}
	return true
}

// registerPendingTool reserves a reply slot for a toolCallId we just sent in
// a tool_lease.
func (s *RivetSession) registerPendingTool(id string) chan []byte {
	ch := make(chan []byte, 1)
	s.mu.Lock()
	s.pendingToolResults[id] = ch
	s.mu.Unlock()
	return ch
}

func (s *RivetSession) unregisterPendingTool(id string) {
	s.mu.Lock()
	delete(s.pendingToolResults, id)
	s.mu.Unlock()
}

func (s *RivetSession) isPendingTool(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	_, ok := s.pendingToolResults[id]
	s.mu.Unlock()
	return ok
}

// compactHistory drops oldest messages (preserving the most recent N) when
// the total byte size exceeds budget. Tries LLM summarization first; falls
// back to truncation-with-marker if summary fails.
//
// Returns the (possibly shorter) history.
func compactHistory(history []anthropicMessage, budgetBytes int, authResolver *AuthResolver, cfg *Config) []anthropicMessage {
	if len(history) <= 10 {
		return history
	}
	if totalHistorySize(history) <= budgetBytes {
		return history
	}
	// Compute how many oldest pairs to remove to fit.
	out := append([]anthropicMessage(nil), history...)
	toSummarize := []anthropicMessage{}
	for len(out) > 10 && totalHistorySize(out) > budgetBytes {
		toSummarize = append(toSummarize, out[0], out[1])
		out = out[2:]
	}
	if len(toSummarize) == 0 {
		return out
	}
	// Try LLM-based summary; degrade gracefully to truncation marker.
	var marker anthropicContent
	if summary := summarizeHistorySafe(toSummarize, authResolver, cfg); summary != "" {
		marker = anthropicContent{
			Type: "text",
			Text: "[Earlier conversation summary]\n" + summary,
		}
	} else {
		marker = anthropicContent{
			Type: "text",
			Text: fmt.Sprintf("[context truncated: %d earlier message(s) omitted for length]", len(toSummarize)),
		}
	}
	return append([]anthropicMessage{{Role: "user", Content: []anthropicContent{marker}}}, out...)
}

func totalHistorySize(h []anthropicMessage) int {
	n := 0
	for _, m := range h {
		for _, c := range m.Content {
			n += len(c.Text) + len(c.ThinkingText) + len(c.ToolContent) + len(c.ToolName) + len(c.ToolUseID)
		}
	}
	return n
}

// summarizeHistorySafe calls Claude haiku to compress old messages into a
// short prose summary. Best-effort: returns "" on any failure (caller falls
// back to truncation marker).
func summarizeHistorySafe(msgs []anthropicMessage, authResolver *AuthResolver, cfg *Config) string {
	if authResolver == nil || cfg == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	auth, route := authResolver.Resolve(ctx, "anthropic", "claude-haiku-4-5-20251001")
	if auth == nil || !auth.Valid() || (route != RouteLocal && route != RouteAPIKey) {
		return ""
	}

	// Render the messages as a transcript for the summarizer.
	var transcript strings.Builder
	for _, m := range msgs {
		transcript.WriteString(strings.ToUpper(m.Role))
		transcript.WriteString(":\n")
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				transcript.WriteString(c.Text)
			case "tool_use":
				transcript.WriteString("[tool ")
				transcript.WriteString(c.ToolName)
				transcript.WriteString(" called]")
			case "tool_result":
				transcript.WriteString("[tool result: ")
				transcript.WriteString(truncateStr(c.ToolContent, 200))
				transcript.WriteString("]")
			}
			transcript.WriteString("\n")
		}
		transcript.WriteString("\n")
	}
	if transcript.Len() > 50000 {
		// Don't pay for huge summary requests; truncate input.
		s := transcript.String()
		transcript.Reset()
		transcript.WriteString(s[:50000])
		transcript.WriteString("\n[input truncated]")
	}

	reqBody := map[string]any{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 1024,
		"system":     "You compress AI coding conversation transcripts. Output a concise summary (<=400 words) preserving: decisions made, key code/files touched, open questions, unresolved errors. Skip pleasantries. Use markdown.",
		"messages":   []map[string]any{{"role": "user", "content": transcript.String()}},
	}
	bodyBytes, _ := json.Marshal(reqBody)
	stableUserID := auth.UserID
	if stableUserID == "" {
		stableUserID = cfg.UserID
	}
	bodyBytes = injectClaudeCodeIdentity(bodyBytes, stableUserID)

	base := "https://api.anthropic.com"
	if auth.BaseURL != "" {
		base = strings.TrimRight(auth.BaseURL, "/")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/messages?beta=true", bytes.NewReader(bodyBytes))
	if err != nil {
		return ""
	}
	if auth.AuthType == AuthXAPIKey {
		req.Header.Set("x-api-key", auth.Token)
	} else {
		req.Header.Set("Authorization", "Bearer "+auth.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20,prompt-caching-2024-07-31")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	respBytes, _ := io.ReadAll(resp.Body)
	out := ""
	gjson.GetBytes(respBytes, "content").ForEach(func(_, c gjson.Result) bool {
		if c.Get("type").String() == "text" {
			out = c.Get("text").String()
			return false
		}
		return true
	})
	log.Infof("[COMPACT] summarized %d msgs → %d chars", len(msgs), len(out))
	return out
}

// takePendingApprovals atomically returns and clears the queued approval IDs.
func (s *RivetSession) takePendingApprovals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingApprovals) == 0 {
		return nil
	}
	out := s.pendingApprovals
	s.pendingApprovals = nil
	return out
}

// takePendingDenials atomically returns and clears tool denial IDs.
func (s *RivetSession) takePendingDenials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingDenials) == 0 {
		return nil
	}
	out := s.pendingDenials
	s.pendingDenials = nil
	return out
}

// takePendingErrorSet atomically returns and clears the queued error_set msg.
func (s *RivetSession) takePendingErrorSet() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pendingErrorSet
	s.pendingErrorSet = ""
	return out
}

// markIdle clears the inflight flag once an inference run finishes (success or
// error) so subsequent user messages can fire.
func (s *RivetSession) markIdle() {
	s.mu.Lock()
	s.inflight = false
	s.mu.Unlock()
}

// joinTextBlocks pulls together the "text" of each item in a content array
// like Anthropic's content blocks. amp's client_append_user_msg uses the same
// shape: [{type:"text",text:"..."}, ...].
func joinTextBlocks(arr gjson.Result) string {
	var b strings.Builder
	arr.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "text" {
			b.WriteString(item.Get("text").String())
		}
		return true
	})
	return b.String()
}

// buildUserContent converts the JSON content array of a client_append_user_msg
// frame into our anthropicContent slice. Supports text + image (Anthropic
// base64 source format). Unknown block types are skipped silently.
func buildUserContent(arr gjson.Result) []anthropicContent {
	out := []anthropicContent{}
	arr.ForEach(func(_, item gjson.Result) bool {
		t := item.Get("type").String()
		switch t {
		case "text":
			txt := item.Get("text").String()
			if txt != "" {
				out = append(out, anthropicContent{Type: "text", Text: txt})
			}
		case "image":
			src := item.Get("source")
			out = append(out, anthropicContent{
				Type:            "image",
				ImageSourceType: src.Get("type").String(),
				ImageMediaType:  src.Get("media_type").String(),
				ImageData:       src.Get("data").String(),
				ImageURL:        src.Get("url").String(),
			})
		}
		return true
	})
	if len(out) == 0 {
		// Defensive: at minimum, emit empty text to prevent zero-content message.
		out = append(out, anthropicContent{Type: "text", Text: ""})
	}
	return out
}

// buildSystemPrompt assembles the system prompt that should accompany the
// local Anthropic call. It mirrors amp's structure (preamble + guidance +
// environment + skills) but does not attempt to be byte-identical with what
// amp's server compiles.
func (s *RivetSession) buildSystemPrompt() string {
	const preamble = `You are Amp, a powerful AI coding agent. You help the user with software engineering tasks. Use the instructions below and the tools available to you to help the user.`

	s.mu.Lock()
	guidance := s.guidance
	env := s.envText
	skills := s.skillsText
	s.mu.Unlock()

	var b strings.Builder
	b.WriteString(preamble)
	if guidance != "" {
		b.WriteString("\n\n# User-provided guidance\n\n")
		b.WriteString(guidance)
	}
	if skills != "" {
		b.WriteString("\n\n# Installed skills\n\nThe user has these named skills available. Invoke a skill by name when the user references it or when it matches the task.\n\n")
		b.WriteString(skills)
	}
	if env != "" {
		b.WriteString("\n\n# Environment\n\n")
		b.WriteString(env)
	}
	return b.String()
}

// runLocalInference is the orchestrator: it builds an Anthropic request from
// the session history, opens a streaming connection using the proxy's local
// Claude credentials, and writes a sequence of Rivet protocol frames to the
// client WebSocket so that amp Neo's UI renders the local response.
//
// frames sent to client (matching what the Rivet server-side actor would
// normally emit for a single assistant turn):
//
//  1. {"type":"agent_state","state":"streaming","messageId":"M-...","agentMode":"smart"}
//  2. {"type":"delta",...,"state":"start"}
//  3. {"type":"delta",...,"blocks":[{"type":"text","text":"...","blockState":"start"}],"blockIndex":0,"state":"generating"}
//  4. one or more {"type":"delta", ... ,"blocks":[{"type":"text","text":"<chunk>","blockState":"streaming"}],...}
//  5. {"type":"delta",..."blocks":[{"type":"text","text":"","blockState":"complete"}],"state":"generating"}
//  6. {"type":"delta",...,"state":"complete"}
//  7. {"type":"message_added","message":{...full assistant message...}}
//  8. {"type":"agent_state","state":"idle","agentMode":"smart"}
func (s *RivetSession) runLocalInference(
	ctx context.Context,
	client *websocket.Conn,
	clientMu *sync.Mutex,
	auth *AuthResolver,
	cfg *Config,
	userText, userMsgID string,
) error {
	defer s.markIdle()
	s.cfg = cfg
	s.auth = auth

	// Wrap context so client_cancel can abort upstream HTTP + tool waits.
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.inflightCancel = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflightCancel = nil
		s.mu.Unlock()
	}()
	ctx = cancelCtx

	// frameWriter owns the WS write goroutine and coalesces streaming
	// text/thinking deltas. See rivet_writer.go for the rationale.
	fw := newFrameWriter(client, clientMu, s.connID, s.threadID)
	defer fw.Close()
	writeFrameRaw := fw.Write

	sendPluginRequest := func(method string, params map[string]any) error {
		id := "actor-hook-" + uuidLike()
		ch := s.registerPluginHook(id)
		defer s.unregisterPluginHook(id)
		if err := writeFrameRaw(map[string]any{
			"type": "plugin_message",
			"message": map[string]any{
				"type":   "request",
				"id":     id,
				"method": method,
				"params": params,
			},
		}); err != nil {
			return err
		}
		select {
		case <-ch:
			return nil
		case <-time.After(5 * time.Second):
			return fmt.Errorf("plugin hook %s timed out", method)
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Echo user message back so amp's UI shows it (server normally does this).
	if userMsgID != "" {
		_ = writeFrameRaw(map[string]any{
			"type": "message_added",
			"message": map[string]any{
				"threadId":  s.threadID,
				"role":      "user",
				"content":   []map[string]any{{"type": "text", "text": userText}},
				"readAt":    nil,
				"messageId": userMsgID,
				"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
			},
			"seq": s.takeSeq(),
		})
	}

	// Cold-start parallel phase: hydration, plugin hooks, and tool
	// bootstrap all involve network/IPC round trips. Running them in
	// series (the original implementation) added 500ms-2s of pre-LLM
	// silence on the first message of a session. Kick them off in
	// parallel and only block at the points where the LLM call genuinely
	// needs the result.
	s.mu.Lock()
	needsHydrate := !s.hydrated
	s.hydrated = true
	priorLocalLen := len(s.history)
	haveTools := len(s.anthroTools) > 0
	s.mu.Unlock()

	hydrateDone := make(chan struct{})
	go func() {
		defer close(hydrateDone)
		if !needsHydrate {
			return
		}
		prior, ok := s.fetchThreadHistory(ctx, cfg)
		if !ok {
			// Network/auth failure — disable persist so we don't clobber
			// the real server-side thread with a partial upload.
			s.mu.Lock()
			s.persistUnsafe = true
			s.mu.Unlock()
			log.Warnf("[RIVET %d] thread hydration FAILED — persistence disabled this session", s.connID)
			return
		}
		s.mu.Lock()
		s.serverPriorLen = len(prior)
		if len(prior) >= priorLocalLen {
			log.Infof("[RIVET %d] hydrated history: in-mem=%d → server=%d (+1 new user msg)", s.connID, priorLocalLen, len(prior))
			newHistory := append([]anthropicMessage(nil), prior...)
			newHistory = append(newHistory, anthropicMessage{
				Role:    "user",
				Content: []anthropicContent{{Type: "text", Text: userText}},
			})
			s.history = newHistory
		}
		s.mu.Unlock()
	}()

	// Plugin lifecycle hooks: amp's executor expects both before any
	// assistant turn. Fire them in parallel — they don't depend on each
	// other and can overlap network with the hydration above.
	hooksDone := make(chan struct{})
	go func() {
		defer close(hooksDone)
		var hookWG sync.WaitGroup
		hookWG.Add(2)
		go func() {
			defer hookWG.Done()
			if err := sendPluginRequest("session.start", map[string]any{
				"event": map[string]any{"thread": map[string]any{"id": s.threadID}},
			}); err != nil {
				log.Warnf("[RIVET %d] session.start hook: %v", s.connID, err)
			}
		}()
		go func() {
			defer hookWG.Done()
			if err := sendPluginRequest("agent.start", map[string]any{
				"event": map[string]any{
					"thread":  map[string]any{"id": s.threadID},
					"message": userText,
					"id":      userMsgID,
				},
			}); err != nil {
				log.Warnf("[RIVET %d] agent.start hook: %v", s.connID, err)
			}
		}()
		hookWG.Wait()
	}()

	// Tools bootstrap: only wait if we don't already have tools cached
	// from a previous turn. On warm threads this skip saves ~50-200ms
	// of "wait for executor_tools_register to arrive" delay.
	if !haveTools {
		if !s.waitForToolsBootstrap(ctx, 10*time.Second) {
			log.Warnf("[RIVET %d] tools bootstrap timeout — running with current tools (n=%d)", s.connID, len(s.anthroTools))
		}
	}

	// Wait for hydration before building the request body — it modifies
	// s.history and the round handler snapshots that history.
	<-hydrateDone
	// Wait for hooks too: the executor needs to know about agent.start
	// before processing any tool calls in this turn.
	<-hooksDone

	provider, model := modelForAgentMode(s.agentMode)
	log.Infof("[RIVET %d] dispatch agentMode=%s → %s/%s", s.connID, s.agentMode, provider, model)

	authInfo, route := auth.Resolve(ctx, provider, model)
	// Accept both RouteLocal (OAuth) and RouteAPIKey (user's own key with
	// optional BaseURL override) — both keep inference off the Amp servers.
	if authInfo == nil || !authInfo.Valid() || (route != RouteLocal && route != RouteAPIKey) {
		// Provider-specific fallbacks: frontier (Gemini) → Smart (Opus 4.7).
		// Most users have Claude OAuth but not a proper Vertex/Gemini API key,
		// and getting frontier mode to "work" with Opus is strictly better
		// than failing entirely.
		if provider == "google" || (s.remoteDriven && provider == "openai") {
			log.Warnf("[RIVET %d] %s unavailable for %s — falling back to anthropic/claude-opus-4-7", s.connID, provider, model)
			provider = "anthropic"
			model = "claude-opus-4-7"
			authInfo, route = auth.Resolve(ctx, provider, model)
		}
		if authInfo == nil || !authInfo.Valid() || (route != RouteLocal && route != RouteAPIKey) {
			errMsg := fmt.Sprintf("[amp-proxy] no usable %s credentials for %s (route=%s). Configure local OAuth / API key, or set thread mode to one we can route.", provider, model, route)
			messageID := newAmpMessageID()
			now := time.Now().UTC().Format(time.RFC3339Nano)
			_ = writeFrameRaw(map[string]any{
				"type": "message_added",
				"message": map[string]any{
					"threadId":  s.threadID,
					"role":      "assistant",
					"content":   []map[string]any{{"type": "text", "text": errMsg}},
					"state":     map[string]any{"type": "complete", "stopReason": "end_turn"},
					"messageId": messageID,
					"createdAt": now,
				},
				"seq": s.takeSeq(),
			})
			_ = writeFrameRaw(map[string]any{
				"type":      "agent_state",
				"state":     "idle",
				"agentMode": s.agentMode,
			})
			// Close WS so amp -x exits.
			clientMu.Lock()
			_ = client.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "auth failure"),
				time.Now().Add(2*time.Second),
			)
			clientMu.Unlock()
			return fmt.Errorf("no usable %s credentials for %s (route=%s)", provider, model, route)
		}
	}

	// Tool loop: each iteration makes one LLM call; if it returns tool_use
	// blocks, execute them via amp's local executor and loop with the
	// tool_result in history. Bounded to prevent infinite loops.
	// stableUserID is used for Claude Code identity injection so Anthropic
	// doesn't put us in the strict "unknown client" rate-limit bucket.
	stableUserID := authInfo.UserID
	if stableUserID == "" {
		stableUserID = cfg.UserID
	}

	const maxToolRounds = 32
	var finalText string
	var finalMessageID string
	// Snapshot the resolved route for stats logging in round handlers.
	s.currentRoute = route
	for round := 0; round < maxToolRounds; round++ {
		stop, text, msgID, err := s.runOneInferenceRound(ctx, client, clientMu, authInfo, stableUserID, provider, model, writeFrameRaw)
		if err != nil {
			// Auto-fallback: if Gemini failed with scope/permission error
			// (the common "free Gemini OAuth doesn't have AI Studio scope"
			// case), retry once with claude-opus-4-7 instead of failing.
			if provider == "google" && round == 0 && (strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "PERMISSION_DENIED") || strings.Contains(err.Error(), "SCOPE_INSUFFICIENT")) {
				log.Warnf("[RIVET %d] gemini failed (%v) — falling back to anthropic/claude-opus-4-7", s.connID, err)
				provider = "anthropic"
				model = "claude-opus-4-7"
				newAuth, newRoute := auth.Resolve(ctx, provider, model)
				if newAuth != nil && newAuth.Valid() && (newRoute == RouteLocal || newRoute == RouteAPIKey) {
					authInfo = newAuth
					continue // retry same round with new provider
				}
			}
			// Ensure amp -x exits even on failure: send idle state and close WS.
			_ = writeFrameRaw(map[string]any{"type": "agent_state", "state": "idle", "agentMode": "smart"})
			clientMu.Lock()
			_ = client.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "inference error"),
				time.Now().Add(2*time.Second),
			)
			clientMu.Unlock()
			return err
		}
		finalText = text
		finalMessageID = msgID
		if stop {
			break
		}
	}
	_ = finalText
	_ = finalMessageID
	// Fire agent.end plugin hook fire-and-forget. We don't await the
	// executor's response because amp -x already closes the WS by the
	// time we get here, and we don't need the reply.
	_ = writeFrameRaw(map[string]any{
		"type": "plugin_message",
		"message": map[string]any{
			"type":   "request",
			"id":     "actor-hook-" + uuidLike(),
			"method": "agent.end",
			"params": map[string]any{
				"event": map[string]any{
					"thread": map[string]any{"id": s.threadID},
					"id":     userMsgID,
				},
			},
		},
	})
	// Generate thread title (background, best effort) if not yet set and
	// this is the first user message of the thread.
	s.mu.Lock()
	noTitle := s.threadTitle == ""
	titleFirstMsg := ""
	for _, m := range s.history {
		if m.Role == "user" {
			for _, c := range m.Content {
				if c.Type == "text" && c.Text != "" {
					titleFirstMsg = c.Text
					break
				}
			}
			if titleFirstMsg != "" {
				break
			}
		}
	}
	s.mu.Unlock()
	// Flush any pending coalesced delta and shut down the writer goroutine
	// before kicking off background work — so the assistant message frame
	// has fully landed by the time title/persist run. (defer fw.Close()
	// also fires on return, but doing it here lets the background goroutine
	// write the thread_title frame directly without racing the close.)
	fw.Close()

	// Title generation + thread persistence are not on the user's perceived
	// streaming path: agent_state→idle was already emitted at the end of
	// the last inference round. Run them in the background with their own
	// timeout context so the parent goroutine can return immediately.
	if noTitle && titleFirstMsg != "" {
		go func(text string) {
			bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.generateAndEmitTitle(bgCtx, auth, cfg, text, func(obj map[string]any) error {
				buf, _ := json.Marshal(obj)
				clientMu.Lock()
				defer clientMu.Unlock()
				return client.WriteMessage(websocket.TextMessage, buf)
			})
		}(titleFirstMsg)
	}
	// Persist the assembled thread (user + assistant + any tool rounds) to
	// the Amp server via /api/internal?uploadThread. amp client's own
	// throttled uploader can't be relied on in execute-mode because it
	// exits before the 1s throttle fires.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		s.persistThreadToAmp(bgCtx, cfg)
	}()
	return nil
}

// runOneInferenceRound makes a single Anthropic call, emits the streaming
// frames, and either returns (stop=true) with the final text on a normal
// end_turn, or executes tool_use blocks against amp's executor and returns
// (stop=false) so the caller can loop.
func (s *RivetSession) runOneInferenceRound(
	ctx context.Context,
	client *websocket.Conn,
	clientMu *sync.Mutex,
	authInfo *ProviderAuth,
	stableUserID string,
	provider string,
	model string,
	writeFrameRaw func(map[string]any) error,
) (stop bool, finalText, msgID string, err error) {
	// Dispatch by provider. Each path translates the session's history into
	// the provider's request shape, streams the response, and emits Rivet
	// frames so amp's UI renders correctly.
	switch provider {
	case "openai":
		return s.runOpenAIRound(ctx, client, clientMu, authInfo, model, writeFrameRaw)
	case "google":
		return s.runGeminiRound(ctx, client, clientMu, authInfo, model, writeFrameRaw)
	}
	// fall through to anthropic-inline path below
	system := s.buildSystemPrompt()
	s.mu.Lock()
	historyCopy := make([]anthropicMessage, len(s.history))
	copy(historyCopy, s.history)
	tools := append([]map[string]any(nil), s.anthroTools...)
	s.mu.Unlock()

	// Compact long histories to stay under the model context window.
	// Rough: 4 bytes ≈ 1 token. Trigger at 80% of context to leave room
	// for the response. Opus 4.7=200k, Opus 4.6=1M, Haiku 4.5=200k.
	contextLimit := 200000
	if strings.Contains(model, "opus-4-6") {
		contextLimit = 1000000
	}
	historyCopy = compactHistory(historyCopy, contextLimit*4*8/10, s.auth, s.cfg) // 80% in bytes

	maxTokens := 8192
	reqBody := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"stream":     true,
		"system": []map[string]any{
			{"type": "text", "text": system},
		},
		"messages": historyCopy,
	}
	if len(tools) > 0 {
		reqBody["tools"] = tools
	}
	// Extended thinking budget by reasoning effort. Disabled for "low"/empty
	// so simple prompts return faster; large budget for xhigh.
	s.mu.Lock()
	effort := s.reasoningEffort
	s.mu.Unlock()
	if budget := thinkingBudgetForEffort(effort); budget > 0 {
		// Anthropic requires max_tokens > thinking.budget_tokens.
		if maxTokens <= budget {
			reqBody["max_tokens"] = budget + 2048
		}
		reqBody["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": budget,
		}
	}
	bodyBytes, _ := json.Marshal(reqBody)

	// Match Claude Code's request body identity: prepends the billing header
	// (`x-anthropic-billing-header: cc_version=...; cc_entrypoint=cli; ...`)
	// and agent identifier into system, plus a stable metadata.user_id.
	// Anthropic categorizes OAuth rate limits by this fingerprint — without
	// it our calls hit the "unknown client" bucket and 429 quickly.
	bodyBytes = injectClaudeCodeIdentity(bodyBytes, stableUserID)

	base := "https://api.anthropic.com"
	if authInfo.BaseURL != "" {
		base = strings.TrimRight(authInfo.BaseURL, "/")
	}
	req, rerr := http.NewRequestWithContext(ctx, "POST", base+"/v1/messages?beta=true", bytes.NewReader(bodyBytes))
	if rerr != nil {
		return false, "", "", rerr
	}
	if authInfo.AuthType == AuthXAPIKey {
		req.Header.Set("x-api-key", authInfo.Token)
	} else {
		req.Header.Set("Authorization", "Bearer "+authInfo.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20,prompt-caching-2024-07-31")
	req.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	// Match Claude Code's full header fingerprint — Anthropic appears to
	// rate-limit OAuth traffic based on this fingerprint, and an incomplete
	// set lands us in a tighter pool than the official client.
	req.Header.Set("X-App", "cli")
	req.Header.Set("User-Agent", "claude-cli/2.1.81 (external, cli)")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Stainless-Runtime-Version", "v22.16.0")
	req.Header.Set("X-Stainless-Package-Version", "0.80.0")
	req.Header.Set("X-Stainless-Os", "MacOS")
	req.Header.Set("X-Stainless-Arch", "arm64")
	req.Header.Set("X-Stainless-Retry-Count", "0")
	req.Header.Set("X-Stainless-Timeout", "600")
	req.Header.Set("X-Stainless-Helper-Method", "stream")
	req.Header.Set("Connection", "keep-alive")

	httpClient := &http.Client{Timeout: 5 * time.Minute}
	// Retry transient 429/5xx with exponential backoff. Anthropic rate-limits
	// per-account; opus 4.7 has tighter limits than haiku, so brief retries
	// often recover.
	var resp *http.Response
	var derr error
	var retries int
	backoffs := []time.Duration{1 * time.Second, 3 * time.Second, 8 * time.Second}
	// Log start for stats; RecordResult below completes it. routeLabel uses
	// the same values as the direct HTTP handlers ("local" / "apikey") so
	// the admin dashboard groups Rivet-local rows together with direct calls.
	routeLabel := routeLabelFor(s.currentRoute)
	logStart := time.Now()
	if s.logger != nil {
		s.logger.LogRequest(model, provider, routeLabel, "/v1/messages", logStart)
	}
	for attempt := 0; ; attempt++ {
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		resp, derr = httpClient.Do(req)
		if derr != nil {
			if s.logger != nil {
				s.logger.RecordResult(model, 0, TokenUsage{}, retries, derr.Error(), "", "")
			}
			return false, "", "", fmt.Errorf("anthropic request failed: %w", derr)
		}
		if resp.StatusCode != 429 && resp.StatusCode < 500 {
			break
		}
		if attempt >= len(backoffs) {
			break
		}
		_ = resp.Body.Close()
		retries++
		log.Warnf("[RIVET %d] anthropic %d, retrying in %v (attempt %d)", s.connID, resp.StatusCode, backoffs[attempt], attempt+1)
		select {
		case <-time.After(backoffs[attempt]):
		case <-ctx.Done():
			if s.logger != nil {
				s.logger.RecordResult(model, 0, TokenUsage{}, retries, ctx.Err().Error(), "", "")
			}
			return false, "", "", ctx.Err()
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		if s.logger != nil {
			s.logger.RecordResult(model, resp.StatusCode, TokenUsage{}, retries, truncateStr(string(errBody), 200), "", string(errBody))
		}
		_ = writeFrameRaw(map[string]any{
			"type": "message_added",
			"message": map[string]any{
				"threadId":  s.threadID,
				"role":      "assistant",
				"content":   []map[string]any{{"type": "text", "text": fmt.Sprintf("[amp-proxy] Claude error %d: %s", resp.StatusCode, truncateStr(string(errBody), 200))}},
				"messageId": newAmpMessageID(),
				"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
			},
			"seq": s.takeSeq(),
		})
		return true, "", "", fmt.Errorf("anthropic status %d: %s", resp.StatusCode, string(errBody))
	}

	messageID := newAmpMessageID()
	writeFrame := writeFrameRaw

	// inference_tools — advertise tool names so amp's UI can show them.
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		if n, _ := t["name"].(string); n != "" {
			toolNames = append(toolNames, n)
		}
	}
	if err := writeFrame(map[string]any{
		"type":      "inference_tools",
		"messageId": messageID,
		"agentMode": "smart",
		"tools":     toolNames,
	}); err != nil {
		return false, "", "", err
	}
	if err := writeFrame(map[string]any{
		"type":            "agent_state",
		"state":           "streaming",
		"messageId":       messageID,
		"agentMode":       "smart",
		"reasoningEffort": "high",
	}); err != nil {
		return false, "", "", err
	}
	if err := writeFrame(map[string]any{
		"type":      "delta",
		"messageId": messageID,
		"role":      "assistant",
		"state":     "start",
	}); err != nil {
		return false, "", "", err
	}

	// Per-block accumulation. amp expects a 3-phase per-block lifecycle
	// (start → streaming → complete) and we mirror Anthropic's block index.
	type liveBlock struct {
		idx        int
		kind       string // "text" | "tool_use" | "thinking"
		text       strings.Builder
		toolID     string
		toolName   string
		toolJSONIn strings.Builder
		signature  string // for thinking blocks
	}
	type usageData struct {
		InputTokens              int
		OutputTokens             int
		CacheCreationInputTokens int
		CacheReadInputTokens     int
	}
	blocks := map[int]*liveBlock{}
	stopReason := ""
	realUsage := usageData{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		evt := gjson.GetBytes(payload, "type").String()
		idx := int(gjson.GetBytes(payload, "index").Int())
		switch evt {
		case "content_block_start":
			t := gjson.GetBytes(payload, "content_block.type").String()
			lb := &liveBlock{idx: idx, kind: t}
			if t == "thinking" {
				blocks[idx] = lb
				_ = writeFrame(map[string]any{
					"type":       "delta",
					"messageId":  messageID,
					"role":       "assistant",
					"blocks":     []map[string]any{{"type": "thinking", "thinking": "", "blockState": "start"}},
					"blockIndex": idx,
					"state":      "generating",
				})
			} else if t == "tool_use" {
				// Translate Anthropic's tool_use_id → amp's TU-... id format
				// expected by amp's executor. Amp keys results on this id.
				lb.toolID = "TU-" + newAmpMessageIDRaw()
				lb.toolName = gjson.GetBytes(payload, "content_block.name").String()
				blocks[idx] = lb
				_ = writeFrame(map[string]any{
					"type":      "delta",
					"messageId": messageID,
					"role":      "assistant",
					"blocks": []map[string]any{{
						"type":             "tool_use",
						"id":               lb.toolID,
						"name":             lb.toolName,
						"blockState":       "start",
						"complete":         false,
						"input":            map[string]any{},
						"inputIncomplete":  map[string]any{},
						"inputPartialJSON": map[string]any{"json": ""},
					}},
					"blockIndex": idx,
					"state":      "generating",
				})
			} else if t == "text" {
				blocks[idx] = lb
				_ = writeFrame(map[string]any{
					"type":       "delta",
					"messageId":  messageID,
					"role":       "assistant",
					"blocks":     []map[string]any{{"type": "text", "text": "", "blockState": "start"}},
					"blockIndex": idx,
					"state":      "generating",
				})
			}
		case "content_block_delta":
			lb := blocks[idx]
			if lb == nil {
				continue
			}
			deltaType := gjson.GetBytes(payload, "delta.type").String()
			switch deltaType {
			case "text_delta":
				txt := gjson.GetBytes(payload, "delta.text").String()
				if txt == "" {
					continue
				}
				lb.text.WriteString(txt)
				_ = writeFrame(map[string]any{
					"type":       "delta",
					"messageId":  messageID,
					"role":       "assistant",
					"blocks":     []map[string]any{{"type": "text", "text": txt, "blockState": "streaming"}},
					"blockIndex": idx,
					"state":      "generating",
				})
			case "thinking_delta":
				txt := gjson.GetBytes(payload, "delta.thinking").String()
				if txt == "" {
					continue
				}
				lb.text.WriteString(txt)
				_ = writeFrame(map[string]any{
					"type":       "delta",
					"messageId":  messageID,
					"role":       "assistant",
					"blocks":     []map[string]any{{"type": "thinking", "thinking": txt, "blockState": "streaming"}},
					"blockIndex": idx,
					"state":      "generating",
				})
			case "signature_delta":
				sig := gjson.GetBytes(payload, "delta.signature").String()
				lb.signature += sig
			case "input_json_delta":
				j := gjson.GetBytes(payload, "delta.partial_json").String()
				if j == "" {
					continue
				}
				lb.toolJSONIn.WriteString(j)
			}
		case "content_block_stop":
			lb := blocks[idx]
			if lb == nil {
				continue
			}
			switch lb.kind {
			case "tool_use":
				var inputObj any
				if jstr := lb.toolJSONIn.String(); jstr != "" {
					_ = json.Unmarshal([]byte(jstr), &inputObj)
				}
				if inputObj == nil {
					inputObj = map[string]any{}
				}
				_ = writeFrame(map[string]any{
					"type":      "delta",
					"messageId": messageID,
					"role":      "assistant",
					"blocks": []map[string]any{{
						"type":       "tool_use",
						"id":         lb.toolID,
						"name":       lb.toolName,
						"blockState": "complete",
						"complete":   true,
						"input":      inputObj,
					}},
					"blockIndex": idx,
					"state":      "generating",
				})
			case "thinking":
				_ = writeFrame(map[string]any{
					"type":       "delta",
					"messageId":  messageID,
					"role":       "assistant",
					"blocks":     []map[string]any{{"type": "thinking", "thinking": "", "blockState": "complete"}},
					"blockIndex": idx,
					"state":      "generating",
				})
			default:
				_ = writeFrame(map[string]any{
					"type":       "delta",
					"messageId":  messageID,
					"role":       "assistant",
					"blocks":     []map[string]any{{"type": "text", "text": "", "blockState": "complete"}},
					"blockIndex": idx,
					"state":      "generating",
				})
			}
		case "message_start":
			// Anthropic emits initial usage in message_start (input + cache).
			if u := gjson.GetBytes(payload, "message.usage"); u.Exists() {
				realUsage.InputTokens = int(u.Get("input_tokens").Int())
				realUsage.CacheCreationInputTokens = int(u.Get("cache_creation_input_tokens").Int())
				realUsage.CacheReadInputTokens = int(u.Get("cache_read_input_tokens").Int())
			}
		case "message_delta":
			if r := gjson.GetBytes(payload, "delta.stop_reason").String(); r != "" {
				stopReason = r
			}
			// Output tokens land in message_delta.usage as the stream finalizes.
			if u := gjson.GetBytes(payload, "usage"); u.Exists() {
				if v := u.Get("output_tokens"); v.Exists() {
					realUsage.OutputTokens = int(v.Int())
				}
				if v := u.Get("input_tokens"); v.Exists() && realUsage.InputTokens == 0 {
					realUsage.InputTokens = int(v.Int())
				}
			}
		case "message_stop":
			// loop exits
		}
	}
	if err := scanner.Err(); err != nil {
		if s.logger != nil {
			s.logger.RecordResult(model, resp.StatusCode, TokenUsage{}, retries, err.Error(), "", "")
		}
		return false, "", "", fmt.Errorf("read anthropic stream: %w", err)
	}
	// Stream succeeded — emit token usage to the stats logger.
	if s.logger != nil {
		s.logger.RecordResult(model, resp.StatusCode, TokenUsage{
			InputTokens:       int64(realUsage.InputTokens),
			OutputTokens:      int64(realUsage.OutputTokens),
			CacheCreateTokens: int64(realUsage.CacheCreationInputTokens),
			CacheReadTokens:   int64(realUsage.CacheReadInputTokens),
		}, retries, "", "", "")
	}

	// Order the live blocks back into Anthropic order.
	ordered := make([]*liveBlock, 0, len(blocks))
	for i := 0; i < len(blocks)+8; i++ {
		if lb, ok := blocks[i]; ok {
			ordered = append(ordered, lb)
		}
	}

	// Assemble final assistant message content for both: amp's message_added
	// AND the next-round Anthropic history.
	assistantContent := make([]map[string]any, 0, len(ordered))
	historyContent := make([]anthropicContent, 0, len(ordered))
	var assistantText strings.Builder
	var toolCalls []*liveBlock
	for _, lb := range ordered {
		switch lb.kind {
		case "text":
			t := lb.text.String()
			assistantContent = append(assistantContent, map[string]any{
				"type": "text", "text": t, "blockState": "complete",
			})
			historyContent = append(historyContent, anthropicContent{Type: "text", Text: t})
			assistantText.WriteString(t)
		case "thinking":
			t := lb.text.String()
			assistantContent = append(assistantContent, map[string]any{
				"type":       "thinking",
				"thinking":   t,
				"signature":  lb.signature,
				"blockState": "complete",
			})
			// Anthropic requires thinking blocks to be passed back unchanged
			// on the next assistant turn for extended-thinking with tools.
			historyContent = append(historyContent, anthropicContent{
				Type:              "thinking",
				ThinkingText:      t,
				ThinkingSignature: lb.signature,
			})
		case "tool_use":
			var inputObj any
			if jstr := lb.toolJSONIn.String(); jstr != "" {
				_ = json.Unmarshal([]byte(jstr), &inputObj)
			}
			if inputObj == nil {
				inputObj = map[string]any{}
			}
			assistantContent = append(assistantContent, map[string]any{
				"type":       "tool_use",
				"id":         lb.toolID,
				"name":       lb.toolName,
				"blockState": "complete",
				"complete":   true,
				"input":      inputObj,
			})
			historyContent = append(historyContent, anthropicContent{
				Type:      "tool_use",
				ToolUseID: lb.toolID,
				ToolName:  lb.toolName,
				ToolInput: inputObj,
			})
			toolCalls = append(toolCalls, lb)
		}
	}

	finalText = assistantText.String()
	msgID = messageID

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeFrame(map[string]any{
		"type": "message_added",
		"message": map[string]any{
			"threadId": s.threadID,
			"role":     "assistant",
			"content":  assistantContent,
			"state":    map[string]any{"type": "complete"},
			"usage": map[string]any{
				"model":                    model,
				"maxInputTokens":           300000,
				"inputTokens":              max(1, realUsage.InputTokens),
				"outputTokens":             max(1, realUsage.OutputTokens),
				"cacheCreationInputTokens": realUsage.CacheCreationInputTokens,
				"cacheReadInputTokens":     realUsage.CacheReadInputTokens,
				"totalInputTokens":         max(1, realUsage.InputTokens+realUsage.CacheReadInputTokens+realUsage.CacheCreationInputTokens),
				"timestamp":                now,
			},
			"readAt":    nil,
			"messageId": messageID,
			"createdAt": now,
		},
		"seq": s.takeSeq(),
	}); err != nil {
		return false, "", "", err
	}

	// Persist assistant turn into history for the next round.
	s.mu.Lock()
	s.history = append(s.history, anthropicMessage{
		Role:    "assistant",
		Content: historyContent,
	})
	s.mu.Unlock()
	s.persistThread()

	if len(toolCalls) == 0 || stopReason == "end_turn" {
		// Signal idle so amp's UI exits its "streaming" state. Persistence
		// happens in the runLocalInference caller, after the loop exits.
		_ = writeFrameRaw(map[string]any{
			"type":      "agent_state",
			"state":     "idle",
			"agentMode": s.agentMode,
		})
		return true, finalText, messageID, nil
	}

	// Tool execution phase.
	_ = writeFrame(map[string]any{
		"type":            "agent_state",
		"state":           "tool_use",
		"messageId":       messageID,
		"agentMode":       "smart",
		"reasoningEffort": "high",
	})
	_ = writeFrame(map[string]any{
		"type":            "agent_state",
		"state":           "running_tools",
		"messageId":       messageID,
		"agentMode":       "smart",
		"reasoningEffort": "high",
	})

	// Issue tool_lease + wait for executor_tool_result for each call.
	toolResults := make([]anthropicContent, 0, len(toolCalls))
	for _, lb := range toolCalls {
		var inputObj any
		if jstr := lb.toolJSONIn.String(); jstr != "" {
			_ = json.Unmarshal([]byte(jstr), &inputObj)
		}
		if inputObj == nil {
			inputObj = map[string]any{}
		}
		ch := s.registerPendingTool(lb.toolID)
		_ = writeFrame(map[string]any{
			"type":       "tool_lease",
			"toolCallId": lb.toolID,
			"toolName":   lb.toolName,
			"args":       inputObj,
			"messageId":  messageID,
		})

		// Wait for executor_tool_result with this id (up to 5 minutes).
		var resultData []byte
		select {
		case resultData = <-ch:
		case <-time.After(5 * time.Minute):
			s.unregisterPendingTool(lb.toolID)
			return true, "", "", fmt.Errorf("tool %s (%s) timed out", lb.toolName, lb.toolID)
		case <-ctx.Done():
			s.unregisterPendingTool(lb.toolID)
			return true, "", "", ctx.Err()
		}
		s.unregisterPendingTool(lb.toolID)

		// Acknowledge to amp: tool_progress snapshot + result_ack.
		runPayload := gjson.GetBytes(resultData, "run")
		var runObj any
		_ = json.Unmarshal([]byte(runPayload.Raw), &runObj)
		_ = writeFrame(map[string]any{
			"type":       "tool_progress",
			"toolCallId": lb.toolID,
			"progress":   map[string]any{"type": "snapshot", "value": runObj},
		})
		_ = writeFrame(map[string]any{
			"type":       "executor_tool_result_ack",
			"toolCallId": lb.toolID,
		})

		// Build a user message_added with the tool_result and seq.
		toolResultMsgID := newAmpMessageID()
		_ = writeFrame(map[string]any{
			"type": "message_added",
			"message": map[string]any{
				"threadId":  s.threadID,
				"role":      "user",
				"content":   []map[string]any{{"type": "tool_result", "toolUseID": lb.toolID, "run": runObj}},
				"readAt":    nil,
				"messageId": toolResultMsgID,
				"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
			},
			"seq": s.takeSeq(),
		})

		// For Anthropic, the tool_result needs to carry the raw content string
		// (best-effort flattened from the run.result map). amp tools return
		// arbitrary JSON; we serialise it for the model.
		var serialized string
		if r := runPayload.Get("result"); r.Exists() {
			serialized = r.Raw
		}
		toolResults = append(toolResults, anthropicContent{
			Type:        "tool_result",
			ToolUseID:   lb.toolID,
			ToolContent: serialized,
		})
	}

	// Append tool results as a user message for the next Anthropic round.
	s.mu.Lock()
	s.history = append(s.history, anthropicMessage{
		Role:    "user",
		Content: toolResults,
	})
	s.mu.Unlock()
	s.persistThread()

	return false, finalText, messageID, nil
}

// runOpenAIRound handles "deep" agent mode via OpenAI Responses API (Codex
// backend). Translates session history (Anthropic-shaped) into Responses
// "input" array, including prior tool_use/tool_result as function_call /
// function_call_output items so the model has full context. Streams the
// response, emits Rivet delta frames for text and tool_use, runs tool calls
// via amp's executor, and returns stop=false when there are tool calls to
// loop on (mirroring Anthropic round semantics).
func (s *RivetSession) runOpenAIRound(
	ctx context.Context,
	client *websocket.Conn,
	clientMu *sync.Mutex,
	authInfo *ProviderAuth,
	model string,
	writeFrameRaw func(map[string]any) error,
) (stop bool, finalText, msgID string, err error) {
	s.mu.Lock()
	historyCopy := make([]anthropicMessage, len(s.history))
	copy(historyCopy, s.history)
	tools := append([]map[string]any(nil), s.anthroTools...)
	s.mu.Unlock()

	// Build Responses-shaped input from history.
	input := make([]map[string]any, 0)
	for _, msg := range historyCopy {
		switch msg.Role {
		case "user":
			// Either plain user text OR a list of tool_results
			var textParts []map[string]any
			for _, c := range msg.Content {
				switch c.Type {
				case "text":
					textParts = append(textParts, map[string]any{"type": "input_text", "text": c.Text})
				case "tool_result":
					input = append(input, map[string]any{
						"type":    "function_call_output",
						"call_id": c.ToolUseID,
						"output":  c.ToolContent,
					})
				}
			}
			if len(textParts) > 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "user",
					"content": textParts,
				})
			}
		case "assistant":
			// Mix of text and tool_use. Emit text as message, tool_use as function_call.
			var textParts []map[string]any
			for _, c := range msg.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						textParts = append(textParts, map[string]any{"type": "output_text", "text": c.Text})
					}
				case "tool_use":
					if len(textParts) > 0 {
						input = append(input, map[string]any{
							"type":    "message",
							"role":    "assistant",
							"content": textParts,
						})
						textParts = nil
					}
					argsJSON, _ := json.Marshal(c.ToolInput)
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   c.ToolUseID,
						"name":      c.ToolName,
						"arguments": string(argsJSON),
					})
				}
			}
			if len(textParts) > 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": textParts,
				})
			}
		}
	}

	// Translate amp's tools → Responses tool format.
	codexTools := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		entry := map[string]any{
			"type":        "function",
			"name":        name,
			"description": desc,
		}
		if schema, ok := t["input_schema"]; ok {
			entry["parameters"] = schema
		} else {
			entry["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		codexTools = append(codexTools, entry)
	}

	reqBody := map[string]any{
		"model":               model,
		"instructions":        s.buildSystemPrompt(),
		"input":               input,
		"tools":               codexTools,
		"stream":              true,
		"store":               false,
		"service_tier":        "priority",
		"parallel_tool_calls": false,
	}
	// Forward reasoning effort to OpenAI Responses API.
	s.mu.Lock()
	effort := s.reasoningEffort
	s.mu.Unlock()
	if rEff := openaiReasoningEffort(effort); rEff != "" {
		reqBody["reasoning"] = map[string]any{"effort": rEff}
	}
	bodyBytes, _ := json.Marshal(reqBody)

	// Default to chatgpt.com codex backend (for codex-file OAuth route).
	// For api-key route with a BaseURL override, use the Responses endpoint
	// at <BaseURL>/v1/responses.
	upstreamURL := "https://chatgpt.com/backend-api/codex/responses"
	if authInfo.Source == "api-key" && authInfo.BaseURL != "" {
		base := strings.TrimRight(authInfo.BaseURL, "/")
		upstreamURL = base + "/v1/responses"
	}
	req, rerr := http.NewRequestWithContext(ctx, "POST", upstreamURL, bytes.NewReader(bodyBytes))
	if rerr != nil {
		return false, "", "", rerr
	}
	req.Header.Set("Authorization", "Bearer "+authInfo.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("Originator", "codex_app")
	req.Header.Set("Version", "1.0.0")
	req.Header.Set("User-Agent", "codex_app/1.0.0 (Mac OS 26.0.1; arm64)")
	req.Header.Set("Session_id", uuidLike())
	if authInfo.Email != "" {
		req.Header.Set("Chatgpt-Account-Id", authInfo.Email)
	}

	httpClient := &http.Client{Timeout: 10 * time.Minute}
	// Stats logging: open the entry before send so RecordResult below can
	// complete it, mirroring the direct OpenAI HTTP handler.
	logStart := time.Now()
	routeLabel := routeLabelFor(s.currentRoute)
	if s.logger != nil {
		s.logger.LogRequest(model, "openai", routeLabel, "/v1/responses", logStart)
	}
	resp, derr := httpClient.Do(req)
	if derr != nil {
		if s.logger != nil {
			s.logger.RecordResult(model, 0, TokenUsage{}, 0, derr.Error(), "", "")
		}
		return false, "", "", fmt.Errorf("openai responses request failed: %w", derr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		if s.logger != nil {
			s.logger.RecordResult(model, resp.StatusCode, TokenUsage{}, 0, truncateStr(string(errBody), 200), "", string(errBody))
		}
		_ = writeFrameRaw(map[string]any{
			"type": "message_added",
			"message": map[string]any{
				"threadId":  s.threadID,
				"role":      "assistant",
				"content":   []map[string]any{{"type": "text", "text": fmt.Sprintf("[amp-proxy] OpenAI error %d: %s", resp.StatusCode, truncateStr(string(errBody), 200))}},
				"messageId": newAmpMessageID(),
				"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
				"state":     map[string]any{"type": "complete"},
			},
			"seq": s.takeSeq(),
		})
		return true, "", "", fmt.Errorf("openai status %d: %s", resp.StatusCode, string(errBody))
	}

	messageID := newAmpMessageID()
	writeFrame := writeFrameRaw

	toolNames := make([]string, 0, len(codexTools))
	for _, t := range codexTools {
		if n, _ := t["name"].(string); n != "" {
			toolNames = append(toolNames, n)
		}
	}
	_ = writeFrame(map[string]any{"type": "inference_tools", "messageId": messageID, "agentMode": s.agentMode, "tools": toolNames})
	_ = writeFrame(map[string]any{"type": "agent_state", "state": "streaming", "messageId": messageID, "agentMode": s.agentMode, "reasoningEffort": "medium"})
	_ = writeFrame(map[string]any{"type": "delta", "messageId": messageID, "role": "assistant", "state": "start"})

	// Track per-output-index block state. Responses API emits items in order
	// via `response.output_item.added`; each is either "message" (text) or
	// "function_call". For function_call we keep TWO ids:
	//   - callID (bare OpenAI call_xxx) — used in history for next Responses round
	//   - ampToolID ("TU-..." random) — used in amp's tool_lease/result frames
	// amp's executor only accepts TU-prefixed alphanumeric ids, so we cannot
	// pass the raw OpenAI call_xxx through to it.
	type codexBlock struct {
		idx       int
		kind      string // "text" or "tool_use"
		text      strings.Builder
		callID    string // raw OpenAI call_id (for function_call_output in next round)
		ampToolID string // TU- prefixed id used in amp-facing frames
		toolName  string
		toolArgs  strings.Builder
	}
	blocks := map[int]*codexBlock{}
	itemIDToIdx := map[string]int{}
	var streamUsage TokenUsage

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		evt := gjson.GetBytes(payload, "type").String()
		switch evt {
		case "response.output_item.added":
			itemType := gjson.GetBytes(payload, "item.type").String()
			itemID := gjson.GetBytes(payload, "item.id").String()
			idx := int(gjson.GetBytes(payload, "output_index").Int())
			switch itemType {
			case "function_call":
				callID := gjson.GetBytes(payload, "item.call_id").String()
				if callID == "" {
					callID = "stub_" + newAmpMessageIDRaw()
				}
				ampID := "TU-" + newAmpMessageIDRaw()
				name := gjson.GetBytes(payload, "item.name").String()
				blocks[idx] = &codexBlock{idx: idx, kind: "tool_use", callID: callID, ampToolID: ampID, toolName: name}
				itemIDToIdx[itemID] = idx
				_ = writeFrame(map[string]any{
					"type":      "delta",
					"messageId": messageID,
					"role":      "assistant",
					"blocks": []map[string]any{{
						"type":             "tool_use",
						"id":               ampID,
						"name":             name,
						"blockState":       "start",
						"complete":         false,
						"input":            map[string]any{},
						"inputIncomplete":  map[string]any{},
						"inputPartialJSON": map[string]any{"json": ""},
					}},
					"blockIndex": idx,
					"state":      "generating",
				})
			case "message":
				blocks[idx] = &codexBlock{idx: idx, kind: "text"}
				itemIDToIdx[itemID] = idx
				_ = writeFrame(map[string]any{
					"type":       "delta",
					"messageId":  messageID,
					"role":       "assistant",
					"blocks":     []map[string]any{{"type": "text", "text": "", "blockState": "start"}},
					"blockIndex": idx,
					"state":      "generating",
				})
			}
		case "response.output_text.delta":
			itemID := gjson.GetBytes(payload, "item_id").String()
			idx, ok := itemIDToIdx[itemID]
			if !ok {
				continue
			}
			lb := blocks[idx]
			if lb == nil {
				continue
			}
			d := gjson.GetBytes(payload, "delta").String()
			if d == "" {
				continue
			}
			lb.text.WriteString(d)
			_ = writeFrame(map[string]any{
				"type":       "delta",
				"messageId":  messageID,
				"role":       "assistant",
				"blocks":     []map[string]any{{"type": "text", "text": d, "blockState": "streaming"}},
				"blockIndex": idx,
				"state":      "generating",
			})
		case "response.function_call_arguments.delta":
			itemID := gjson.GetBytes(payload, "item_id").String()
			idx, ok := itemIDToIdx[itemID]
			if !ok {
				continue
			}
			lb := blocks[idx]
			if lb == nil {
				continue
			}
			lb.toolArgs.WriteString(gjson.GetBytes(payload, "delta").String())
		case "response.output_item.done":
			itemID := gjson.GetBytes(payload, "item.id").String()
			idx, ok := itemIDToIdx[itemID]
			if !ok {
				continue
			}
			lb := blocks[idx]
			if lb == nil {
				continue
			}
			if lb.kind == "tool_use" {
				var inputObj any
				if jstr := lb.toolArgs.String(); jstr != "" {
					_ = json.Unmarshal([]byte(jstr), &inputObj)
				}
				if inputObj == nil {
					inputObj = map[string]any{}
				}
				_ = writeFrame(map[string]any{
					"type":      "delta",
					"messageId": messageID,
					"role":      "assistant",
					"blocks": []map[string]any{{
						"type":       "tool_use",
						"id":         lb.ampToolID,
						"name":       lb.toolName,
						"blockState": "complete",
						"complete":   true,
						"input":      inputObj,
					}},
					"blockIndex": idx,
					"state":      "generating",
				})
			} else {
				_ = writeFrame(map[string]any{
					"type":       "delta",
					"messageId":  messageID,
					"role":       "assistant",
					"blocks":     []map[string]any{{"type": "text", "text": "", "blockState": "complete"}},
					"blockIndex": idx,
					"state":      "generating",
				})
			}
		case "response.completed":
			// Streaming Responses API delivers final usage here. Some
			// upstreams place it under `response.usage`, others at top level —
			// ParseOpenAIUsage handles both.
			streamUsage = ParseOpenAIUsage(payload)
			// will exit loop via scanner end
		}
	}
	// Stream done — flush usage to stats. Errors from the scanner itself
	// fall through with empty usage so the entry still completes.
	if s.logger != nil {
		errMsg := ""
		if err := scanner.Err(); err != nil {
			errMsg = err.Error()
		}
		s.logger.RecordResult(model, resp.StatusCode, streamUsage, 0, errMsg, "", "")
	}

	// Order blocks by idx, assemble assistant_added content + history content.
	ordered := make([]*codexBlock, 0, len(blocks))
	for i := 0; i < len(blocks)+8; i++ {
		if lb, ok := blocks[i]; ok {
			ordered = append(ordered, lb)
		}
	}
	assistantContent := make([]map[string]any, 0, len(ordered))
	historyContent := make([]anthropicContent, 0, len(ordered))
	var assistantText strings.Builder
	var toolCalls []*codexBlock
	for _, lb := range ordered {
		switch lb.kind {
		case "text":
			t := lb.text.String()
			assistantContent = append(assistantContent, map[string]any{
				"type": "text", "text": t, "blockState": "complete",
			})
			historyContent = append(historyContent, anthropicContent{Type: "text", Text: t})
			assistantText.WriteString(t)
		case "tool_use":
			var inputObj any
			if jstr := lb.toolArgs.String(); jstr != "" {
				_ = json.Unmarshal([]byte(jstr), &inputObj)
			}
			if inputObj == nil {
				inputObj = map[string]any{}
			}
			assistantContent = append(assistantContent, map[string]any{
				"type":       "tool_use",
				"id":         lb.ampToolID,
				"name":       lb.toolName,
				"blockState": "complete",
				"complete":   true,
				"input":      inputObj,
			})
			// Store the bare OpenAI call_id (not ampToolID) — the next
			// Responses round needs the original call_id to chain function
			// outputs.
			historyContent = append(historyContent, anthropicContent{
				Type:      "tool_use",
				ToolUseID: lb.callID,
				ToolName:  lb.toolName,
				ToolInput: inputObj,
			})
			toolCalls = append(toolCalls, lb)
		}
	}

	finalText = assistantText.String()
	msgID = messageID

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeFrame(map[string]any{
		"type": "message_added",
		"message": map[string]any{
			"threadId": s.threadID,
			"role":     "assistant",
			"content":  assistantContent,
			"state":    map[string]any{"type": "complete"},
			"usage": map[string]any{
				"model":                    model,
				"maxInputTokens":           200000,
				"inputTokens":              max(1, len(historyCopy)*50),
				"outputTokens":             max(1, len(finalText)/4),
				"cacheCreationInputTokens": 0,
				"cacheReadInputTokens":     0,
				"totalInputTokens":         max(1, len(historyCopy)*50),
				"timestamp":                now,
			},
			"readAt":    nil,
			"messageId": messageID,
			"createdAt": now,
		},
		"seq": s.takeSeq(),
	})

	s.mu.Lock()
	s.history = append(s.history, anthropicMessage{
		Role:    "assistant",
		Content: historyContent,
	})
	s.mu.Unlock()
	s.persistThread()

	if len(toolCalls) == 0 {
		clientMu.Lock()
		_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "turn complete"), time.Now().Add(2*time.Second))
		clientMu.Unlock()
		return true, finalText, messageID, nil
	}

	// Tool execution phase.
	_ = writeFrame(map[string]any{"type": "agent_state", "state": "tool_use", "messageId": messageID, "agentMode": s.agentMode, "reasoningEffort": "medium"})
	_ = writeFrame(map[string]any{"type": "agent_state", "state": "running_tools", "messageId": messageID, "agentMode": s.agentMode, "reasoningEffort": "medium"})

	toolResults := make([]anthropicContent, 0, len(toolCalls))
	for _, lb := range toolCalls {
		var inputObj any
		if jstr := lb.toolArgs.String(); jstr != "" {
			_ = json.Unmarshal([]byte(jstr), &inputObj)
		}
		if inputObj == nil {
			inputObj = map[string]any{}
		}
		ch := s.registerPendingTool(lb.ampToolID)
		_ = writeFrame(map[string]any{
			"type":       "tool_lease",
			"toolCallId": lb.ampToolID,
			"toolName":   lb.toolName,
			"args":       inputObj,
			"messageId":  messageID,
		})

		var resultData []byte
		select {
		case resultData = <-ch:
		case <-time.After(5 * time.Minute):
			s.unregisterPendingTool(lb.ampToolID)
			return true, "", "", fmt.Errorf("tool %s (%s) timed out", lb.toolName, lb.ampToolID)
		case <-ctx.Done():
			s.unregisterPendingTool(lb.ampToolID)
			return true, "", "", ctx.Err()
		}
		s.unregisterPendingTool(lb.ampToolID)

		runPayload := gjson.GetBytes(resultData, "run")
		var runObj any
		_ = json.Unmarshal([]byte(runPayload.Raw), &runObj)
		_ = writeFrame(map[string]any{"type": "tool_progress", "toolCallId": lb.ampToolID, "progress": map[string]any{"type": "snapshot", "value": runObj}})
		_ = writeFrame(map[string]any{"type": "executor_tool_result_ack", "toolCallId": lb.ampToolID})

		toolResultMsgID := newAmpMessageID()
		_ = writeFrame(map[string]any{
			"type": "message_added",
			"message": map[string]any{
				"threadId":  s.threadID,
				"role":      "user",
				"content":   []map[string]any{{"type": "tool_result", "toolUseID": lb.ampToolID, "run": runObj}},
				"readAt":    nil,
				"messageId": toolResultMsgID,
				"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
			},
			"seq": s.takeSeq(),
		})

		var serialized string
		if r := runPayload.Get("result"); r.Exists() {
			serialized = r.Raw
		}
		// Store bare callID for next OpenAI Responses input.
		toolResults = append(toolResults, anthropicContent{
			Type:        "tool_result",
			ToolUseID:   lb.callID,
			ToolContent: serialized,
		})
	}

	s.mu.Lock()
	s.history = append(s.history, anthropicMessage{
		Role:    "user",
		Content: toolResults,
	})
	s.mu.Unlock()
	s.persistThread()

	return false, finalText, messageID, nil
}

package rivet

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/fran0220/amp-proxy-neo/internal/neo/threadstore"
	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	log "github.com/sirupsen/logrus"
)

// persistThread stores the Amp-shaped thread locally first, then best-effort
// uploads it to the configured Amp upstream. Local persistence is the source
// of truth for Neo; upstream upload is compatibility/backfill only.
func (s *RivetSession) persistThread(ctx context.Context, cfg *Config) {
	if s.threadID == "" {
		return
	}
	s.mu.Lock()
	unsafe := s.persistUnsafe
	currentLen := len(s.history)
	floor := s.serverPriorLen
	s.mu.Unlock()
	if unsafe {
		log.Warnf("[RIVET %d] skipping uploadThread (persist marked unsafe)", s.connID)
		return
	}
	if floor > 0 && currentLen < floor {
		log.Warnf("[RIVET %d] skipping uploadThread (have %d msgs < server floor %d) — would clobber", s.connID, currentLen, floor)
		return
	}

	thread := s.buildThreadForUpload()
	if s.store == nil {
		log.Errorf("[RIVET %d] no local threadstore configured", s.connID)
		return
	}
	rawThread, err := json.Marshal(thread)
	if err != nil {
		log.Errorf("[RIVET %d] marshal local thread: %v", s.connID, err)
		return
	}
	localThread, err := threadstore.ParseThread(rawThread)
	if err != nil {
		log.Errorf("[RIVET %d] parse local thread: %v", s.connID, err)
		return
	}
	if err := s.store.UploadThread(ctx, localThread); err != nil {
		log.Errorf("[RIVET %d] local UploadThread failed: %v", s.connID, err)
		return
	}
	log.Infof("[RIVET %d] local UploadThread ok thread=%s msgs=%d", s.connID, s.threadID, len(localThread.Messages))

	upstreamURL := strings.TrimRight(cfg.Amp.UpstreamURL, "/")
	if upstreamURL == "" {
		return
	}
	apiKey := strings.TrimSpace(cfg.Amp.APIKey)
	if apiKey == "" {
		log.Debugf("[RIVET %d] no amp api key, skipping upstream upload", s.connID)
		return
	}

	body := map[string]any{
		"method": "uploadThread",
		"params": map[string]any{
			"thread":          thread,
			"createdOnServer": true,
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		log.Errorf("[RIVET %d] marshal uploadThread body: %v", s.connID, err)
		return
	}
	// Save uncompressed body for debugging
	_ = writeUploadDump(s.connID, s.threadID, bodyBytes)

	// Gzip the body — matches what the amp client does (vm0 in the bundle).
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(bodyBytes); err != nil {
		log.Errorf("[RIVET %d] gzip body: %v", s.connID, err)
		return
	}
	_ = zw.Close()

	endpoint := upstreamURL + "/api/internal?" + url.QueryEscape("uploadThread")
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(compressed.Bytes()))
	if err != nil {
		log.Errorf("[RIVET %d] new uploadThread request: %v", s.connID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Api-Key", apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Errorf("[RIVET %d] uploadThread post: %v", s.connID, err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Warnf("[RIVET %d] uploadThread %d: %s", s.connID, resp.StatusCode, truncateStr(string(respBody), 200))
		return
	}
	log.Infof("[RIVET %d] uploadThread ok thread=%s msgs=%d", s.connID, s.threadID, len(thread["messages"].([]map[string]any)))
}

// buildThreadForUpload converts the session's in-memory anthropic-shaped
// history into the JSON shape that Amp's server expects (matches what
// `amp threads export <id>` returns).
func (s *RivetSession) buildThreadForUpload() map[string]any {
	s.mu.Lock()
	history := append([]anthropicMessage(nil), s.history...)
	agentMode := s.agentMode
	if agentMode == "" {
		agentMode = "smart"
	}
	reasoningEffort := s.reasoningEffort
	envText := s.envText
	serverV := s.serverV
	serverMeta := append(json.RawMessage(nil), s.serverMeta...)
	serverEnv := append(json.RawMessage(nil), s.serverEnv...)
	serverCreatorUserID := s.serverCreatorUserID
	serverCreated := s.serverCreated
	title := s.threadTitle
	s.mu.Unlock()

	now := time.Now().UnixMilli()
	messages := make([]map[string]any, 0, len(history))
	nextMessageID := 0

	for _, m := range history {
		switch m.Role {
		case "user":
			// First content block is the original user prompt (text). If this
			// message is the result of a tool_result loop turn, it will only
			// contain tool_result blocks — those are still persisted as user
			// messages but without sentAt (they're inferred internally).
			contents := make([]map[string]any, 0, len(m.Content))
			isToolResultTurn := true
			for _, c := range m.Content {
				if c.Type == "tool_result" {
					contents = append(contents, map[string]any{
						"type":      "tool_result",
						"toolUseID": c.ToolUseID,
						"run":       c.ToolContent, // string or object
					})
				} else {
					isToolResultTurn = false
					if c.Type == "text" {
						contents = append(contents, map[string]any{"type": "text", "text": c.Text})
					}
				}
			}
			msg := map[string]any{
				"role":      "user",
				"content":   contents,
				"messageId": nextMessageID,
			}
			if !isToolResultTurn {
				msg["meta"] = map[string]any{"sentAt": now}
				msg["agentMode"] = agentMode
				msg["userState"] = map[string]any{"currentlyVisibleFiles": []any{}}
			}
			messages = append(messages, msg)
			nextMessageID++
		case "assistant":
			contents := make([]map[string]any, 0, len(m.Content))
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					contents = append(contents, map[string]any{"type": "text", "text": c.Text})
				case "thinking":
					contents = append(contents, map[string]any{
						"type":       "thinking",
						"provider":   "anthropic",
						"thinking":   c.ThinkingText,
						"signature":  c.ThinkingSignature,
						"startTime":  now,
						"finalTime":  now,
						"blockState": "complete",
					})
				case "tool_use":
					contents = append(contents, map[string]any{
						"type":       "tool_use",
						"id":         c.ToolUseID,
						"name":       c.ToolName,
						"input":      c.ToolInput,
						"complete":   true,
						"startTime":  now,
						"finalTime":  now,
						"blockState": "complete",
					})
				}
			}
			msg := map[string]any{
				"role":      "assistant",
				"content":   contents,
				"messageId": nextMessageID,
				"state":     map[string]any{"type": "complete", "stopReason": "end_turn"},
			}
			messages = append(messages, msg)
			nextMessageID++
		}
	}

	// v must be strictly greater than the server's current version, or
	// the server keeps the v bump but discards the messages diff.
	v := len(messages) + 1
	if serverV+1 > v {
		v = serverV + 1
	}
	created := serverCreated
	if created == 0 {
		created = now
	}
	thread := map[string]any{
		"id":            s.threadID,
		"v":             v,
		"created":       created,
		"updatedAt":     now,
		"messages":      messages,
		"agentMode":     agentMode,
		"nextMessageId": nextMessageID,
	}
	if reasoningEffort != "" {
		thread["reasoningEffort"] = reasoningEffort
	}
	if title != "" {
		thread["title"] = title
	}
	// Carry server-side fields verbatim. Without these (esp. meta and
	// creatorUserID), the server validates the upload, bumps v, but
	// silently discards the messages diff.
	if len(serverMeta) > 0 && string(serverMeta) != "null" {
		var m any
		if err := json.Unmarshal(serverMeta, &m); err == nil {
			thread["meta"] = m
		}
	}
	if len(serverEnv) > 0 && string(serverEnv) != "null" {
		var e any
		if err := json.Unmarshal(serverEnv, &e); err == nil {
			thread["env"] = e
		}
	} else if envText != "" {
		var envObj any
		if err := json.Unmarshal([]byte(envText), &envObj); err == nil {
			thread["env"] = map[string]any{"initial": envObj}
		}
	}
	if serverCreatorUserID != "" {
		thread["creatorUserID"] = serverCreatorUserID
	}
	return thread
}

// fetchThreadHistory pulls the persisted thread from Amp server via
// /api/internal?getThread and converts its messages back into the
// anthropic-shaped history slice used by the inference orchestrator.
//
// Returns (history, ok). On any failure returns (nil, false) — caller should
// proceed with empty history rather than abort.
func (s *RivetSession) fetchThreadHistory(ctx context.Context, cfg *Config) ([]anthropicMessage, bool) {
	if s.threadID == "" {
		return nil, false
	}
	if s.store != nil {
		thread, err := s.store.GetThread(ctx, s.threadID)
		if err == nil {
			out, ok := s.hydrateFromRawThread(thread.Title, thread.Raw)
			if ok {
				log.Infof("[RIVET %d] local getThread ok thread=%s persisted_msgs=%d", s.connID, s.threadID, len(thread.Messages))
				return out, true
			}
		} else if err != threadstore.ErrNotFound {
			log.Warnf("[RIVET %d] local getThread: %v", s.connID, err)
		}
	}
	upstreamURL := strings.TrimRight(cfg.Amp.UpstreamURL, "/")
	if upstreamURL == "" {
		return nil, false
	}
	apiKey := strings.TrimSpace(cfg.Amp.APIKey)
	if apiKey == "" {
		return nil, false
	}

	body, _ := json.Marshal(map[string]any{
		"method": "getThread",
		"params": map[string]any{"thread": s.threadID},
	})
	endpoint := upstreamURL + "/api/internal?" + url.QueryEscape("getThread")
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Api-Key", apiKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Warnf("[RIVET %d] getThread http: %v", s.connID, err)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warnf("[RIVET %d] getThread status %d", s.connID, resp.StatusCode)
		return nil, false
	}
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	var envelope struct {
		Ok     bool `json:"ok"`
		Result struct {
			Thread struct {
				Title string `json:"title"`
				Data  struct {
					V             int               `json:"v"`
					Created       int64             `json:"created"`
					Meta          json.RawMessage   `json:"meta"`
					Env           json.RawMessage   `json:"env"`
					CreatorUserID string            `json:"creatorUserID"`
					Messages      []json.RawMessage `json:"messages"`
				} `json:"data"`
			} `json:"thread"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		log.Warnf("[RIVET %d] getThread parse: %v", s.connID, err)
		return nil, false
	}
	if !envelope.Ok {
		return nil, false
	}
	if s.store != nil {
		if raw, err := json.Marshal(envelope.Result.Thread.Data); err == nil {
			if thread, err := threadstore.ParseThread(raw); err == nil {
				thread.Title = envelope.Result.Thread.Title
				if err := s.store.UploadThread(ctx, thread); err != nil && err != threadstore.ErrVersionConflict {
					log.Warnf("[RIVET %d] backfill local thread: %v", s.connID, err)
				}
			}
		}
	}
	// Cache server-side fields needed for round-trip preservation.
	d := envelope.Result.Thread.Data
	s.mu.Lock()
	s.serverV = d.V
	s.serverMeta = append([]byte(nil), d.Meta...)
	s.serverEnv = append([]byte(nil), d.Env...)
	s.serverCreatorUserID = d.CreatorUserID
	s.serverCreated = d.Created
	if envelope.Result.Thread.Title != "" {
		s.threadTitle = envelope.Result.Thread.Title
	}
	s.mu.Unlock()
	log.Infof("[RIVET %d] getThread ok thread=%s persisted_msgs=%d serverV=%d", s.connID, s.threadID, len(d.Messages), d.V)
	out := make([]anthropicMessage, 0, len(envelope.Result.Thread.Data.Messages))
	for _, raw := range envelope.Result.Thread.Data.Messages {
		msg, ok := persistedMsgToAnthropic(raw)
		if !ok {
			continue
		}
		out = append(out, msg)
	}
	return out, true
}

func (s *RivetSession) hydrateFromRawThread(title string, raw json.RawMessage) ([]anthropicMessage, bool) {
	var d struct {
		V             int               `json:"v"`
		Created       int64             `json:"created"`
		Meta          json.RawMessage   `json:"meta"`
		Env           json.RawMessage   `json:"env"`
		CreatorUserID string            `json:"creatorUserID"`
		Messages      []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		log.Warnf("[RIVET %d] local getThread parse: %v", s.connID, err)
		return nil, false
	}
	s.mu.Lock()
	s.serverV = d.V
	s.serverMeta = append([]byte(nil), d.Meta...)
	s.serverEnv = append([]byte(nil), d.Env...)
	s.serverCreatorUserID = d.CreatorUserID
	s.serverCreated = d.Created
	if title != "" {
		s.threadTitle = title
	}
	s.mu.Unlock()
	out := make([]anthropicMessage, 0, len(d.Messages))
	for _, raw := range d.Messages {
		msg, ok := persistedMsgToAnthropic(raw)
		if ok {
			out = append(out, msg)
		}
	}
	return out, true
}

// persistedMsgToAnthropic converts one message from the persisted thread
// JSON shape back to the anthropic-shaped struct the inference orchestrator
// uses for next-turn context.
func persistedMsgToAnthropic(raw json.RawMessage) (anthropicMessage, bool) {
	var m struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text,omitempty"`
			Thinking  string          `json:"thinking,omitempty"`
			Signature string          `json:"signature,omitempty"`
			ID        string          `json:"id,omitempty"`
			Name      string          `json:"name,omitempty"`
			Input     json.RawMessage `json:"input,omitempty"`
			// tool_result fields (in user messages of tool-result turns)
			ToolUseID string          `json:"toolUseID,omitempty"`
			Run       json.RawMessage `json:"run,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return anthropicMessage{}, false
	}
	if m.Role != "user" && m.Role != "assistant" {
		// info / system — skip; LLM doesn't need them.
		return anthropicMessage{}, false
	}
	out := anthropicMessage{Role: m.Role, Content: make([]anthropicContent, 0, len(m.Content))}
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			out.Content = append(out.Content, anthropicContent{Type: "text", Text: c.Text})
		case "thinking":
			// Anthropic requires thinking blocks to be replayed verbatim
			// with their original signature in the next turn — but only if
			// extended-thinking is enabled for THIS request AND the
			// signature is from the same model family. Cross-model or stale
			// signatures get rejected with 400.
			//
			// Safe fallback: if signature looks suspect (empty or too short
			// to be valid base64), drop the signature and convert to a text
			// block prefixed with the thinking content. The model loses the
			// signed reasoning chain but the turn still succeeds.
			if isLikelyValidThinkingSig(c.Signature) {
				out.Content = append(out.Content, anthropicContent{
					Type:              "thinking",
					ThinkingText:      c.Thinking,
					ThinkingSignature: c.Signature,
				})
			} else if c.Thinking != "" {
				out.Content = append(out.Content, anthropicContent{
					Type: "text",
					Text: "[previous reasoning] " + c.Thinking,
				})
			}
		case "tool_use":
			var input any
			if len(c.Input) > 0 {
				_ = json.Unmarshal(c.Input, &input)
			}
			out.Content = append(out.Content, anthropicContent{
				Type:      "tool_use",
				ToolUseID: c.ID,
				ToolName:  c.Name,
				ToolInput: input,
			})
		case "tool_result":
			content := ""
			if len(c.Run) > 0 {
				content = string(c.Run)
			}
			out.Content = append(out.Content, anthropicContent{
				Type:        "tool_result",
				ToolUseID:   c.ToolUseID,
				ToolContent: content,
			})
		}
	}
	if len(out.Content) == 0 {
		return anthropicMessage{}, false
	}
	return out, true
}

func init() { _ = fmt.Sprintf }

// writeUploadDump persists the uploadThread JSON to /tmp/amp-uploads/<conn>-<ts>.json
// for offline diff against ground truth. Best effort, ignores errors.
func writeUploadDump(connID uint64, threadID string, body []byte) error {
	if err := osMkdirAll("/tmp/amp-uploads", 0755); err != nil {
		return err
	}
	ts := time.Now().Format("150405.000")
	path := fmt.Sprintf("/tmp/amp-uploads/conn%d-%s-%s.json", connID, ts, threadID)
	return osWriteFile(path, body, 0644)
}

// thin wrappers so test/build doesn't pull os import here; the real
// definitions are in std lib.
var osMkdirAll = func(p string, m os.FileMode) error { return os.MkdirAll(p, m) }
var osWriteFile = func(p string, b []byte, m os.FileMode) error { return os.WriteFile(p, b, m) }

// isLikelyValidThinkingSig is a heuristic: Anthropic's extended-thinking
// signatures are base64url-encoded ~200+ char strings. Anything shorter or
// containing whitespace / unexpected chars is likely corrupt/stale and will
// trip a 400 on the next inference if we replay it verbatim.
func isLikelyValidThinkingSig(sig string) bool {
	if len(sig) < 64 {
		return false
	}
	for _, r := range sig {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

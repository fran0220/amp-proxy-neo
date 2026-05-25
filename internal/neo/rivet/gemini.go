package rivet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	. "github.com/fran0220/amp-proxy-neo/pkg/auth"
	. "github.com/fran0220/amp-proxy-neo/pkg/logger"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

var _ = websocket.TextMessage

// runGeminiRound dispatches one inference round to Google Gemini's
// streamGenerateContent endpoint. Translates anthropic-shaped history into
// Gemini's contents format, parses SSE response, and emits Rivet delta /
// message_added frames.
//
// Supports text + tool calling. Vision (image_url parts) is left as TODO.
func (s *RivetSession) runGeminiRound(
	ctx context.Context,
	client *websocket.Conn,
	clientMu *sync.Mutex,
	authInfo *ProviderAuth,
	model string,
	writeFrameRaw func(map[string]any) error,
) (stop bool, finalText, msgID string, err error) {
	system := s.buildSystemPrompt()
	s.mu.Lock()
	historyCopy := make([]anthropicMessage, len(s.history))
	copy(historyCopy, s.history)
	tools := append([]map[string]any(nil), s.anthroTools...)
	s.mu.Unlock()

	// Translate history → Gemini contents[]
	contents := make([]map[string]any, 0, len(historyCopy))
	for _, m := range historyCopy {
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		parts := make([]map[string]any, 0, len(m.Content))
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					parts = append(parts, map[string]any{"text": c.Text})
				}
			case "tool_use":
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{
						"name": c.ToolName,
						"args": c.ToolInput,
					},
				})
			case "tool_result":
				// Gemini wants functionResponse with name + response
				var responseObj any
				if c.ToolContent != "" {
					_ = json.Unmarshal([]byte(c.ToolContent), &responseObj)
					if responseObj == nil {
						responseObj = map[string]any{"output": c.ToolContent}
					}
				} else {
					responseObj = map[string]any{}
				}
				// Gemini requires name on functionResponse too; we don't have it
				// per tool_result block (Anthropic only requires tool_use_id).
				// Use the id as a stand-in name.
				parts = append(parts, map[string]any{
					"functionResponse": map[string]any{
						"name":     c.ToolUseID,
						"response": responseObj,
					},
				})
			}
		}
		if len(parts) > 0 {
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
	}

	// Translate Anthropic tools → Gemini functionDeclarations
	var geminiTools []map[string]any
	if len(tools) > 0 {
		decls := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			name, _ := t["name"].(string)
			desc, _ := t["description"].(string)
			schema := t["input_schema"]
			entry := map[string]any{"name": name, "description": desc}
			if schema != nil {
				entry["parameters"] = schema
			}
			decls = append(decls, entry)
		}
		geminiTools = []map[string]any{{"functionDeclarations": decls}}
	}

	reqBody := map[string]any{
		"contents": contents,
	}
	if system != "" {
		reqBody["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}
	if len(geminiTools) > 0 {
		reqBody["tools"] = geminiTools
	}
	// Reasoning effort → thinkingConfig
	s.mu.Lock()
	effort := s.reasoningEffort
	s.mu.Unlock()
	if effort != "" {
		// Gemini thinking budget: low/medium/high/xhigh → approximate token budget.
		budget := 0
		switch strings.ToLower(effort) {
		case "low":
			budget = 1024
		case "medium":
			budget = 4096
		case "high":
			budget = 12288
		case "xhigh", "max":
			budget = 32768
		}
		if budget > 0 {
			reqBody["generationConfig"] = map[string]any{
				"thinkingConfig": map[string]any{"thinkingBudget": budget},
			}
		}
	}
	bodyBytes, _ := json.Marshal(reqBody)

	base := "https://generativelanguage.googleapis.com"
	if authInfo.BaseURL != "" {
		base = strings.TrimRight(authInfo.BaseURL, "/")
	}
	endpoint := fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", base, model)

	req, rerr := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if rerr != nil {
		return false, "", "", rerr
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if authInfo.AuthType == AuthGoogAPIKey {
		req.Header.Set("x-goog-api-key", authInfo.Token)
	} else {
		req.Header.Set("Authorization", "Bearer "+authInfo.Token)
	}

	httpClient := &http.Client{Timeout: 5 * time.Minute}
	// Stats logging: open the entry before send so RecordResult below can
	// complete it. Mirrors the direct Gemini HTTP handler.
	logStart := time.Now()
	routeLabel := routeLabelFor(s.currentRoute)
	if s.logger != nil {
		s.logger.LogRequest(model, "google", routeLabel, "/v1beta/models/"+model+":streamGenerateContent", logStart)
	}
	resp, derr := httpClient.Do(req)
	if derr != nil {
		if s.logger != nil {
			s.logger.RecordResult(model, 0, TokenUsage{}, 0, derr.Error(), "", "")
		}
		return false, "", "", fmt.Errorf("gemini request failed: %w", derr)
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
				"content":   []map[string]any{{"type": "text", "text": fmt.Sprintf("[amp-proxy] Gemini error %d: %s", resp.StatusCode, truncateStr(string(errBody), 200))}},
				"messageId": newAmpMessageID(),
				"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
				"state":     map[string]any{"type": "complete"},
			},
			"seq": s.takeSeq(),
		})
		return true, "", "", fmt.Errorf("gemini status %d: %s", resp.StatusCode, string(errBody))
	}

	messageID := newAmpMessageID()
	_ = writeFrameRaw(map[string]any{
		"type":      "inference_tools",
		"messageId": messageID,
		"agentMode": s.agentMode,
		"tools":     []string{},
	})
	_ = writeFrameRaw(map[string]any{
		"type":      "agent_state",
		"state":     "streaming",
		"messageId": messageID,
		"agentMode": s.agentMode,
	})
	_ = writeFrameRaw(map[string]any{
		"type":      "delta",
		"messageId": messageID,
		"role":      "assistant",
		"state":     "start",
	})

	// Open one text block; we don't pre-emit a "start" for tool blocks (Gemini
	// streams the full functionCall in one chunk so we emit start+complete together).
	textBlockIdx := 0
	textBlockOpen := false
	var textAccum strings.Builder
	var toolCalls []struct {
		idx  int
		id   string
		name string
		args any
	}
	toolBlockIdx := 1
	stopReason := "end_turn"
	var streamUsage TokenUsage

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		gjson.GetBytes(payload, "candidates").ForEach(func(_, cand gjson.Result) bool {
			cand.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" {
					if !textBlockOpen {
						_ = writeFrameRaw(map[string]any{
							"type":       "delta",
							"messageId":  messageID,
							"role":       "assistant",
							"blocks":     []map[string]any{{"type": "text", "text": "", "blockState": "start"}},
							"blockIndex": textBlockIdx,
							"state":      "generating",
						})
						textBlockOpen = true
					}
					textAccum.WriteString(text)
					_ = writeFrameRaw(map[string]any{
						"type":       "delta",
						"messageId":  messageID,
						"role":       "assistant",
						"blocks":     []map[string]any{{"type": "text", "text": text, "blockState": "streaming"}},
						"blockIndex": textBlockIdx,
						"state":      "generating",
					})
				}
				if fc := part.Get("functionCall"); fc.Exists() {
					id := "toolu_gem_" + newAmpMessageIDRaw()
					name := fc.Get("name").String()
					var args any
					_ = json.Unmarshal([]byte(fc.Get("args").Raw), &args)
					if args == nil {
						args = map[string]any{}
					}
					idx := toolBlockIdx
					toolBlockIdx++
					_ = writeFrameRaw(map[string]any{
						"type":      "delta",
						"messageId": messageID,
						"role":      "assistant",
						"blocks": []map[string]any{{
							"type": "tool_use", "id": id, "name": name,
							"blockState": "complete", "complete": true, "input": args,
						}},
						"blockIndex": idx,
						"state":      "generating",
					})
					toolCalls = append(toolCalls, struct {
						idx  int
						id   string
						name string
						args any
					}{idx, id, name, args})
				}
				return true
			})
			if reason := cand.Get("finishReason").String(); reason != "" && reason != "FINISH_REASON_UNSPECIFIED" {
				if reason == "STOP" {
					stopReason = "end_turn"
				} else if reason == "MAX_TOKENS" {
					stopReason = "max_tokens"
				} else {
					stopReason = strings.ToLower(reason)
				}
			}
			return true
		})
		// Gemini emits usageMetadata on each SSE chunk; the last one wins.
		if u := ParseGeminiUsage(payload); u.InputTokens > 0 || u.OutputTokens > 0 {
			streamUsage = u
		}
	}
	if err := scanner.Err(); err != nil {
		if s.logger != nil {
			s.logger.RecordResult(model, resp.StatusCode, streamUsage, 0, err.Error(), "", "")
		}
		return false, "", "", fmt.Errorf("read gemini stream: %w", err)
	}
	if s.logger != nil {
		s.logger.RecordResult(model, resp.StatusCode, streamUsage, 0, "", "", "")
	}

	if textBlockOpen {
		_ = writeFrameRaw(map[string]any{
			"type":       "delta",
			"messageId":  messageID,
			"role":       "assistant",
			"blocks":     []map[string]any{{"type": "text", "text": "", "blockState": "complete"}},
			"blockIndex": textBlockIdx,
			"state":      "generating",
		})
	}

	// Assemble final message
	finalContent := []map[string]any{}
	historyContent := []anthropicContent{}
	if t := textAccum.String(); t != "" {
		finalContent = append(finalContent, map[string]any{"type": "text", "text": t})
		historyContent = append(historyContent, anthropicContent{Type: "text", Text: t})
	}
	for _, tc := range toolCalls {
		finalContent = append(finalContent, map[string]any{
			"type": "tool_use", "id": tc.id, "name": tc.name, "input": tc.args,
			"complete": true, "blockState": "complete",
		})
		historyContent = append(historyContent, anthropicContent{
			Type: "tool_use", ToolUseID: tc.id, ToolName: tc.name, ToolInput: tc.args,
		})
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeFrameRaw(map[string]any{
		"type": "message_added",
		"message": map[string]any{
			"threadId": s.threadID,
			"role":     "assistant",
			"content":  finalContent,
			"state":    map[string]any{"type": "complete", "stopReason": stopReason},
			"usage": map[string]any{
				"model":     model,
				"timestamp": now,
			},
			"readAt":    nil,
			"messageId": messageID,
			"createdAt": now,
		},
		"seq": s.takeSeq(),
	})

	// Append to history
	s.mu.Lock()
	s.history = append(s.history, anthropicMessage{Role: "assistant", Content: historyContent})
	s.mu.Unlock()
	s.persistInMemoryThread()

	if len(toolCalls) == 0 || stopReason != "end_turn" && len(toolCalls) == 0 {
		_ = writeFrameRaw(map[string]any{"type": "agent_state", "state": "idle", "agentMode": s.agentMode})
		return true, textAccum.String(), messageID, nil
	}

	if len(toolCalls) == 0 {
		_ = writeFrameRaw(map[string]any{"type": "agent_state", "state": "idle", "agentMode": s.agentMode})
		return true, textAccum.String(), messageID, nil
	}

	// Tool execution phase (mirror anthropic path)
	_ = writeFrameRaw(map[string]any{"type": "agent_state", "state": "running_tools", "messageId": messageID, "agentMode": s.agentMode})
	toolResults := []anthropicContent{}
	for _, tc := range toolCalls {
		ch := s.registerPendingTool(tc.id)
		_ = writeFrameRaw(map[string]any{
			"type":       "tool_lease",
			"toolCallId": tc.id,
			"toolName":   tc.name,
			"args":       tc.args,
			"messageId":  messageID,
		})
		var resultData []byte
		select {
		case resultData = <-ch:
		case <-time.After(5 * time.Minute):
			s.unregisterPendingTool(tc.id)
			return true, "", "", fmt.Errorf("tool %s (%s) timed out", tc.name, tc.id)
		case <-ctx.Done():
			s.unregisterPendingTool(tc.id)
			return true, "", "", ctx.Err()
		}
		s.unregisterPendingTool(tc.id)

		runPayload := gjson.GetBytes(resultData, "run")
		var runObj any
		_ = json.Unmarshal([]byte(runPayload.Raw), &runObj)
		_ = writeFrameRaw(map[string]any{
			"type":       "tool_progress",
			"toolCallId": tc.id,
			"progress":   map[string]any{"type": "snapshot", "value": runObj},
		})
		_ = writeFrameRaw(map[string]any{
			"type":       "executor_tool_result_ack",
			"toolCallId": tc.id,
		})
		toolResultMsgID := newAmpMessageID()
		_ = writeFrameRaw(map[string]any{
			"type": "message_added",
			"message": map[string]any{
				"threadId":  s.threadID,
				"role":      "user",
				"content":   []map[string]any{{"type": "tool_result", "toolUseID": tc.id, "run": runObj}},
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
		toolResults = append(toolResults, anthropicContent{
			Type: "tool_result", ToolUseID: tc.id, ToolContent: serialized,
		})
	}
	s.mu.Lock()
	s.history = append(s.history, anthropicMessage{Role: "user", Content: toolResults})
	s.mu.Unlock()
	s.persistInMemoryThread()
	return false, textAccum.String(), messageID, nil
}

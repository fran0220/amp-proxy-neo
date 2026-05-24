package rivet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	. "github.com/fran0220/amp-proxy-neo/pkg/auth"
	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// generateAndEmitTitle calls Claude haiku-4-5 with the first user message to
// produce a 5-word title for the thread, then:
//   - emits a `thread_title` Rivet frame so the client UI updates immediately
//   - caches it in the session so the next uploadThread includes it
//
// Mirrors the amp client's bJ0 function (uses set_title tool, max 60 tokens,
// system prompt asks for ≤5 words sentence-case).
//
// Best-effort: any failure is logged and ignored — thread will just show
// "Untitled" which is what we had before.
func (s *RivetSession) generateAndEmitTitle(
	ctx context.Context,
	authResolver *AuthResolver,
	cfg *Config,
	firstUserText string,
	writeFrameRaw func(map[string]any) error,
) {
	if firstUserText == "" {
		return
	}
	s.mu.Lock()
	if s.threadTitle != "" {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	auth, route := authResolver.Resolve(ctx, "anthropic", "claude-haiku-4-5-20251001")
	if auth == nil || !auth.Valid() || (route != RouteLocal && route != RouteAPIKey) {
		log.Warnf("[RIVET %d] title: no haiku credentials (route=%s) — skipping", s.connID, route)
		return
	}

	const systemPrompt = `You are an assistant that generates short, descriptive titles (maximum 5 words, "Sentence case" with the first word capitalized not "Title Case") based on user's message to an agentic coding tool. Your titles should be concise (max 5 words) and capture the essence of the query or topic. DO NOT ASSUME OR GUESS the user's intent beyond what is in their message. Omit generic words like "question", "request", etc. Be professional and precise. Use common software engineering terms and acronyms if they are helpful. Use the set_title tool to provide your answer.`

	reqBody := map[string]any{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 60,
		"system":     systemPrompt,
		"messages": []map[string]any{
			{"role": "user", "content": fmt.Sprintf("<message>%s</message>", firstUserText)},
		},
		"tools": []map[string]any{{
			"name": "set_title",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{"type": "string", "description": "the short title"},
				},
				"required": []string{"title"},
			},
		}},
		"tool_choice": map[string]any{"type": "tool", "name": "set_title"},
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
	titleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(titleCtx, "POST", base+"/v1/messages?beta=true", bytes.NewReader(bodyBytes))
	if auth.AuthType == AuthXAPIKey {
		req.Header.Set("x-api-key", auth.Token)
	} else {
		req.Header.Set("Authorization", "Bearer "+auth.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20,prompt-caching-2024-07-31")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Warnf("[RIVET %d] title gen failed: %v", s.connID, err)
		return
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Warnf("[RIVET %d] title gen status %d: %s", s.connID, resp.StatusCode, truncateStr(string(respBytes), 200))
		return
	}

	// Extract title from tool_use content block input.
	title := ""
	gjson.GetBytes(respBytes, "content").ForEach(func(_, c gjson.Result) bool {
		if c.Get("type").String() == "tool_use" && c.Get("name").String() == "set_title" {
			title = c.Get("input.title").String()
			return false
		}
		return true
	})
	title = strings.TrimSpace(title)
	if title == "" {
		return
	}
	s.mu.Lock()
	s.threadTitle = title
	s.mu.Unlock()
	log.Infof("[RIVET %d] thread title: %q", s.connID, title)

	_ = writeFrameRaw(map[string]any{
		"type":  "thread_title",
		"title": title,
	})
}

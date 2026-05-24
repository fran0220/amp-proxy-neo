package adminbase

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	"github.com/fran0220/amp-proxy-neo/pkg/identity"
)

// handleThreadsList GET /api/threads?limit=50 — proxies listThreads to Amp.
func (s *AdminServer) handleThreadsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := jsonNumber(l); err == nil {
			limit = int(n)
		}
	}
	body, _ := json.Marshal(map[string]any{
		"method": "listThreads",
		"params": map[string]any{"limit": limit},
	})
	resp, err := callAmpInternal(r.Context(), s.cfg, "listThreads", body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Repackage to {threads: [...]} for Browser convenience.
	var envelope struct {
		Result struct {
			Threads []any `json:"threads"`
		} `json:"result"`
	}
	_ = json.Unmarshal(resp, &envelope)
	_ = json.NewEncoder(w).Encode(map[string]any{"threads": envelope.Result.Threads})
}

// handleThreadGet GET /api/threads/{id} — fetches full thread data.
func (s *AdminServer) handleThreadGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/threads/")
	if id == "" {
		http.Error(w, "missing thread id", http.StatusBadRequest)
		return
	}
	body, _ := json.Marshal(map[string]any{
		"method": "getThread",
		"params": map[string]any{"thread": id},
	})
	resp, err := callAmpInternal(r.Context(), s.cfg, "getThread", body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Unwrap to thread.data so Browser gets the raw message array.
	var envelope struct {
		Result struct {
			Thread struct {
				Data map[string]any `json:"data"`
			} `json:"thread"`
		} `json:"result"`
	}
	_ = json.Unmarshal(resp, &envelope)
	_ = json.NewEncoder(w).Encode(envelope.Result.Thread.Data)
}

// callAmpInternal makes a POST to /api/internal?<method> on the upstream Amp
// server using the configured API key. Used by the admin Thread API.
func callAmpInternal(ctx context.Context, cfg *Config, method string, body []byte) ([]byte, error) {
	base := strings.TrimRight(cfg.Amp.UpstreamURL, "/")
	endpoint := base + "/api/internal?" + url.QueryEscape(method)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Amp.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Amp.APIKey)
		req.Header.Set("X-Api-Key", cfg.Amp.APIKey)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, jsonError("amp returned %d: %s", resp.StatusCode, identity.TruncateStr(string(respBytes), 200))
	}
	return respBytes, nil
}

// tiny helpers — keep dependencies trivial
func jsonNumber(s string) (float64, error) {
	var n float64
	err := json.Unmarshal([]byte(s), &n)
	return n, err
}

func jsonError(format string, args ...any) error {
	return &simpleErr{msg: format, args: args}
}

type simpleErr struct {
	msg  string
	args []any
}

func (e *simpleErr) Error() string {
	if len(e.args) == 0 {
		return e.msg
	}
	// Use fmt.Sprintf via a small inline implementation to avoid importing fmt twice.
	out := e.msg
	for _, a := range e.args {
		out = strings.Replace(out, "%d", toString(a), 1)
		out = strings.Replace(out, "%s", toString(a), 1)
	}
	return out
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return jsonItoa(x)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func jsonItoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

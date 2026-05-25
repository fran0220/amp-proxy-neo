package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

type CustomOpenAIProbe struct {
	AvailableModels   []string `json:"available_models"`
	SupportsTools     bool     `json:"supports_tools"`
	SupportsStreaming bool     `json:"supports_streaming"`
	LatencyMS         int64    `json:"latency_ms"`
	EndpointKind      string   `json:"endpoint_kind"`
	Error             string   `json:"error,omitempty"`
}

type CustomOpenAIHandler struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
	mu      sync.Mutex
	probe   CustomOpenAIProbe
	probed  bool
}

func NewCustomOpenAIHandler(baseURL, apiKey string) *CustomOpenAIHandler {
	return &CustomOpenAIHandler{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Client: &http.Client{Timeout: 10 * time.Minute}}
}

func (h *CustomOpenAIHandler) Probe(ctx context.Context) CustomOpenAIProbe {
	h.mu.Lock()
	if h.probed {
		p := h.probe
		h.mu.Unlock()
		return p
	}
	h.mu.Unlock()
	p := ProbeCustomOpenAI(ctx, h.BaseURL, h.APIKey)
	h.mu.Lock()
	h.probe = p
	h.probed = true
	h.mu.Unlock()
	return p
}

func ProbeCustomOpenAI(ctx context.Context, baseURL, apiKey string) CustomOpenAIProbe {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	streamCh := make(chan bool, 1)
	toolsCh := make(chan bool, 1)
	p := CustomOpenAIProbe{EndpointKind: detectEndpointKind(baseURL)}

	req, _ := http.NewRequestWithContext(ctx, "GET", BuildOpenAIModelsURL(baseURL), nil)
	setOptionalBearer(req, apiKey)
	resp, err := client.Do(req)
	if err != nil {
		p.Error = err.Error()
	} else {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			p.Error = fmt.Sprintf("/v1/models HTTP %d", resp.StatusCode)
		} else {
			gjson.GetBytes(b, "data").ForEach(func(_, v gjson.Result) bool {
				if id := v.Get("id").String(); id != "" {
					p.AvailableModels = append(p.AvailableModels, id)
				}
				return true
			})
			sort.Strings(p.AvailableModels)
		}
	}
	probeModel := "__probe__"
	if len(p.AvailableModels) > 0 {
		probeModel = p.AvailableModels[0]
	}
	go func() {
		body, _ := json.Marshal(map[string]any{"model": probeModel, "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": true, "max_tokens": 1})
		req, _ := http.NewRequestWithContext(ctx, "POST", BuildOpenAIURL(baseURL, "/v1/chat/completions"), bytes.NewReader(body))
		setOptionalBearer(req, apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			streamCh <- false
			return
		}
		defer resp.Body.Close()
		streamCh <- resp.StatusCode < 500
	}()
	go func() {
		body, _ := json.Marshal(map[string]any{"model": probeModel, "messages": []map[string]any{{"role": "user", "content": "hi"}}, "tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "ping", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}, "max_tokens": 1})
		req, _ := http.NewRequestWithContext(ctx, "POST", BuildOpenAIURL(baseURL, "/v1/chat/completions"), bytes.NewReader(body))
		setOptionalBearer(req, apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			toolsCh <- false
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		toolsCh <- resp.StatusCode < 400
	}()

	p.LatencyMS = time.Since(start).Milliseconds()
	for i := 0; i < 2; i++ {
		select {
		case s := <-streamCh:
			p.SupportsStreaming = s
		case t := <-toolsCh:
			p.SupportsTools = t
		case <-ctx.Done():
			if p.Error == "" {
				p.Error = ctx.Err().Error()
			}
			return p
		}
	}
	p.LatencyMS = time.Since(start).Milliseconds()
	return p
}

func detectEndpointKind(baseURL string) string {
	s := strings.ToLower(baseURL)
	switch {
	case strings.Contains(s, "11434") || strings.Contains(s, "ollama"):
		return "ollama"
	case strings.Contains(s, "1234") || strings.Contains(s, "lmstudio"):
		return "lmstudio"
	case strings.Contains(s, "8000") || strings.Contains(s, "vllm"):
		return "vllm"
	default:
		return "generic"
	}
}

func setOptionalBearer(req *http.Request, apiKey string) {
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

type ChatStreamResult struct {
	Text     string
	ToolUses []ToolUse
}

func (h *CustomOpenAIHandler) StreamChatCompletion(ctx context.Context, body []byte, onText func(string)) (ChatStreamResult, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", BuildOpenAIURL(h.BaseURL, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return ChatStreamResult{}, err
	}
	setOptionalBearer(req, h.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := h.Client.Do(req)
	if err != nil {
		return ChatStreamResult{}, fmt.Errorf("custom OpenAI endpoint unreachable (%s): %w", h.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return ChatStreamResult{}, fmt.Errorf("custom OpenAI status %d from %s: %s", resp.StatusCode, h.BaseURL, string(b))
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(resp.Body)
		return parseNonStreamChat(b, onText)
	}
	return parseStreamChat(resp.Body, onText)
}

func parseNonStreamChat(b []byte, onText func(string)) (ChatStreamResult, error) {
	text := gjson.GetBytes(b, "choices.0.message.content").String()
	if text != "" && onText != nil {
		onText(text)
	}
	var uses []ToolUse
	gjson.GetBytes(b, "choices.0.message.tool_calls").ForEach(func(_, tc gjson.Result) bool { uses = append(uses, toolUseFromGJSON(tc)); return true })
	return ChatStreamResult{Text: text, ToolUses: uses}, nil
}

func parseStreamChat(r io.Reader, onText func(string)) (ChatStreamResult, error) {
	type acc struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*acc{}
	var text strings.Builder
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data: ")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		d := gjson.GetBytes(payload, "choices.0.delta.content").String()
		if d != "" {
			text.WriteString(d)
			if onText != nil {
				onText(d)
			}
		}
		gjson.GetBytes(payload, "choices.0.delta.tool_calls").ForEach(func(_, tc gjson.Result) bool {
			idx := int(tc.Get("index").Int())
			c := calls[idx]
			if c == nil {
				c = &acc{}
				calls[idx] = c
			}
			if id := tc.Get("id").String(); id != "" {
				c.id = id
			}
			if n := tc.Get("function.name").String(); n != "" {
				c.name = n
			}
			if a := tc.Get("function.arguments").String(); a != "" {
				c.args.WriteString(a)
			}
			return true
		})
	}
	if err := scanner.Err(); err != nil {
		return ChatStreamResult{}, err
	}
	var uses []ToolUse
	for _, c := range calls {
		var input map[string]any
		_ = json.Unmarshal([]byte(c.args.String()), &input)
		if input == nil {
			input = map[string]any{}
		}
		if c.name != "" {
			uses = append(uses, ToolUse{ID: c.id, Name: c.name, Input: input})
		}
	}
	return ChatStreamResult{Text: text.String(), ToolUses: uses}, nil
}

func toolUseFromGJSON(tc gjson.Result) ToolUse {
	var input map[string]any
	_ = json.Unmarshal([]byte(tc.Get("function.arguments").String()), &input)
	if input == nil {
		input = map[string]any{}
	}
	return ToolUse{ID: tc.Get("id").String(), Name: tc.Get("function.name").String(), Input: input}
}

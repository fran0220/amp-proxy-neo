package provider

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"

	. "github.com/fran0220/amp-proxy-neo/pkg/auth"
	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	. "github.com/fran0220/amp-proxy-neo/pkg/logger"
	. "github.com/fran0220/amp-proxy-neo/pkg/retry"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	CodexBaseURL       = "https://chatgpt.com/backend-api/codex"
	CodexOriginator    = "codex_app"
	CodexClientVersion = "1.0.0"
	CodexUserAgent     = "codex_app/1.0.0 (Mac OS 26.0.1; arm64)"
)

type CodexHandler struct {
	cfg     *Config
	retryer *Retryer
	client  *http.Client
	logger  *RequestLogger
}

func NewCodexHandler(cfg *Config, retryer *Retryer, logger *RequestLogger) *CodexHandler {
	return &CodexHandler{
		cfg:     cfg,
		retryer: retryer,
		client:  &http.Client{},
		logger:  logger,
	}
}

func (h *CodexHandler) Handle(w http.ResponseWriter, r *http.Request, body []byte, auth *ProviderAuth) {
	model := gjson.GetBytes(body, "model").String()

	// Modify body for Codex endpoint — strip unsupported parameters
	body, _ = sjson.SetBytes(body, "stream", true)
	body, _ = sjson.SetBytes(body, "store", false)
	// Codex backend maps "fast" → "priority"; it rejects "fast" with 400
	body, _ = sjson.SetBytes(body, "service_tier", "priority")
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "max_output_tokens")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}

	upstreamURL := CodexBaseURL + "/responses"

	sessionID := uuid.NewString()

	resp, err := h.retryer.Do(r.Context(), h.client, func() (*http.Request, error) {
		req, reqErr := http.NewRequest(r.Method, upstreamURL, bytes.NewReader(body))
		if reqErr != nil {
			return nil, reqErr
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+auth.Token)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Connection", "Keep-Alive")
		req.Header.Set("Originator", CodexOriginator)
		req.Header.Set("Version", CodexClientVersion)
		req.Header.Set("User-Agent", CodexUserAgent)
		req.Header.Set("Session_id", sessionID)

		// Account ID is stored in auth.Email for codex-file source
		if auth.Email != "" {
			req.Header.Set("Chatgpt-Account-Id", auth.Email)
		}

		return req, nil
	})
	if err != nil {
		log.Errorf("codex request failed: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"%s","type":"proxy_error"}}`, err.Error())))
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		usage := h.streamResponse(w, resp.Body)
		h.logger.RecordResult(model, resp.StatusCode, usage, 0, "", "", "")
	} else {
		respBody, _ := io.ReadAll(resp.Body)
		_, _ = w.Write(respBody)
		usage := ParseOpenAIUsage(respBody)
		errMsg := ""
		if resp.StatusCode >= 400 {
			errMsg = gjson.GetBytes(respBody, "error.message").String()
		}
		h.logger.RecordResult(model, resp.StatusCode, usage, 0, errMsg, "", string(respBody))
	}
}

func (h *CodexHandler) streamResponse(w http.ResponseWriter, body io.Reader) TokenUsage {
	flusher, ok := w.(http.Flusher)
	if !ok {
		data, _ := io.ReadAll(body)
		_, _ = w.Write(data)
		return ParseOpenAIUsage(data)
	}

	var outputItems [][]byte
	var usage TokenUsage
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()

		if !bytes.HasPrefix(line, []byte("data: ")) {
			_, _ = w.Write(line)
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
			continue
		}

		dataJSON := line[len("data: "):]
		eventType := gjson.GetBytes(dataJSON, "type").String()

		// Collect completed output items
		if eventType == "response.output_item.done" {
			item := gjson.GetBytes(dataJSON, "item").Raw
			if item != "" {
				outputItems = append(outputItems, []byte(item))
			}
		}

		// Patch response.completed if output is empty (Codex backend returns empty output[])
		if eventType == "response.completed" && len(outputItems) > 0 {
			outputArr := gjson.GetBytes(dataJSON, "response.output")
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

				patched, err := sjson.SetRawBytes(dataJSON, "response.output", buf.Bytes())
				if err == nil {
					dataJSON = patched
					log.Infof("[CODEX] patched response.completed with %d output items", len(outputItems))
				}
			}
		}

		// Track best non-zero usage from any data line
		if u := ParseOpenAIUsage(dataJSON); u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 {
			usage = u
		}

		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(dataJSON)
		_, _ = w.Write([]byte("\n"))
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		log.Warnf("codex SSE stream scan error: %v", err)
	}

	return usage
}

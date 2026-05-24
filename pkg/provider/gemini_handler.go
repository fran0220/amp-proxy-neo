package provider

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	. "github.com/fran0220/amp-proxy-neo/pkg/auth"
	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	. "github.com/fran0220/amp-proxy-neo/pkg/logger"
	. "github.com/fran0220/amp-proxy-neo/pkg/retry"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const geminiAPIBase = "https://generativelanguage.googleapis.com"

type GeminiHandler struct {
	cfg     *Config
	retryer *Retryer
	client  *http.Client
	logger  *RequestLogger
}

func NewGeminiHandler(cfg *Config, retryer *Retryer, logger *RequestLogger) *GeminiHandler {
	return &GeminiHandler{
		cfg:     cfg,
		retryer: retryer,
		client:  &http.Client{},
		logger:  logger,
	}
}

func (h *GeminiHandler) Handle(w http.ResponseWriter, r *http.Request, body []byte, auth *ProviderAuth) {
	baseURL := auth.BaseURL
	if baseURL == "" {
		baseURL = h.cfg.Gemini.BaseURL
	}
	if baseURL == "" {
		baseURL = geminiAPIBase
	}
	baseURL = strings.TrimRight(baseURL, "/")

	upstreamPath := extractGeminiPath(r.URL.Path)
	upstreamURL := baseURL + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	model := ExtractGeminiModel(r.URL.Path)
	isStream := strings.Contains(upstreamPath, "streamGenerateContent")

	// Add alt=sse for streaming requests
	if isStream {
		if strings.Contains(upstreamURL, "?") {
			upstreamURL += "&alt=sse"
		} else {
			upstreamURL += "?alt=sse"
		}
	}

	resp, err := h.retryer.Do(r.Context(), h.client, func() (*http.Request, error) {
		req, reqErr := http.NewRequest(r.Method, upstreamURL, bytes.NewReader(body))
		if reqErr != nil {
			return nil, reqErr
		}

		req.Header.Set("Content-Type", "application/json")
		if auth.AuthType == AuthGoogAPIKey {
			req.Header.Set("x-goog-api-key", auth.Token)
		} else {
			req.Header.Set("Authorization", "Bearer "+auth.Token)
		}
		req.Header.Set("Connection", "keep-alive")

		if isStream {
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req.Header.Set("Accept", "application/json")
		}

		return req, nil
	})
	if err != nil {
		log.Errorf("gemini request failed: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"%s","status":"PROXY_ERROR"}}`, err.Error())))
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if isStream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		usage := h.streamResponse(w, resp.Body)
		h.logger.RecordResult(model, resp.StatusCode, usage, 0, "", "", "")
	} else {
		respBody, _ := io.ReadAll(resp.Body)
		_, _ = w.Write(respBody)
		usage := ParseGeminiUsage(respBody)
		errMsg := ""
		if resp.StatusCode >= 400 {
			errMsg = gjson.GetBytes(respBody, "error.message").String()
		}
		h.logger.RecordResult(model, resp.StatusCode, usage, 0, errMsg, "", string(respBody))
	}
}

func (h *GeminiHandler) streamResponse(w http.ResponseWriter, body io.Reader) TokenUsage {
	flusher, ok := w.(http.Flusher)
	if !ok {
		data, _ := io.ReadAll(body)
		_, _ = w.Write(data)
		return ParseGeminiUsage(data)
	}

	var lastDataLine []byte
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data: ")) {
			lastDataLine = make([]byte, len(line))
			copy(lastDataLine, line)
		}
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		log.Warnf("gemini SSE stream scan error: %v", err)
	}

	if lastDataLine != nil {
		return ParseGeminiUsage(lastDataLine[len("data: "):])
	}
	return TokenUsage{}
}

// extractGeminiPath strips the /api/provider/google prefix and normalizes
// Vertex AI style paths to AI Studio style paths.
// e.g. /api/provider/google/v1beta1/publishers/google/models/gemini-3-flash:generateContent
//
//	-> /v1beta/models/gemini-3-flash:generateContent
func extractGeminiPath(path string) string {
	const prefix = "/api/provider/google"
	if strings.HasPrefix(path, prefix) {
		path = path[len(prefix):]
	}
	// Normalize Vertex AI path: /v1beta1/publishers/google/models/... -> /v1beta/models/...
	if idx := strings.Index(path, "/publishers/"); idx >= 0 {
		modelsIdx := strings.Index(path[idx:], "/models/")
		if modelsIdx >= 0 {
			path = "/v1beta" + path[idx+modelsIdx:]
		}
	}
	// Normalize version: v1beta1 -> v1beta
	if strings.HasPrefix(path, "/v1beta1/") {
		path = "/v1beta/" + path[len("/v1beta1/"):]
	}
	return path
}

// extractGeminiModel extracts the model name from a Gemini URL path.
// e.g. /api/provider/google/v1beta1/models/gemini-3-flash:generateContent -> gemini-3-flash
// e.g. /api/provider/google/v1beta1/publishers/google/models/gemini-3-flash:streamGenerateContent -> gemini-3-flash
func ExtractGeminiModel(path string) string {
	idx := strings.Index(path, "/models/")
	if idx < 0 {
		return ""
	}
	modelPart := path[idx+len("/models/"):]
	if colonIdx := strings.Index(modelPart, ":"); colonIdx > 0 {
		return modelPart[:colonIdx]
	}
	return modelPart
}

package provider

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	. "github.com/fran0220/amp-proxy-neo/pkg/auth"
	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	. "github.com/fran0220/amp-proxy-neo/pkg/logger"
	. "github.com/fran0220/amp-proxy-neo/pkg/retry"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codeAssistEndpoint = "https://cloudcode-pa.googleapis.com"
	codeAssistVersion  = "v1internal"
	geminiCLIVersion   = "0.0.151"
	geminiCLIUA        = "google-cloud-sdk gemini_cli/" + geminiCLIVersion + " (Linux)"
	geminiCLIAPIClient = "gemini-cli/" + geminiCLIVersion + " gl-python/3.12.9 grpc/1.73.1"
)

type GeminiCLIHandler struct {
	cfg     *Config
	retryer *Retryer
	client  *http.Client
	logger  *RequestLogger
}

func NewGeminiCLIHandler(cfg *Config, retryer *Retryer, logger *RequestLogger) *GeminiCLIHandler {
	return &GeminiCLIHandler{
		cfg:     cfg,
		retryer: retryer,
		client:  &http.Client{},
		logger:  logger,
	}
}

func (h *GeminiCLIHandler) Handle(w http.ResponseWriter, r *http.Request, body []byte, auth *ProviderAuth) {
	model := ExtractGeminiModel(r.URL.Path)
	isStream := strings.Contains(r.URL.Path, "streamGenerateContent")

	projectID := getGeminiProjectID(auth.Token)

	// Ensure all contents have a role (Cloud Code Assist requires it)
	contents := gjson.GetBytes(body, "contents")
	if contents.Exists() && contents.IsArray() {
		for i, c := range contents.Array() {
			if !c.Get("role").Exists() {
				body, _ = sjson.SetBytes(body, fmt.Sprintf("contents.%d.role", i), "user")
			}
		}
	}

	// Wrap the original Gemini API body into Cloud Code Assist format:
	// { "request": { ...original body... }, "model": "...", "project": "..." }
	wrapped, _ := sjson.SetRawBytes([]byte("{}"), "request", body)
	wrapped, _ = sjson.SetBytes(wrapped, "model", model)
	if projectID != "" {
		wrapped, _ = sjson.SetBytes(wrapped, "project", projectID)
	}

	// Determine action from URL path
	action := "generateContent"
	if isStream {
		action = "streamGenerateContent"
	}
	if strings.Contains(r.URL.Path, "countTokens") {
		action = "countTokens"
	}

	upstreamURL := fmt.Sprintf("%s/%s:%s", codeAssistEndpoint, codeAssistVersion, action)
	if isStream {
		upstreamURL += "?alt=sse"
	}

	resp, err := h.retryer.Do(r.Context(), h.client, func() (*http.Request, error) {
		req, reqErr := http.NewRequest(r.Method, upstreamURL, bytes.NewReader(wrapped))
		if reqErr != nil {
			return nil, reqErr
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+auth.Token)
		req.Header.Set("User-Agent", geminiCLIUA)
		req.Header.Set("X-Goog-Api-Client", geminiCLIAPIClient)
		req.Header.Set("Connection", "keep-alive")

		if isStream {
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req.Header.Set("Accept", "application/json")
		}

		return req, nil
	})
	if err != nil {
		log.Errorf("[GEMINI-CLI] request failed: %v", err)
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

func (h *GeminiCLIHandler) streamResponse(w http.ResponseWriter, body io.Reader) TokenUsage {
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
		log.Warnf("[GEMINI-CLI] SSE stream scan error: %v", err)
	}

	if lastDataLine != nil {
		return ParseGeminiUsage(lastDataLine[len("data: "):])
	}
	return TokenUsage{}
}

// discoverGeminiProjectID discovers the GCP project ID via the Cloud Code Assist
// loadCodeAssist API, using the provided OAuth bearer token. If the API returns a
// project, it is cached for reuse; otherwise falls back to empty string.
func discoverGeminiProjectID(token string) string {
	body := []byte(`{"metadata":{"ideType":"IDE_UNSPECIFIED","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}}`)
	url := fmt.Sprintf("%s/%s:loadCodeAssist", codeAssistEndpoint, codeAssistVersion)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Warnf("[GEMINI-CLI] project discovery request error: %v", err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", geminiCLIUA)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Warnf("[GEMINI-CLI] project discovery failed: %v", err)
		return ""
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Warnf("[GEMINI-CLI] project discovery returned %d: %s", resp.StatusCode, string(respBody))
		return ""
	}

	// Extract project ID from response
	projectID := gjson.GetBytes(respBody, "cloudaicompanionProject").String()
	if projectID == "" {
		projectID = gjson.GetBytes(respBody, "cloudaicompanionProject.id").String()
	}
	if projectID != "" {
		log.Infof("[GEMINI-CLI] discovered project ID: %s", projectID)
	}
	return projectID
}

var (
	geminiProjectID   string
	geminiProjectOnce sync.Once
)

// getGeminiProjectID returns the cached project ID, discovering it on first call.
func getGeminiProjectID(token string) string {
	geminiProjectOnce.Do(func() {
		geminiProjectID = discoverGeminiProjectID(token)
	})
	return geminiProjectID
}

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
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type OpenAIResponsesHandler struct {
	cfg     *Config
	retryer *Retryer
	client  *http.Client
	logger  *RequestLogger
}

func NewOpenAIResponsesHandler(cfg *Config, retryer *Retryer, logger *RequestLogger) *OpenAIResponsesHandler {
	return &OpenAIResponsesHandler{
		cfg:     cfg,
		retryer: retryer,
		client:  &http.Client{},
		logger:  logger,
	}
}

func (h *OpenAIResponsesHandler) Handle(w http.ResponseWriter, r *http.Request, body []byte, auth *ProviderAuth) {
	baseURL := auth.BaseURL
	if baseURL == "" {
		baseURL = h.cfg.OpenAI.BaseURL
	}
	upstreamURL := BuildOpenAIResponsesURL(baseURL)
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	model := gjson.GetBytes(body, "model").String()
	isStream := gjson.GetBytes(body, "stream").Bool()

	if !gjson.GetBytes(body, "service_tier").Exists() {
		body, _ = sjson.SetBytes(body, "service_tier", "fast")
	}

	log.Infof("[RESPONSES] %s model=%s stream=%v", r.Method, model, isStream)

	resp, err := h.retryer.Do(r.Context(), h.client, func() (*http.Request, error) {
		req, reqErr := http.NewRequest("POST", upstreamURL, bytes.NewReader(body))
		if reqErr != nil {
			return nil, reqErr
		}

		req.Header.Set("Authorization", "Bearer "+auth.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connection", "keep-alive")

		for _, hdr := range []string{"User-Agent", "X-Stainless-Lang", "X-Stainless-Runtime",
			"X-Stainless-Runtime-Version", "X-Stainless-Package-Version",
			"X-Stainless-Os", "X-Stainless-Arch", "X-Stainless-Retry-Count"} {
			if v := r.Header.Get(hdr); v != "" {
				req.Header.Set(hdr, v)
			}
		}

		if isStream {
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req.Header.Set("Accept", "application/json")
		}

		return req, nil
	})
	if err != nil {
		log.Errorf("[RESPONSES] request failed: %v", err)
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

	if isStream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
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

func (h *OpenAIResponsesHandler) streamResponse(w http.ResponseWriter, body io.Reader) TokenUsage {
	flusher, ok := w.(http.Flusher)
	if !ok {
		data, _ := io.ReadAll(body)
		_, _ = w.Write(data)
		return ParseOpenAIUsage(data)
	}

	// Collect output items from response.output_item.done events so we can
	// patch the response.completed event if the upstream (e.g. NewAPI) returns
	// an empty output array.
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

		// Patch response.completed if output is empty
		if eventType == "response.completed" && len(outputItems) > 0 {
			outputArr := gjson.GetBytes(dataJSON, "response.output")
			if outputArr.Exists() && outputArr.IsArray() && len(outputArr.Array()) == 0 {
				// Build the output array from collected items
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
					log.Infof("[RESPONSES] patched response.completed with %d output items", len(outputItems))
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
		log.Warnf("[RESPONSES] SSE stream scan error: %v", err)
	}

	return usage
}

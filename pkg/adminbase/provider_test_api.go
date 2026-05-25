package adminbase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fran0220/amp-proxy-neo/pkg/provider"
	"github.com/tidwall/gjson"
)

type TestResult struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Latency  int64  `json:"latency_ms"`
	Provider string `json:"provider"`
}

type DiscoveredModel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
}

func (s *AdminServer) handleLLMProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	writeJSON(w, provider.ProbeCustomOpenAI(ctx, req.BaseURL, req.APIKey))
}

func testAPIKey(provider, apiKey, baseURL string) TestResult {
	if apiKey == "" {
		return TestResult{Success: false, Message: "API key is empty"}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	start := time.Now()

	switch provider {
	case "anthropic", "claude":
		return testClaude(client, apiKey, baseURL, start)
	case "openai", "codex":
		return testOpenAI(client, apiKey, baseURL, start)
	case "google", "gemini":
		return testGemini(client, apiKey, baseURL, start)
	case "custom":
		return testOpenAI(client, apiKey, baseURL, start)
	default:
		return TestResult{Success: false, Message: "unknown provider: " + provider}
	}
}

func testClaude(client *http.Client, apiKey, baseURL string, start time.Time) TestResult {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	url := strings.TrimRight(baseURL, "/") + "/v1/messages"

	body := `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return TestResult{Success: false, Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return TestResult{Success: false, Message: err.Error(), Latency: time.Since(start).Milliseconds()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	latency := time.Since(start).Milliseconds()
	if resp.StatusCode == 200 {
		return TestResult{Success: true, Message: "OK", Latency: latency, Provider: "claude"}
	}
	if resp.StatusCode == 429 {
		return TestResult{Success: true, Message: "OK (rate limited)", Latency: latency, Provider: "claude"}
	}
	errMsg := gjson.GetBytes(respBody, "error.message").String()
	if errMsg == "" {
		errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return TestResult{Success: false, Message: errMsg, Latency: latency, Provider: "claude"}
}

func testOpenAI(client *http.Client, apiKey, baseURL string, start time.Time) TestResult {
	body := `{"model":"gpt-5.4","input":"ping","max_output_tokens":1}`
	req, err := http.NewRequest("POST", provider.BuildOpenAIResponsesURL(baseURL), strings.NewReader(body))
	if err != nil {
		return TestResult{Success: false, Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return TestResult{Success: false, Message: err.Error(), Latency: time.Since(start).Milliseconds()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	latency := time.Since(start).Milliseconds()
	if resp.StatusCode == 200 {
		return TestResult{Success: true, Message: "OK (gpt-5.4 responses)", Latency: latency, Provider: "openai"}
	}
	if resp.StatusCode == 429 {
		return TestResult{Success: true, Message: "OK (gpt-5.4 rate limited)", Latency: latency, Provider: "openai"}
	}
	errMsg := gjson.GetBytes(respBody, "error.message").String()
	if errMsg == "" {
		errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return TestResult{Success: false, Message: errMsg, Latency: latency, Provider: "openai"}
}

func testGemini(client *http.Client, apiKey, baseURL string, start time.Time) TestResult {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	url := strings.TrimRight(baseURL, "/") + "/v1beta/models?key=" + apiKey

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return TestResult{Success: false, Message: err.Error()}
	}

	resp, err := client.Do(req)
	if err != nil {
		return TestResult{Success: false, Message: err.Error(), Latency: time.Since(start).Milliseconds()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	latency := time.Since(start).Milliseconds()
	if resp.StatusCode == 200 {
		count := gjson.GetBytes(respBody, "models.#").Int()
		return TestResult{Success: true, Message: fmt.Sprintf("OK (%d models)", count), Latency: latency, Provider: "gemini"}
	}
	errMsg := gjson.GetBytes(respBody, "error.message").String()
	if errMsg == "" {
		errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return TestResult{Success: false, Message: errMsg, Latency: latency, Provider: "gemini"}
}

func discoverModels(provider, apiKey, baseURL string) []DiscoveredModel {
	if apiKey == "" {
		return nil
	}

	client := &http.Client{Timeout: 15 * time.Second}

	switch provider {
	case "openai", "codex", "custom":
		return discoverOpenAIModels(client, apiKey, baseURL)
	case "google", "gemini":
		return discoverGeminiModels(client, apiKey, baseURL)
	case "anthropic", "claude":
		return []DiscoveredModel{
			{ID: "claude-opus-4-6", Name: "Claude Opus 4"},
			{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4"},
			{ID: "claude-haiku-4-5-20251001", Name: "Claude Haiku 4.5"},
		}
	default:
		return nil
	}
}

func discoverOpenAIModels(client *http.Client, apiKey, baseURL string) []DiscoveredModel {
	req, _ := http.NewRequest("GET", provider.BuildOpenAIModelsURL(baseURL), nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil
	}

	var models []DiscoveredModel
	for _, m := range result.Data {
		models = append(models, DiscoveredModel{ID: m.ID, Name: m.ID, Provider: m.OwnedBy})
	}
	return models
}

func discoverGeminiModels(client *http.Client, apiKey, baseURL string) []DiscoveredModel {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	url := strings.TrimRight(baseURL, "/") + "/v1beta/models?key=" + apiKey

	req, _ := http.NewRequest("GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil
	}

	var models []DiscoveredModel
	for _, m := range result.Models {
		id := m.Name
		if strings.HasPrefix(id, "models/") {
			id = id[len("models/"):]
		}
		models = append(models, DiscoveredModel{ID: id, Name: m.DisplayName})
	}
	return models
}

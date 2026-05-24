package adminbase

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fran0220/amp-proxy-neo/pkg/provider"
)

func TestBuildOpenAIURLHandlesBaseURLWithV1(t *testing.T) {
	t.Parallel()

	if got := provider.BuildOpenAIURL("https://example.com/v1", "/v1/responses"); got != "https://example.com/v1/responses" {
		t.Fatalf("buildOpenAIURL duplicated /v1: got %q", got)
	}

	if got := provider.BuildOpenAIURL("https://example.com/openai/v1/", "/v1/chat/completions"); got != "https://example.com/openai/v1/chat/completions" {
		t.Fatalf("buildOpenAIURL produced wrong nested path: got %q", got)
	}
}

func TestTestOpenAIUsesGPT54ResponsesEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type: %q", got)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if payload["model"] != "gpt-5.4" {
			t.Fatalf("unexpected model: %#v", payload["model"])
		}
		if payload["input"] != "ping" {
			t.Fatalf("unexpected input: %#v", payload["input"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_123","model":"gpt-5.4","output":[]}`))
	}))
	defer server.Close()

	result := testOpenAI(server.Client(), "sk-test", server.URL+"/v1", time.Now())
	if !result.Success {
		t.Fatalf("expected success, got %#v", result)
	}
	if !strings.Contains(result.Message, "gpt-5.4") {
		t.Fatalf("expected gpt-5.4 in message, got %q", result.Message)
	}
}

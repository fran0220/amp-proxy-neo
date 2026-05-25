package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCustomOpenAIStreamChatCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"qwen2.5-coder:7b","owned_by":"ollama"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := NewCustomOpenAIHandler(srv.URL+"/v1", "")
	var streamed string
	res, err := h.StreamChatCompletion(context.Background(), []byte(`{"model":"qwen2.5-coder:7b","messages":[{"role":"user","content":"hi"}],"stream":true}`), func(s string) { streamed += s })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello" || streamed != "hello" {
		t.Fatalf("text=%q streamed=%q", res.Text, streamed)
	}
}

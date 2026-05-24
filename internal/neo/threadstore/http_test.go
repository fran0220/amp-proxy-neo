package threadstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHttpUploadGzip(t *testing.T) {
	store := newTestStore(t)
	handler := NewHandler(store)
	raw := readFixture(t)
	thread := parseTestThread(t, raw)
	body := uploadBody(t, raw)

	req := httptest.NewRequest(http.MethodPost, "/api/internal?uploadThread", gzipBody(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d body=%s", rec.Code, rec.Body.String())
	}
	out, err := store.GetThread(req.Context(), thread.ID)
	if err != nil {
		t.Fatalf("get stored thread: %v", err)
	}
	if !bytes.Equal(out.Raw, raw) {
		t.Fatalf("stored raw mismatch")
	}
}

func TestHttpGetEnvelope(t *testing.T) {
	store := newTestStore(t)
	handler := NewHandler(store)
	raw := readFixture(t)
	thread := parseTestThread(t, raw)
	if err := store.UploadThread(context.Background(), thread); err != nil {
		t.Fatalf("upload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/internal?getThread", bytes.NewReader(getBody(t, thread.ID)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Ok     bool `json:"ok"`
		Result struct {
			Thread struct {
				Title string          `json:"title"`
				Data  json.RawMessage `json:"data"`
			} `json:"thread"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if !env.Ok {
		t.Fatalf("ok=false")
	}
	if env.Result.Thread.Title != thread.Title {
		t.Fatalf("title = %q, want %q", env.Result.Thread.Title, thread.Title)
	}
	if !jsonEqual(env.Result.Thread.Data, raw) {
		t.Fatalf("data mismatch\nwant %s\ngot  %s", raw, env.Result.Thread.Data)
	}
	var data struct {
		Messages []json.RawMessage `json:"messages"`
		V        int               `json:"v"`
	}
	if err := json.Unmarshal(env.Result.Thread.Data, &data); err != nil {
		t.Fatalf("parse data: %v", err)
	}
	if data.V != thread.V || len(data.Messages) != len(thread.Messages) {
		t.Fatalf("data v/messages = %d/%d, want %d/%d", data.V, len(data.Messages), thread.V, len(thread.Messages))
	}
}

func uploadBody(t *testing.T, thread json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"method": "uploadThread",
		"params": map[string]any{
			"thread":          thread,
			"createdOnServer": true,
		},
	})
	if err != nil {
		t.Fatalf("marshal upload body: %v", err)
	}
	return body
}

func getBody(t *testing.T, id string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"method": "getThread",
		"params": map[string]any{"thread": id},
	})
	if err != nil {
		t.Fatalf("marshal get body: %v", err)
	}
	return body
}

func gzipBody(t *testing.T, body []byte) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}

func jsonEqual(a, b []byte) bool {
	var av any
	var bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && equalJSON(av, bv)
}

func equalJSON(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fran0220/amp-proxy-neo/internal/neo/threadstore"
	"github.com/fran0220/amp-proxy-neo/pkg/config"
	"github.com/gorilla/websocket"
)

func TestChatWSUpgradeSendMessage(t *testing.T) {
	t.Setenv("AMP_PROXY_NEO_STANDALONE_STUB_RESPONSE", "hello over websocket")
	store := newMemoryThreadStore()
	h := newChatWSHandler(config.DefaultConfig(), nil, nil, store)
	server := httptest.NewServer(h)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{"type": "send_message", "reqId": "req-1", "text": "tell me a joke", "agentMode": "smart"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	seenDelta := false
	seenDone := false
	deadline := time.After(3 * time.Second)
	for !seenDone {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for done; seenDelta=%v", seenDelta)
		default:
		}
		var frame map[string]any
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read: %v", err)
		}
		if frame["type"] == "delta" {
			seenDelta = true
		}
		if frame["type"] == "done" {
			seenDone = true
		}
	}
	if !seenDelta {
		t.Fatalf("did not receive delta frame")
	}
	if len(store.items) != 1 {
		t.Fatalf("stored threads = %d, want 1", len(store.items))
	}
}

type memoryThreadStore struct {
	mu    sync.Mutex
	items map[string]*threadstore.Thread
}

func newMemoryThreadStore() *memoryThreadStore {
	return &memoryThreadStore{items: make(map[string]*threadstore.Thread)}
}

func (s *memoryThreadStore) UploadThread(_ context.Context, thread *threadstore.Thread) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[thread.ID] = thread
	return nil
}

func (s *memoryThreadStore) GetThread(_ context.Context, id string) (*threadstore.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread := s.items[id]
	if thread == nil {
		return nil, threadstore.ErrNotFound
	}
	return thread, nil
}

func (s *memoryThreadStore) ListThreads(context.Context, threadstore.ListOptions) ([]*threadstore.ThreadSummary, error) {
	return nil, nil
}

func (s *memoryThreadStore) DeleteThread(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	return nil
}

func (s *memoryThreadStore) Close() error { return nil }

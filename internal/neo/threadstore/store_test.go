package threadstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUploadGetRoundTrip(t *testing.T) {
	store := newTestStore(t)
	raw := readFixture(t)
	thread := parseTestThread(t, raw)

	if err := store.UploadThread(context.Background(), thread); err != nil {
		t.Fatalf("upload: %v", err)
	}
	out, err := store.GetThread(context.Background(), thread.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(out.Raw, raw) {
		t.Fatalf("raw mismatch\nwant %s\ngot  %s", raw, out.Raw)
	}
	if len(out.Messages) != len(thread.Messages) {
		t.Fatalf("message count = %d, want %d", len(out.Messages), len(thread.Messages))
	}
	for i := range out.Messages {
		if !bytes.Equal(out.Messages[i], thread.Messages[i]) {
			t.Fatalf("message %d mismatch", i)
		}
	}
}

func TestVersionMonotonic(t *testing.T) {
	store := newTestStore(t)
	base := parseTestThread(t, readFixture(t))
	base.Raw = setThreadFields(t, base.Raw, map[string]any{"v": 2, "updatedAt": int64(200)})
	base = parseTestThread(t, base.Raw)
	if err := store.UploadThread(context.Background(), base); err != nil {
		t.Fatalf("upload v2: %v", err)
	}

	older := parseTestThread(t, setThreadFields(t, base.Raw, map[string]any{"v": 1, "updatedAt": int64(300)}))
	if err := store.UploadThread(context.Background(), older); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("upload v1 err = %v, want ErrVersionConflict", err)
	}
	out, err := store.GetThread(context.Background(), base.ID)
	if err != nil {
		t.Fatalf("get after conflict: %v", err)
	}
	if out.V != 2 || out.UpdatedAt != 200 {
		t.Fatalf("stored v=%d updated=%d, want v=2 updated=200", out.V, out.UpdatedAt)
	}
}

func TestListOrder(t *testing.T) {
	store := newTestStore(t)
	base := readFixture(t)
	for _, item := range []struct {
		id      string
		updated int64
	}{
		{id: "T-a", updated: 10},
		{id: "T-c", updated: 30},
		{id: "T-b", updated: 20},
	} {
		raw := setThreadFields(t, base, map[string]any{"id": item.id, "v": 1, "updatedAt": item.updated})
		if err := store.UploadThread(context.Background(), parseTestThread(t, raw)); err != nil {
			t.Fatalf("upload %s: %v", item.id, err)
		}
	}
	threads, err := store.ListThreads(context.Background(), ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := []string{threads[0].ID, threads[1].ID, threads[2].ID}
	want := []string{"T-c", "T-b", "T-a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestDelete(t *testing.T) {
	store := newTestStore(t)
	thread := parseTestThread(t, readFixture(t))
	if err := store.UploadThread(context.Background(), thread); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := store.DeleteThread(context.Background(), thread.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetThread(context.Background(), thread.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get err = %v, want ErrNotFound", err)
	}
}

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func readFixture(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/sample_thread.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func parseTestThread(t *testing.T, raw json.RawMessage) *Thread {
	t.Helper()
	thread, err := ParseThread(raw)
	if err != nil {
		t.Fatalf("parse thread: %v", err)
	}
	return thread
}

func setThreadFields(t *testing.T, raw json.RawMessage, fields map[string]any) json.RawMessage {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal thread: %v", err)
	}
	for k, v := range fields {
		obj[k] = v
	}
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal thread: %v", err)
	}
	return out
}

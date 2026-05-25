package threadstore

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExportImportThreadRoundTrip(t *testing.T) {
	src := newTestStore(t)
	raw := readFixture(t)
	thread := parseTestThread(t, raw)
	if err := src.UploadThread(context.Background(), thread); err != nil {
		t.Fatalf("upload: %v", err)
	}

	exported, err := ExportThread(src, thread.ID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	dst := newTestStore(t)
	imported, err := ImportFromReader(dst, bytes.NewReader(exported), "auto")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 1 {
		t.Fatalf("imported = %d, want 1", imported)
	}
	out, err := dst.GetThread(context.Background(), thread.ID)
	if err != nil {
		t.Fatalf("get imported: %v", err)
	}
	if !bytes.Equal(out.Raw, raw) {
		t.Fatalf("raw mismatch\nwant %s\ngot  %s", raw, out.Raw)
	}
}

func TestExportAllImportTarGzip(t *testing.T) {
	src := newTestStore(t)
	base := readFixture(t)
	for _, id := range []string{"T-one", "T-two"} {
		raw := setThreadFields(t, base, map[string]any{"id": id, "v": 1, "updatedAt": time.Now().UnixMilli()})
		if err := src.UploadThread(context.Background(), parseTestThread(t, raw)); err != nil {
			t.Fatalf("upload %s: %v", id, err)
		}
	}

	var buf bytes.Buffer
	if err := ExportAll(src, &buf); err != nil {
		t.Fatalf("export all: %v", err)
	}
	dst := newTestStore(t)
	imported, err := ImportFromReader(dst, bytes.NewReader(buf.Bytes()), "auto")
	if err != nil {
		t.Fatalf("import tar.gz: %v", err)
	}
	if imported != 2 {
		t.Fatalf("imported = %d, want 2", imported)
	}
}

func TestImportKeepsHigherVersion(t *testing.T) {
	store := newTestStore(t)
	base := readFixture(t)
	v2 := parseTestThread(t, setThreadFields(t, base, map[string]any{"id": "T-version", "v": 2, "updatedAt": int64(200)}))
	v1 := setThreadFields(t, base, map[string]any{"id": "T-version", "v": 1, "updatedAt": int64(300)})
	if err := store.UploadThread(context.Background(), v2); err != nil {
		t.Fatalf("upload v2: %v", err)
	}
	imported, err := ImportFromReader(store, bytes.NewReader(v1), "json")
	if err != nil {
		t.Fatalf("import v1: %v", err)
	}
	if imported != 0 {
		t.Fatalf("imported = %d, want 0", imported)
	}
	out, err := store.GetThread(context.Background(), "T-version")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.V != 2 || out.UpdatedAt != 200 {
		t.Fatalf("stored v=%d updated=%d, want v=2 updated=200", out.V, out.UpdatedAt)
	}
}

func TestExportThreadMessagesNDJSON(t *testing.T) {
	store := newTestStore(t)
	thread := parseTestThread(t, readFixture(t))
	if err := store.UploadThread(context.Background(), thread); err != nil {
		t.Fatalf("upload: %v", err)
	}
	var buf bytes.Buffer
	if err := ExportThreadMessagesNDJSON(store, thread.ID, &buf); err != nil {
		t.Fatalf("export ndjson: %v", err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatalf("invalid json line: %s", line)
		}
	}
}

func TestICloudSyncImportsHigherVersion(t *testing.T) {
	store := newTestStore(t)
	base := readFixture(t)
	local := parseTestThread(t, setThreadFields(t, base, map[string]any{"id": "T-cloud", "v": 1, "updatedAt": int64(100)}))
	cloud := setThreadFields(t, base, map[string]any{"id": "T-cloud", "v": 2, "updatedAt": int64(200)})
	if err := store.UploadThread(context.Background(), local); err != nil {
		t.Fatalf("upload local: %v", err)
	}
	cloudDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cloudDir, "T-cloud.json"), cloud, 0o644); err != nil {
		t.Fatalf("write cloud: %v", err)
	}
	syncStore := NewICloudSyncStore(store, cloudDir, filepath.Join(t.TempDir(), "sync-conflicts.log"))
	status, err := syncStore.ImportExisting(context.Background())
	if err != nil {
		t.Fatalf("import existing: %v", err)
	}
	if status.Count != 1 {
		t.Fatalf("status count = %d, want 1", status.Count)
	}
	out, err := store.GetThread(context.Background(), "T-cloud")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.V != 2 {
		t.Fatalf("v = %d, want 2", out.V)
	}
}

func TestICloudSyncEqualVersionUsesNewerMTime(t *testing.T) {
	store := newTestStore(t)
	base := readFixture(t)
	local := parseTestThread(t, setThreadFields(t, base, map[string]any{"id": "T-cloud-mtime", "v": 1, "updatedAt": int64(100), "title": "local"}))
	cloud := setThreadFields(t, base, map[string]any{"id": "T-cloud-mtime", "v": 1, "updatedAt": int64(100), "title": "cloud"})
	if err := store.UploadThread(context.Background(), local); err != nil {
		t.Fatalf("upload local: %v", err)
	}
	cloudDir := t.TempDir()
	cloudPath := filepath.Join(cloudDir, "T-cloud-mtime.json")
	if err := os.WriteFile(cloudPath, cloud, 0o644); err != nil {
		t.Fatalf("write cloud: %v", err)
	}
	newer := time.UnixMilli(100).Add(time.Minute)
	if err := os.Chtimes(cloudPath, newer, newer); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	syncStore := NewICloudSyncStore(store, cloudDir, filepath.Join(t.TempDir(), "sync-conflicts.log"))
	if _, err := syncStore.ImportExisting(context.Background()); err != nil {
		t.Fatalf("import existing: %v", err)
	}
	out, err := store.GetThread(context.Background(), "T-cloud-mtime")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.Title != "cloud" {
		t.Fatalf("title = %q, want cloud", out.Title)
	}
}

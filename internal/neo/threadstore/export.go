package threadstore

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const exportFormatVersion = 1

type exportManifest struct {
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exportedAt"`
	Count      int       `json:"count"`
}

func ExportThread(store Store, id string) ([]byte, error) {
	thread, err := store.GetThread(context.Background(), id)
	if err != nil {
		return nil, err
	}
	raw, err := thread.rawJSON()
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), raw...)
	out = append(out, '\n')
	return out, nil
}

func ExportThreadMessagesNDJSON(store Store, id string, w io.Writer) error {
	thread, err := store.GetThread(context.Background(), id)
	if err != nil {
		return err
	}
	for _, msg := range thread.Messages {
		if len(bytes.TrimSpace(msg)) == 0 {
			continue
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}

func ExportAll(store Store, w io.Writer) error {
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	summaries, err := store.ListThreads(context.Background(), ListOptions{Limit: int(^uint(0) >> 1)})
	if err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(exportManifest{Version: exportFormatVersion, ExportedAt: time.Now().UTC(), Count: len(summaries)}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeTarFile(tw, "manifest.json", append(manifest, '\n')); err != nil {
		return err
	}
	for _, summary := range summaries {
		data, err := ExportThread(store, summary.ID)
		if err != nil {
			return err
		}
		name := "threads/" + safeThreadFilename(summary.ID) + ".json"
		if err := writeTarFile(tw, name, data); err != nil {
			return err
		}
	}
	return nil
}

func ImportFromReader(store Store, r io.Reader, format string) (int, error) {
	br := bufio.NewReader(r)
	if format == "" || format == "auto" {
		peek, _ := br.Peek(3)
		trimmed, _ := br.Peek(512)
		switch {
		case len(peek) >= 2 && peek[0] == 0x1f && peek[1] == 0x8b:
			format = "tar.gz"
		case len(bytes.TrimSpace(trimmed)) > 0 && bytes.TrimSpace(trimmed)[0] == '{':
			format = "json"
		default:
			format = "ndjson"
		}
	}
	switch strings.ToLower(format) {
	case "tar.gz", "tgz", "tar":
		return importTarGzip(store, br)
	case "json", "thread-json":
		return importThreadJSON(store, br)
	case "ndjson", "jsonl":
		return importNDJSON(store, br)
	default:
		return 0, fmt.Errorf("unknown import format %q", format)
	}
}

func ImportThread(store Store, thread *Thread) (bool, error) {
	if thread == nil {
		return false, errors.New("nil thread")
	}
	if err := store.UploadThread(context.Background(), thread); err != nil {
		if errors.Is(err, ErrVersionConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func importTarGzip(store Store, r io.Reader) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	imported := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return imported, nil
		}
		if err != nil {
			return imported, err
		}
		if hdr.Typeflag != tar.TypeReg || !strings.HasPrefix(hdr.Name, "threads/") || !strings.HasSuffix(hdr.Name, ".json") {
			continue
		}
		thread, err := parseThreadFromReader(tr)
		if err != nil {
			return imported, fmt.Errorf("import %s: %w", hdr.Name, err)
		}
		ok, err := ImportThread(store, thread)
		if err != nil {
			return imported, err
		}
		if ok {
			imported++
		}
	}
}

func importThreadJSON(store Store, r io.Reader) (int, error) {
	thread, err := parseThreadFromReader(r)
	if err != nil {
		return 0, err
	}
	ok, err := ImportThread(store, thread)
	if err != nil || !ok {
		return 0, err
	}
	return 1, nil
}

func importNDJSON(store Store, r io.Reader) (int, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var messages []json.RawMessage
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		msg := append(json.RawMessage(nil), line...)
		if !json.Valid(msg) {
			return 0, fmt.Errorf("invalid ndjson line: %q", string(line))
		}
		messages = append(messages, msg)
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if len(messages) == 0 {
		return 0, nil
	}
	now := time.Now().UnixMilli()
	raw, err := json.Marshal(map[string]any{
		"id":            fmt.Sprintf("imported-%d", now),
		"v":             1,
		"created":       now,
		"updatedAt":     now,
		"title":         "Imported NDJSON",
		"nextMessageId": len(messages),
		"messages":      messages,
	})
	if err != nil {
		return 0, err
	}
	thread, err := ParseThread(raw)
	if err != nil {
		return 0, err
	}
	ok, err := ImportThread(store, thread)
	if err != nil || !ok {
		return 0, err
	}
	return 1, nil
}

func parseThreadFromReader(r io.Reader) (*Thread, error) {
	var raw json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, err
	}
	return ParseThread(raw)
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func safeThreadFilename(id string) string {
	id = filepath.Base(strings.TrimSpace(id))
	id = strings.ReplaceAll(id, string(filepath.Separator), "_")
	if id == "." || id == "" {
		return "thread"
	}
	return id
}

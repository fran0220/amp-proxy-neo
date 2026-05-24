package util

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// AMP_PROXY_DUMP_DIR — set to a directory path to enable per-request capture
// of /api/internal?<method> traffic and Rivet WS frames. Each session writes
// to <dir>/<startTs>/timeline.jsonl with one JSON line per event so the file
// can be parsed by `jq` or diffed across sessions.
//
// Set to empty to disable (default off — capture is only useful during RE).
func dumpDir() string {
	d := strings.TrimSpace(os.Getenv("AMP_PROXY_DUMP_DIR"))
	return d
}

var dumpSessionDir string
var dumpSessionOnce atomic.Bool

func dumpSession() string {
	if dumpDir() == "" {
		return ""
	}
	if dumpSessionOnce.CompareAndSwap(false, true) {
		ts := time.Now().Format("20060102-150405")
		dumpSessionDir = filepath.Join(dumpDir(), ts)
		_ = os.MkdirAll(dumpSessionDir, 0755)
		log.Infof("[DUMP] writing capture to %s", dumpSessionDir)
	}
	return dumpSessionDir
}

// dumpEvent appends one JSON event to the session timeline file. kind is
// "http_req" | "http_resp" | "ws_frame". payload is the event-specific data.
func dumpEvent(kind string, payload map[string]any) {
	dir := dumpSession()
	if dir == "" {
		return
	}
	evt := map[string]any{
		"ts":   time.Now().Format(time.RFC3339Nano),
		"kind": kind,
	}
	for k, v := range payload {
		evt[k] = v
	}
	line, err := json.Marshal(evt)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "timeline.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
	_, _ = f.Write([]byte("\n"))
}

// isInterestingHTTPPath reports whether an HTTP request path is one whose
// body content we want to capture (currently: amp internal API + threads HTTP).
func isInterestingHTTPPath(p string) bool {
	return strings.HasPrefix(p, "/api/internal") ||
		strings.HasPrefix(p, "/api/threads") ||
		strings.HasPrefix(p, "/api/telemetry")
}

// captureRequestBody reads and resets r.Body, returning the bytes if path is
// interesting. Empty result when capture is disabled or path uninteresting.
func CaptureRequestBody(r *http.Request) []byte {
	if dumpDir() == "" || !isInterestingHTTPPath(r.URL.Path) || r.Method != "POST" {
		return nil
	}
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warnf("[DUMP] failed to read request body for %s: %v", r.URL.Path, err)
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

// decodeMaybeGzip decompresses if Content-Encoding is gzip, else returns input.
func decodeMaybeGzip(body []byte, encoding string) []byte {
	if !strings.EqualFold(strings.TrimSpace(encoding), "gzip") {
		return body
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return body
	}
	return out
}

// dumpHTTPRequest writes a captured request body to the timeline.
func DumpHTTPRequest(r *http.Request, body []byte) {
	if len(body) == 0 {
		return
	}
	decoded := decodeMaybeGzip(body, r.Header.Get("Content-Encoding"))
	var parsed any
	if err := json.Unmarshal(decoded, &parsed); err != nil {
		parsed = string(decoded)
	}
	dumpEvent("http_req", map[string]any{
		"method":    r.Method,
		"path":      r.URL.Path,
		"raw_query": r.URL.RawQuery,
		"size":      len(decoded),
		"body":      parsed,
	})
}

// dumpHTTPResponse reads, resets and writes a captured response body. Only
// triggers for interesting paths (checked via request URL on the response).
func DumpHTTPResponse(resp *http.Response) error {
	if dumpDir() == "" {
		return nil
	}
	if resp.Request == nil || !isInterestingHTTPPath(resp.Request.URL.Path) {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	decoded := decodeMaybeGzip(body, resp.Header.Get("Content-Encoding"))
	var parsed any
	if err := json.Unmarshal(decoded, &parsed); err != nil {
		parsed = string(decoded)
	}
	dumpEvent("http_resp", map[string]any{
		"method": resp.Request.Method,
		"path":   resp.Request.URL.Path,
		"status": resp.StatusCode,
		"size":   len(decoded),
		"body":   parsed,
	})
	return nil
}

// dumpWSFrame writes a Rivet WS frame to the timeline (if dumping enabled).
// direction is ">" (client→upstream) or "<" (upstream→client).
func DumpWSFrame(connID uint64, direction string, msgType int, data []byte) {
	if dumpDir() == "" {
		return
	}
	if msgType == 1 { // websocket.TextMessage = 1
		var parsed any
		if err := json.Unmarshal(data, &parsed); err == nil {
			dumpEvent("ws_frame", map[string]any{
				"conn":      connID,
				"direction": direction,
				"size":      len(data),
				"frame":     parsed,
			})
			return
		}
	}
	dumpEvent("ws_frame", map[string]any{
		"conn":      connID,
		"direction": direction,
		"msg_type":  msgType,
		"size":      len(data),
		"binary":    true,
	})
}

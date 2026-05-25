package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fran0220/amp-proxy-neo/internal/neo/selfserve"
	"github.com/fran0220/amp-proxy-neo/internal/neo/threadstore"
	"github.com/fran0220/amp-proxy-neo/pkg/adminbase"
	"github.com/fran0220/amp-proxy-neo/pkg/logger"
	"github.com/fran0220/amp-proxy-neo/pkg/util"
	log "github.com/sirupsen/logrus"
)

func (s *appState) proxyMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/api/status", s.health)
	mux.HandleFunc("/api/internal", s.internal)
	mux.HandleFunc("/api/telemetry", s.telemetry)
	mux.HandleFunc("/api/v2/spans", s.telemetry)
	if s.cfg.SelfServe() {
		actors := selfserve.ActorsHandler()
		mux.Handle("/metadata", selfserve.MetadataHandler())
		mux.Handle("/actors", actors)
		mux.Handle("/actors/", actors)
		mux.Handle("/auth/", selfserve.AuthHandler(selfserve.AuthHandlerConfig{Dir: s.dir, UserID: s.cfg.Neo.UserID}))
		mux.HandleFunc("/", s.fallback)
		return mux
	}
	mux.HandleFunc("/metadata", s.rivetHTTP)
	mux.HandleFunc("/actors/", s.rivetHTTP)
	mux.HandleFunc("/", s.fallback)
	return mux
}

func (s *appState) adminMux(admin *adminbase.AdminServer) http.Handler {
	base := http.NewServeMux()
	admin.RegisterAPIWithoutStatus(base)
	base.HandleFunc("/api/status", s.adminStatus)
	base.HandleFunc("/api/threads", s.threadsList)
	base.HandleFunc("/api/threads/", s.thread)
	s.registerChatWS(base)
	base.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(mustSub(s.chat, "assets")))))
	base.HandleFunc("/", s.chatIndex)

	root := http.NewServeMux()
	root.HandleFunc("/api/update/check", s.updateCheck)
	root.Handle("/", base)
	return cors(root)
}

func (s *appState) internal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	rr := &recorder{h: http.Header{}}
	threadstore.NewHandler(s.threads).ServeHTTP(rr, r)
	if rr.code >= 200 && rr.code < 400 {
		rr.copyTo(w)
		return
	}
	if s.cfg.SelfServe() {
		// threadstore only knows uploadThread/getThread/listThreads/deleteThread —
		// every other action (getUserInfo, loadPlugins, getThreadLabels, etc.)
		// falls through to the self-serve stub router so the amp CLI can boot
		// without an ampcode.com round-trip.
		r.Body = io.NopCloser(bytes.NewReader(body))
		selfserve.InternalActionHandler(selfserve.InternalActionConfig{UserID: s.cfg.Neo.UserID}).ServeHTTP(w, r)
		return
	}
	if s.upstream != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.upstream.forward(w, r)
		return
	}
	rr.copyTo(w)
}

// telemetry accepts and discards amp CLI telemetry/spans in self-serve mode so
// the CLI does not log warnings or back-off. Upstream mode forwards instead.
func (s *appState) telemetry(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SelfServe() {
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if s.upstream != nil {
		s.upstream.forward(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *appState) fallback(w http.ResponseWriter, r *http.Request) {
	if s.gw.CanHandle(r) {
		s.gw.HandleWS(w, r)
		return
	}
	if s.gw.IsRivetPath(r.URL.Path) {
		s.gw.HandleHTTP(w, r)
		return
	}
	if s.upstream != nil {
		s.upstream.forward(w, r)
		return
	}
	writeJSON(w, 501, map[string]string{"error": "upstream_disabled"})
}
func (s *appState) rivetHTTP(w http.ResponseWriter, r *http.Request) {
	if s.gw.CanHandle(r) {
		s.gw.HandleWS(w, r)
	} else {
		s.gw.HandleHTTP(w, r)
	}
}
func (s *appState) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "app": appName, "version": util.BuildVersion})
}
func (s *appState) adminStatus(w http.ResponseWriter, r *http.Request) {
	lastCheck, updateAvailable, latest := s.updater.LastCheck()
	writeJSON(w, 200, map[string]any{
		"app":               appName,
		"version":           util.BuildVersion,
		"commit":            util.BuildCommit,
		"build_date":        util.BuildDate,
		"channel":           s.cfg.Neo.Update.Channel,
		"last_update_check": lastCheck,
		"update_available":  updateAvailable,
		"latest_version":    latest,
		"uptime_sec":        int64(time.Since(s.started).Seconds()),
		"listen":            s.listen,
		"admin":             s.admin,
		"config_dir":        s.dir,
		"threads_db":        s.threadsDB,
		"logs_db":           s.logsDB,
	})
}

func (s *appState) updateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	info, err := s.updater.Check(r.Context())
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, info)
}

func (s *appState) threadsList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	items, err := s.threads.ListThreads(r.Context(), threadstore.ListOptions{Limit: limit})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"threads": items})
}

func (s *appState) thread(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/threads/")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "missing thread id"})
		return
	}
	if r.Method == http.MethodDelete {
		err := s.threads.DeleteThread(r.Context(), id)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, 200, map[string]bool{"ok": true})
		}
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	t, err := s.threads.GetThread(r.Context(), id)
	if errors.Is(err, threadstore.ErrNotFound) {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(t.Raw)
}

func (s *appState) chatIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/chat" && !strings.HasPrefix(r.URL.Path, "/chat/") {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	b, err := fs.ReadFile(s.chat, "index.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<h1>AMP Proxy Neo</h1><p>Chat UI is not built yet. Run <code>cd webui-react && npm run build</code> first.</p>"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

func mustNeoDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	dir := filepath.Join(home, ".amp-proxy-neo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Fatal(err)
	}
	return dir
}
func mustLog(path string) io.Writer {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return os.Stderr
	}
	return io.MultiWriter(os.Stderr, f)
}
func ensureNeoLogDB(dir string) {
	legacy := filepath.Join(dir, "amp-proxy.db")
	neo := filepath.Join(dir, "amp-proxy-neo.db")
	if _, err := os.Lstat(legacy); err == nil {
		if _, targetErr := os.Stat(neo); os.IsNotExist(targetErr) {
			_ = os.Rename(legacy, neo)
			_ = os.Symlink(neo, legacy)
		}
		return
	}
	_ = os.Symlink(neo, legacy)
}
func env(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
func start(name, addr string, h http.Handler) {
	log.Infof("%s listening on %s", name, addr)
	if err := http.ListenAndServe(addr, h); err != nil {
		log.Fatalf("%s server: %v", name, err)
	}
}
func flushLoop(l *logger.RequestLogger) {
	t := time.NewTicker(30 * time.Second)
	for range t.C {
		l.FlushPending()
	}
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return missingFS{}
	}
	return sub
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type recorder struct {
	h    http.Header
	code int
	body bytes.Buffer
}

func (r *recorder) Header() http.Header  { return r.h }
func (r *recorder) WriteHeader(code int) { r.code = code }
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.body.Write(b)
}
func (r *recorder) copyTo(w http.ResponseWriter) {
	for k, vs := range r.h {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if r.code == 0 {
		r.code = 200
	}
	w.WriteHeader(r.code)
	_, _ = w.Write(r.body.Bytes())
}

type upstreamProxy struct{ proxy *httputil.ReverseProxy }

func newUpstreamProxy(raw, key string) (*upstreamProxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream url: %w", err)
	}
	p := httputil.NewSingleHostReverseProxy(u)
	old := p.Director
	p.FlushInterval = -1
	p.Director = func(r *http.Request) {
		old(r)
		r.Host = u.Host
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
			r.Header.Set("X-Api-Key", key)
		}
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if !errors.Is(err, context.Canceled) {
			writeJSON(w, 502, map[string]string{"error": "upstream_proxy_error"})
		}
	}
	return &upstreamProxy{p}, nil
}
func (p *upstreamProxy) forward(w http.ResponseWriter, r *http.Request) { p.proxy.ServeHTTP(w, r) }

type missingFS struct{}

func (missingFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

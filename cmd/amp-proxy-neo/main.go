package main

import (
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
	"strings"
	"time"

	"github.com/fran0220/amp-proxy-neo/internal/neo/threadstore"
	"github.com/fran0220/amp-proxy-neo/pkg/adminbase"
	"github.com/fran0220/amp-proxy-neo/pkg/auth"
	"github.com/fran0220/amp-proxy-neo/pkg/config"
	"github.com/fran0220/amp-proxy-neo/pkg/logger"
	"github.com/fran0220/amp-proxy-neo/pkg/token"
	log "github.com/sirupsen/logrus"
)

const (
	appName    = "amp-proxy-neo"
	appVersion = "0.2.0-ready"
	defaultDir = ".amp-proxy-neo"
)

type appState struct {
	started     time.Time
	dir         string
	logPath     string
	chatDir     string
	listen      string
	admin       string
	cfg         *config.Config
	threadStore threadstore.Store
	upstream    *upstreamProxy
}

func main() {
	log.SetLevel(log.InfoLevel)

	dir, err := ensureNeoDir()
	if err != nil {
		log.Fatalf("create neo config dir: %v", err)
	}
	logPath := filepath.Join(dir, "proxy.log")
	logOutput, err := openProxyLogFile(logPath)
	if err != nil {
		log.SetOutput(os.Stderr)
		log.Errorf("open proxy log failed: %v", err)
	} else {
		log.SetOutput(logOutput)
	}

	cfg := loadNeoConfig(dir)
	cfg.Listen = envOrDefault("AMP_PROXY_NEO_LISTEN", cfg.Listen)
	adminAddr := envOrDefault("AMP_PROXY_NEO_ADMIN", ":9320")

	claudeProfiles := token.NewClaudeProfileManager(cfg)
	codexMgr := token.NewCodexTokenManager()
	geminiMgr := token.NewGeminiTokenManager()
	authResolver := auth.NewAuthResolver(cfg, claudeProfiles, codexMgr, geminiMgr)

	reqLogger := logger.NewRequestLoggerInDir(dir)
	defer reqLogger.Close()

	threads, err := threadstore.OpenSQLite(filepath.Join(dir, "threads.db"))
	if err != nil {
		log.Fatalf("open neo threadstore: %v", err)
	}
	defer threads.Close()

	var upstream *upstreamProxy
	if strings.TrimSpace(cfg.Amp.UpstreamURL) != "" {
		upstream, err = newUpstreamProxy(cfg.Amp.UpstreamURL, cfg.Amp.APIKey)
		if err != nil {
			log.Warnf("upstream disabled: %v", err)
		}
	}

	state := &appState{
		started:     time.Now(),
		dir:         dir,
		logPath:     logPath,
		chatDir:     findChatDistDir(),
		listen:      cfg.Listen,
		admin:       adminAddr,
		cfg:         cfg,
		threadStore: threads,
		upstream:    upstream,
	}

	adminServer := adminbase.NewAdminServer(cfg, claudeProfiles, reqLogger, authResolver, emptyWebFS{})
	go startServer("proxy", cfg.Listen, state.newProxyMux())
	go startServer("admin", adminAddr, state.newAdminMux(adminServer))

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			reqLogger.FlushPending()
		}
	}()

	log.Warn("rivet gateway not yet wired, WS upgrades will 501")
	log.Infof("%s ready proxy=%s admin=%s dir=%s", appName, cfg.Listen, adminAddr, dir)
	setupTray(adminAddr, logPath)
}

func ensureNeoDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, defaultDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func loadNeoConfig(dir string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Listen = ":9319"
	cfg.Amp.UpstreamURL = "https://ampcode.com"
	return config.LoadConfigFromDirWithDefault(dir, cfg)
}

func openProxyLogFile(path string) (io.Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return io.MultiWriter(os.Stderr, f), nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func startServer(name, addr string, handler http.Handler) {
	server := &http.Server{Addr: addr, Handler: handler}
	log.Infof("%s listening on %s", name, addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("%s server error: %v", name, err)
	}
}

func (s *appState) newProxyMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.Handle("/api/internal", threadstore.NewHandler(s.threadStore))
	mux.HandleFunc("/metadata", s.handleRivetHTTP)
	mux.HandleFunc("/actors/", s.handleRivetHTTP)
	mux.HandleFunc("/", s.handleProxyFallback)
	return mux
}

func (s *appState) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "app": appName, "version": appVersion, "mode": "ready"})
}

func (s *appState) handleRivetHTTP(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "rivet_gateway_pending", "hint": "Wave 2 PR3 internal/neo/rivet not wired yet"})
		return
	}
	if s.upstream != nil {
		s.upstream.forward(w, r)
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "self_serve_pending", "hint": "metadata and actors endpoints require upstream until Wave 3"})
}

func (s *appState) handleProxyFallback(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "rivet_gateway_pending", "hint": "Wave 2 PR3 internal/neo/rivet not wired yet"})
		return
	}
	if s.upstream != nil {
		s.upstream.forward(w, r)
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "upstream_disabled", "hint": "set amp.upstream-url or wait for Wave 3 self-serve mode"})
}

func (s *appState) newAdminMux(admin *adminbase.AdminServer) http.Handler {
	mux := http.NewServeMux()
	admin.RegisterAPIWithoutStatus(mux)
	mux.HandleFunc("/api/status", s.handleAdminStatus)
	mux.HandleFunc("/api/threads", s.handleThreadsList)
	mux.HandleFunc("/api/threads/", s.handleThread)
	mux.Handle("/assets/", s.chatAssetsHandler())
	mux.HandleFunc("/", s.handleChat)
	return corsMiddleware(mux)
}

func (s *appState) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app":        appName,
		"version":    appVersion,
		"mode":       "ready",
		"uptime_sec": int64(time.Since(s.started).Seconds()),
		"listen":     s.listen,
		"admin":      s.admin,
		"dir":        s.dir,
		"rivet":      "pending",
	})
}

func (s *appState) handleThreadsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var n int
		if err := json.Unmarshal([]byte(raw), &n); err == nil && n > 0 {
			limit = n
		}
	}
	threads, err := s.threadStore.ListThreads(r.Context(), threadstore.ListOptions{Limit: limit})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads})
}

func (s *appState) handleThread(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/threads/")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing thread id"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		thread, err := s.threadStore.GetThread(r.Context(), id)
		if errors.Is(err, threadstore.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(thread.Raw)
	case http.MethodDelete:
		if err := s.threadStore.DeleteThread(r.Context(), id); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
	}
}

func (s *appState) chatAssetsHandler() http.Handler {
	if s.chatDir == "" {
		return http.HandlerFunc(chatBuildMissing)
	}
	return http.FileServer(http.Dir(s.chatDir))
}

func (s *appState) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/chat" && !strings.HasPrefix(r.URL.Path, "/chat/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if s.chatDir == "" {
		chatBuildMissing(w, r)
		return
	}
	index, err := os.ReadFile(filepath.Join(s.chatDir, "index.html"))
	if err != nil {
		chatBuildMissing(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(index)
	}
}

func chatBuildMissing(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintln(w, "AMP Proxy Neo chat UI is not built yet. Run: cd webui-react && npm run build")
}

func findChatDistDir() string {
	if dir := strings.TrimSpace(os.Getenv("AMP_PROXY_NEO_CHAT_DIST")); dir != "" {
		if fileExists(filepath.Join(dir, "index.html")) {
			return dir
		}
		log.Warnf("AMP_PROXY_NEO_CHAT_DIST does not contain index.html: %s", dir)
	}
	if exe, err := os.Executable(); err == nil {
		appResourceDir := filepath.Clean(filepath.Join(filepath.Dir(exe), "..", "Resources", "webui-react", "dist"))
		if fileExists(filepath.Join(appResourceDir, "index.html")) {
			return appResourceDir
		}
	}
	candidates := []string{
		filepath.Join("webui-react", "dist"),
		filepath.Join("..", "..", "webui-react", "dist"),
		filepath.Join("..", "webui-react", "dist"),
	}
	for _, candidate := range candidates {
		if fileExists(filepath.Join(candidate, "index.html")) {
			return candidate
		}
	}
	log.Warn("webui-react/dist not found; chat UI will return build instructions")
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

type upstreamProxy struct {
	proxy *httputil.ReverseProxy
}

func newUpstreamProxy(upstreamURL, apiKey string) (*upstreamProxy, error) {
	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream url %q: %w", upstreamURL, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(parsed)
	originalDirector := proxy.Director
	proxy.FlushInterval = -1
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = parsed.Host
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("X-Api-Key", apiKey)
		}
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Errorf("neo upstream proxy error: %s %s: %v", req.Method, req.URL.Path, err)
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "upstream_proxy_error", "message": "failed to reach AMP upstream"})
	}
	return &upstreamProxy{proxy: proxy}, nil
}

func (p *upstreamProxy) forward(w http.ResponseWriter, r *http.Request) {
	p.proxy.ServeHTTP(w, r)
}

type emptyWebFS struct{}

func (emptyWebFS) Open(name string) (fs.File, error) {
	return nil, fs.ErrNotExist
}

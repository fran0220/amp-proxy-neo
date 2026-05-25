package threadstore

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

type Handler struct {
	store Store
}

func NewHandler(store Store) http.Handler {
	return &Handler{store: store}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := readJSONBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var env requestEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	method := internalMethod(r, env.Method)
	switch method {
	case "uploadThread":
		h.handleUploadThread(w, r, env.Params)
	case "getThread":
		h.handleGetThread(w, r, env.Params)
	case "listThreads":
		h.handleListThreads(w, r, env.Params)
	case "deleteThread":
		h.handleDeleteThread(w, r, env.Params)
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

type requestEnvelope struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (h *Handler) handleUploadThread(w http.ResponseWriter, r *http.Request, params json.RawMessage) {
	var p struct {
		Thread json.RawMessage `json:"thread"`
	}
	if err := json.Unmarshal(params, &p); err != nil || len(p.Thread) == 0 {
		http.Error(w, "missing thread", http.StatusBadRequest)
		return
	}
	thread, err := ParseThread(p.Thread)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.store.UploadThread(r.Context(), thread); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, ErrVersionConflict) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "result": map[string]any{}})
}

func (h *Handler) handleGetThread(w http.ResponseWriter, r *http.Request, params json.RawMessage) {
	id, err := threadIDParam(params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	thread, err := h.store.GetThread(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		// amp CLI shape: `if (!r.ok && r.error.code === "thread-not-found") return null`.
		// Return 200 with the error envelope so callers can branch on it.
		writeJSON(w, map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "thread-not-found", "message": err.Error()},
		})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"ok": true,
		"result": map[string]any{
			"thread": map[string]any{
				"title": thread.Title,
				"data":  thread.Raw,
			},
		},
	})
}

func (h *Handler) handleListThreads(w http.ResponseWriter, r *http.Request, params json.RawMessage) {
	var p struct {
		Limit int `json:"limit"`
	}
	_ = json.Unmarshal(params, &p)
	threads, err := h.store.ListThreads(r.Context(), ListOptions{Limit: p.Limit})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "result": map[string]any{"threads": threads}})
}

func (h *Handler) handleDeleteThread(w http.ResponseWriter, r *http.Request, params json.RawMessage) {
	id, err := threadIDParam(params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteThread(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "result": map[string]any{}})
}

func readJSONBody(r *http.Request) ([]byte, error) {
	reader := r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	defer r.Body.Close()
	return io.ReadAll(reader)
}

func internalMethod(r *http.Request, bodyMethod string) string {
	if r.URL.RawQuery != "" {
		for key := range r.URL.Query() {
			if key != "" {
				return key
			}
		}
		if !strings.Contains(r.URL.RawQuery, "=") {
			return r.URL.RawQuery
		}
	}
	return bodyMethod
}

func threadIDParam(params json.RawMessage) (string, error) {
	var p struct {
		Thread string `json:"thread"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", err
	}
	if p.Thread != "" {
		return p.Thread, nil
	}
	if p.ID != "" {
		return p.ID, nil
	}
	return "", errors.New("missing thread id")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("threadstore write json: %v", err)
	}
}

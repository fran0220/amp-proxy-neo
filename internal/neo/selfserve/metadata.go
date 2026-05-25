package selfserve

import (
	"encoding/json"
	"net/http"
	"time"
)

func MetadataHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
			return
		}
		// TODO: mirror all fields from a real /metadata capture when available.
		writeJSON(w, http.StatusOK, map[string]any{
			"actor_targets": []string{"thread_actor", "executor"},
			"region":        "local",
			"version":       "amp-proxy-neo-selfserve-1",
			"time":          time.Now().UTC().Format(time.RFC3339),
			"selfServe":     true,
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

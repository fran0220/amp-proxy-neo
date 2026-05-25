package selfserve

import (
	"encoding/json"
	"net/http"
)

// InternalActionConfig configures the /api/internal stub action router.
type InternalActionConfig struct {
	UserID string
}

// InternalActionHandler handles /api/internal actions in self-serve mode.
// It returns reasonable stub responses for actions the amp CLI calls during
// startup (getUserInfo, loadPlugins, etc.) and a permissive {ok:true,result:{}}
// fallback for actions we do not understand yet, so the CLI does not abort.
func InternalActionHandler(cfg InternalActionConfig) http.Handler {
	if cfg.UserID == "" {
		cfg.UserID = "local-user"
	}
	return &internalRouter{cfg: cfg}
}

type internalRouter struct {
	cfg InternalActionConfig
}

type internalEnvelope struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (h *internalRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
		return
	}
	var env internalEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_json"})
		return
	}
	switch env.Method {
	case "getUserInfo":
		h.getUserInfo(w)
	case "loadPlugins":
		// amp expects result to be an array: T.result.filter((a) => ...).
		writeJSON(w, http.StatusOK, ok([]any{}))
	case "getThreadLabels":
		writeJSON(w, http.StatusOK, ok([]any{}))
	case "setThreadLabels":
		writeJSON(w, http.StatusOK, ok(map[string]any{}))
	case "shareThreadWithOperator":
		writeJSON(w, http.StatusOK, ok(map[string]any{}))
	case "searchSubthreads":
		writeJSON(w, http.StatusOK, ok([]any{}))
	case "getWorkspaceInfo":
		writeJSON(w, http.StatusOK, ok(map[string]any{
			"workspace": nil,
			"groups":    []any{},
		}))
	default:
		// Permissive fallback so unknown actions do not abort the CLI startup.
		// We log nothing here on purpose to keep the noise down; callers can
		// inspect /api/logs/errors if a stub is too thin.
		writeJSON(w, http.StatusOK, ok(map[string]any{}))
	}
}

func (h *internalRouter) getUserInfo(w http.ResponseWriter) {
	// amp CLI accesses result.features, result.team, result.mysteriousMessage
	// directly and passes the whole result as the user object. So result IS
	// the user, not { user: {...} }. See amp-darwin-arm64 binary for the
	// `T?{user:T,features:T.features,workspace:T?.team,...}` mapping.
	writeJSON(w, http.StatusOK, ok(map[string]any{
		"id":               h.cfg.UserID,
		"username":         "local",
		"displayName":      "Local",
		"email":            h.cfg.UserID + "@amp-proxy-neo.local",
		"avatarURL":        "",
		"features":         map[string]any{},
		"team":             nil,
		"mysteriousMessage": "",
	}))
}

func ok(result any) map[string]any {
	return map[string]any{"ok": true, "result": result}
}

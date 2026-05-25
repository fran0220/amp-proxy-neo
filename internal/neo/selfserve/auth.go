package selfserve

import (
	"net/http"
	"time"
)

type AuthHandlerConfig struct {
	Dir    string
	UserID string
}

func AuthHandler(cfg AuthHandlerConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := cfg.UserID
		if userID == "" && cfg.Dir != "" {
			if id, err := LoadOrCreateUserID(cfg.Dir); err == nil {
				userID = id
			}
		}
		if userID == "" {
			userID = "user_00000000000000000000000000000000_account_00000000-0000-4000-8000-000000000000_session_00000000-0000-4000-8000-000000000000"
		}

		switch r.URL.Path {
		case "/auth/sign-in":
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
				return
			}
			token, err := signForAuth(cfg.Dir, userID)
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"user": defaultUser(userID), "warning": err.Error()})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "amp-session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(24 * time.Hour)})
			writeJSON(w, http.StatusOK, map[string]any{"user": defaultUser(userID)})
		case "/auth/sign-out":
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "amp-session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		case "/auth/user":
			if r.Method != http.MethodGet {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
				return
			}
			_, _ = r.Cookie("amp-session")
			writeJSON(w, http.StatusOK, map[string]any{"user": defaultUser(userID), "authenticated": true})
		default:
			writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "not_implemented", "message": "self-serve auth supports /auth/sign-in, /auth/sign-out, and /auth/user"})
		}
	})
}

func defaultUser(id string) map[string]any {
	return map[string]any{"id": id, "email": "local@amp-proxy-neo.invalid", "name": "Amp Proxy Neo"}
}

func signForAuth(dir, userID string) (string, error) {
	if dir == "" {
		return Sign("local-auth", "smart", userID)
	}
	secret, err := LoadOrCreateSecret(dir)
	if err != nil {
		return "", err
	}
	return SignWithSecret(secret, "local-auth", "smart", userID)
}

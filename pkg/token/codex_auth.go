package token

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	codexAuthFile      = ".codex/auth.json"
	codexTokenURL      = "https://auth.openai.com/oauth/token"
	codexClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexRefreshMargin = 5 * time.Minute
)

// codexCredentials mirrors the actual ~/.codex/auth.json structure.
// Tokens are nested: { "tokens": { "access_token": ..., "refresh_token": ... }, "OPENAI_API_KEY": ... }
type codexCredentials struct {
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
		IDToken      string `json:"id_token"`
	} `json:"tokens"`
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	AuthMode     string `json:"auth_mode"`
	LastRefresh  string `json:"last_refresh"`
}

type CodexTokenStatus struct {
	Valid     bool
	ExpiresIn time.Duration
	Email     string
	Error     error
}

type CodexTokenManager struct {
	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	email        string
	lastError    error
	sfGroup      singleflight.Group
	httpClient   *http.Client
}

func NewCodexTokenManager() *CodexTokenManager {
	tm := &CodexTokenManager{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}

	if err := tm.loadFromFile(); err != nil {
		log.Warnf("codex auth not available: %v", err)
		tm.lastError = err
	} else {
		log.Infof("loaded Codex OAuth token (email=%s, expires in %s)", tm.email, time.Until(tm.expiresAt).Round(time.Second))
		// Immediately try to refresh to get a valid token with real expiry
		if tm.refreshToken != "" {
			go func() {
				if err := tm.refresh(context.Background()); err != nil {
					log.Warnf("codex initial refresh failed (will retry): %v", err)
				}
			}()
		}
	}

	go tm.refreshLoop()
	return tm
}

func (tm *CodexTokenManager) GetAccessToken(ctx context.Context) (string, error) {
	tm.mu.RLock()
	token := tm.accessToken
	expiresAt := tm.expiresAt
	tm.mu.RUnlock()

	if token != "" && time.Now().Before(expiresAt.Add(-codexRefreshMargin)) {
		return token, nil
	}

	if err := tm.refresh(ctx); err != nil {
		if token != "" && time.Now().Before(expiresAt) {
			return token, nil
		}
		return "", fmt.Errorf("codex token expired and refresh failed: %w", err)
	}

	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.accessToken, nil
}

func (tm *CodexTokenManager) Status() CodexTokenStatus {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if tm.lastError != nil && tm.accessToken == "" {
		return CodexTokenStatus{Valid: false, Error: tm.lastError}
	}
	if tm.accessToken == "" {
		return CodexTokenStatus{Valid: false, Error: fmt.Errorf("no token loaded")}
	}
	remaining := time.Until(tm.expiresAt)
	if remaining <= 0 {
		return CodexTokenStatus{Valid: false, Error: fmt.Errorf("token expired"), Email: tm.email}
	}
	return CodexTokenStatus{Valid: true, ExpiresIn: remaining, Email: tm.email}
}

func codexAuthFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, codexAuthFile)
}

func (tm *CodexTokenManager) loadFromFile() error {
	data, err := os.ReadFile(codexAuthFilePath())
	if err != nil {
		return fmt.Errorf("read %s: %w", codexAuthFile, err)
	}

	var creds codexCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("parse %s: %w", codexAuthFile, err)
	}

	accessToken := creds.Tokens.AccessToken
	refreshToken := creds.Tokens.RefreshToken

	if accessToken == "" && refreshToken == "" {
		return fmt.Errorf("%s has no tokens (access_token and refresh_token both empty)", codexAuthFile)
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.accessToken = accessToken
	tm.refreshToken = refreshToken
	tm.email = creds.Tokens.AccountID
	// Codex tokens don't include expiry; assume short-lived, trigger refresh on use
	tm.expiresAt = time.Now().Add(30 * time.Minute)
	tm.lastError = nil
	return nil
}

func (tm *CodexTokenManager) refresh(ctx context.Context) error {
	_, err, _ := tm.sfGroup.Do("refresh", func() (interface{}, error) {
		tm.mu.RLock()
		refreshToken := tm.refreshToken
		tm.mu.RUnlock()

		if refreshToken == "" {
			return nil, fmt.Errorf("no refresh token available")
		}

		body := map[string]string{
			"client_id":     codexClientID,
			"grant_type":    "refresh_token",
			"refresh_token": refreshToken,
		}
		bodyJSON, _ := json.Marshal(body)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(string(bodyJSON)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := tm.httpClient.Do(req)
		if err != nil {
			tm.mu.Lock()
			tm.lastError = err
			tm.mu.Unlock()
			return nil, fmt.Errorf("codex refresh request failed: %w", err)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			errMsg := fmt.Errorf("codex refresh failed (status %d): %s", resp.StatusCode, string(respBody))
			tm.mu.Lock()
			tm.lastError = errMsg
			tm.mu.Unlock()
			return nil, errMsg
		}

		var result struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("parse codex refresh response: %w", err)
		}

		if result.AccessToken == "" {
			return nil, fmt.Errorf("codex refresh returned empty access token")
		}

		tm.mu.Lock()
		tm.accessToken = result.AccessToken
		if result.RefreshToken != "" {
			tm.refreshToken = result.RefreshToken
		}
		if result.ExpiresIn > 0 {
			tm.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
		} else {
			tm.expiresAt = time.Now().Add(1 * time.Hour)
		}
		tm.lastError = nil
		tm.mu.Unlock()

		log.Infof("codex token refreshed, expires in %ds", result.ExpiresIn)
		return nil, nil
	})
	return err
}

func (tm *CodexTokenManager) refreshLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		tm.mu.RLock()
		expiresAt := tm.expiresAt
		hasRefresh := tm.refreshToken != ""
		hasToken := tm.accessToken != ""
		tm.mu.RUnlock()

		if !hasRefresh || !hasToken {
			// No token loaded yet — try loading from file (user may have logged in via codex CLI)
			if err := tm.loadFromFile(); err == nil {
				log.Info("codex token loaded from file (was previously unavailable)")
			}
			continue
		}
		if time.Until(expiresAt) < codexRefreshMargin {
			log.Info("codex token approaching expiry, refreshing...")
			if err := tm.refresh(context.Background()); err != nil {
				log.Errorf("codex background refresh failed: %v", err)
				// Try reloading from file (user may have re-logged in via codex CLI)
				log.Info("reloading codex token from file...")
				if err2 := tm.loadFromFile(); err2 != nil {
					log.Errorf("codex file reload also failed: %v", err2)
				} else {
					log.Info("codex token reloaded from file")
				}
			}
		}
	}
}

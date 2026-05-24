package token

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fran0220/amp-proxy-neo/pkg/keychain"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	anthropicTokenURL  = "https://api.anthropic.com/v1/oauth/token"
	anthropicClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	tokenRefreshMargin = 5 * time.Minute
)

// TokenManager manages the lifecycle of Claude OAuth tokens.
// It reads from Keychain on startup and automatically refreshes before expiry.
// Each TokenManager is bound to a specific Keychain entry suffix so that
// multiple Claude profiles can coexist in the same process.
type TokenManager struct {
	keychainSuffix string
	mu             sync.RWMutex
	accessToken    string
	refreshToken   string
	expiresAt      time.Time
	lastRefresh    time.Time
	lastError      error
	sfGroup        singleflight.Group
	httpClient     *http.Client
}

// TokenStatus is a snapshot of token state for UI display.
type TokenStatus struct {
	Valid     bool
	ExpiresIn time.Duration
	Error     error
}

// NewTokenManager constructs a TokenManager bound to the default Keychain entry.
// Equivalent to NewTokenManagerForSuffix("").
func NewTokenManager() *TokenManager {
	return NewTokenManagerForSuffix("")
}

// NewTokenManagerForSuffix constructs a TokenManager bound to the Claude Code
// Keychain entry "Claude Code-credentials[-<suffix>]".
func NewTokenManagerForSuffix(suffix string) *TokenManager {
	tm := &TokenManager{
		keychainSuffix: suffix,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}

	label := keychain.KeychainServiceName(suffix)

	// Load initial credentials from Keychain
	if err := tm.loadFromKeychain(); err != nil {
		log.Warnf("[%s] failed to load OAuth token from Keychain: %v", label, err)
		tm.lastError = err
	} else {
		log.Infof("[%s] loaded OAuth token from Keychain (expires in %s)", label, time.Until(tm.expiresAt).Round(time.Second))
	}

	// Start background refresh loop
	go tm.refreshLoop()

	return tm
}

// GetAccessToken returns a valid access token, refreshing if necessary.
func (tm *TokenManager) GetAccessToken(ctx context.Context) (string, error) {
	tm.mu.RLock()
	token := tm.accessToken
	expiresAt := tm.expiresAt
	tm.mu.RUnlock()

	if token != "" && time.Now().Before(expiresAt.Add(-tokenRefreshMargin)) {
		return token, nil
	}

	// Token expired or about to expire — refresh
	if err := tm.refresh(ctx); err != nil {
		// If refresh fails but token is still technically valid, use it
		if token != "" && time.Now().Before(expiresAt) {
			log.Warnf("token refresh failed but token still valid: %v", err)
			return token, nil
		}
		return "", fmt.Errorf("token expired and refresh failed: %w", err)
	}

	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.accessToken, nil
}

// Status returns a snapshot for UI display.
func (tm *TokenManager) Status() TokenStatus {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if tm.lastError != nil && tm.accessToken == "" {
		return TokenStatus{Valid: false, Error: tm.lastError}
	}
	if tm.accessToken == "" {
		return TokenStatus{Valid: false, Error: fmt.Errorf("no token loaded")}
	}

	remaining := time.Until(tm.expiresAt)
	if remaining <= 0 {
		return TokenStatus{Valid: false, Error: fmt.Errorf("token expired")}
	}
	return TokenStatus{Valid: true, ExpiresIn: remaining}
}

// KeychainSuffix returns the Keychain entry suffix this manager is bound to.
func (tm *TokenManager) KeychainSuffix() string {
	if tm == nil {
		return ""
	}
	return tm.keychainSuffix
}

func (tm *TokenManager) loadFromKeychain() error {
	creds, err := keychain.ReadClaudeKeychainCredentials(tm.keychainSuffix)
	if err != nil {
		return err
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.accessToken = creds.AccessToken
	tm.refreshToken = creds.RefreshToken
	tm.expiresAt = time.UnixMilli(creds.ExpiresAt)
	tm.lastError = nil
	return nil
}

func (tm *TokenManager) refresh(ctx context.Context) error {
	_, err, _ := tm.sfGroup.Do("refresh", func() (interface{}, error) {
		tm.mu.RLock()
		refreshToken := tm.refreshToken
		tm.mu.RUnlock()

		if refreshToken == "" {
			return nil, fmt.Errorf("no refresh token available")
		}

		newAccess, newRefresh, expiresIn, err := tm.doRefresh(ctx, refreshToken)
		if err != nil {
			tm.mu.Lock()
			tm.lastError = err
			tm.mu.Unlock()
			return nil, err
		}

		tm.mu.Lock()
		tm.accessToken = newAccess
		if newRefresh != "" {
			tm.refreshToken = newRefresh
		}
		tm.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
		tm.lastRefresh = time.Now()
		tm.lastError = nil
		tm.mu.Unlock()

		log.Infof("token refreshed, expires in %ds", expiresIn)
		return nil, nil
	})
	return err
}

// doRefresh performs the actual OAuth token refresh HTTP request.
func (tm *TokenManager) doRefresh(ctx context.Context, refreshToken string) (accessToken, newRefreshToken string, expiresIn int, err error) {
	body := map[string]string{
		"client_id":     anthropicClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	}
	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicTokenURL, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := tm.httpClient.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", 0, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", "", 0, fmt.Errorf("refresh failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", 0, fmt.Errorf("parse refresh response: %w", err)
	}

	if result.AccessToken == "" {
		return "", "", 0, fmt.Errorf("refresh returned empty access token")
	}

	return result.AccessToken, result.RefreshToken, result.ExpiresIn, nil
}

// refreshLoop periodically checks and refreshes the token before expiry.
func (tm *TokenManager) refreshLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		tm.mu.RLock()
		expiresAt := tm.expiresAt
		hasRefresh := tm.refreshToken != ""
		tm.mu.RUnlock()

		if !hasRefresh {
			continue
		}

		remaining := time.Until(expiresAt)
		if remaining < tokenRefreshMargin {
			log.Info("token approaching expiry, refreshing...")
			if err := tm.refresh(context.Background()); err != nil {
				log.Errorf("background token refresh failed: %v", err)
				// Refresh token may be stale — try reloading from Keychain
				log.Info("reloading token from Keychain...")
				if err2 := tm.loadFromKeychain(); err2 != nil {
					log.Errorf("keychain reload also failed: %v", err2)
				} else {
					log.Info("token reloaded from Keychain")
				}
			}
		}
	}
}

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
	"syscall"
	"time"

	"github.com/fran0220/amp-proxy-neo/pkg/keychain"
	log "github.com/sirupsen/logrus"
)

const (
	anthropicTokenURL  = "https://api.anthropic.com/v1/oauth/token"
	anthropicClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	tokenRefreshMargin = 5 * time.Minute
	// keychainCacheTTL bounds how stale our in-memory snapshot may be before
	// we go back to the keychain. Short enough that a refresh by another
	// process (Claude.app, legacy amp-proxy) is picked up quickly; long
	// enough that we are not hitting the `security` CLI on every request.
	keychainCacheTTL = 5 * time.Second
)

// TokenManager exposes Claude OAuth tokens with the macOS Keychain as the
// single source of truth. There is no long-lived in-memory copy of the
// access/refresh token — every call freshly consults the keychain (cached for
// up to keychainCacheTTL to avoid `security` CLI overhead), and any refresh
// against Anthropic is wrapped in a per-suffix file lock so multiple processes
// (Claude.app, legacy amp-proxy, amp-proxy-neo) never race the rotated
// refresh token.
type TokenManager struct {
	keychainSuffix string
	httpClient     *http.Client

	mu        sync.Mutex // guards snapshot
	snapshot  *credsSnapshot
	snapAt    time.Time
	loadError error
}

type credsSnapshot struct {
	creds *keychain.KeychainCredentials
}

// TokenStatus is a snapshot of token state for UI display.
type TokenStatus struct {
	Valid     bool
	ExpiresIn time.Duration
	Error     error
}

// NewTokenManager constructs a TokenManager bound to the default Keychain entry.
func NewTokenManager() *TokenManager {
	return NewTokenManagerForSuffix("")
}

// NewTokenManagerForSuffix constructs a TokenManager bound to the Claude Code
// Keychain entry "Claude Code-credentials[-<suffix>]". No background goroutine
// is started — refresh happens lazily inside GetAccessToken.
func NewTokenManagerForSuffix(suffix string) *TokenManager {
	tm := &TokenManager{
		keychainSuffix: suffix,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
	label := keychain.KeychainServiceName(suffix)
	if creds, err := tm.readKeychainFresh(); err != nil {
		log.Warnf("[%s] failed to load OAuth token from Keychain: %v", label, err)
	} else {
		log.Infof("[%s] loaded OAuth token from Keychain (expires in %s)", label, time.Until(time.UnixMilli(creds.ExpiresAt)).Round(time.Second))
	}
	return tm
}

// GetAccessToken returns a non-expired access token, refreshing through the
// cross-process file lock if necessary. The returned token always reflects the
// latest keychain state at call time (within keychainCacheTTL).
func (tm *TokenManager) GetAccessToken(ctx context.Context) (string, error) {
	creds, err := tm.readKeychain()
	if err != nil {
		return "", err
	}
	expiresAt := time.UnixMilli(creds.ExpiresAt)
	if creds.AccessToken != "" && time.Now().Before(expiresAt.Add(-tokenRefreshMargin)) {
		return creds.AccessToken, nil
	}
	// Token expired or about to expire. Acquire the cross-process lock, then
	// re-check (some other process — including legacy amp-proxy or Claude.app —
	// may have refreshed while we were waiting), and only then refresh.
	unlock, err := tm.lock()
	if err != nil {
		return "", fmt.Errorf("acquire claude refresh lock: %w", err)
	}
	defer unlock()
	tm.invalidateCache()
	creds, err = tm.readKeychainFresh()
	if err == nil && creds.AccessToken != "" && time.Now().Before(time.UnixMilli(creds.ExpiresAt).Add(-tokenRefreshMargin)) {
		return creds.AccessToken, nil
	}
	if creds == nil || creds.RefreshToken == "" {
		if creds != nil && creds.AccessToken != "" && time.Now().Before(time.UnixMilli(creds.ExpiresAt)) {
			return creds.AccessToken, nil
		}
		return "", fmt.Errorf("no refresh token available")
	}
	updated, err := tm.doRefresh(ctx, creds)
	if err != nil {
		// Refresh failed but the existing access token may still be technically
		// valid — let the caller try it once before giving up.
		if creds.AccessToken != "" && time.Now().Before(time.UnixMilli(creds.ExpiresAt)) {
			log.Warnf("token refresh failed but token still valid: %v", err)
			return creds.AccessToken, nil
		}
		return "", fmt.Errorf("token expired and refresh failed: %w", err)
	}
	if writeErr := keychain.WriteClaudeKeychainCredentials(tm.keychainSuffix, updated); writeErr != nil {
		log.Warnf("[%s] keychain write-back failed: %v", keychain.KeychainServiceName(tm.keychainSuffix), writeErr)
	} else {
		log.Infof("[%s] keychain refreshed (expiresAt=%s)", keychain.KeychainServiceName(tm.keychainSuffix), time.UnixMilli(updated.ExpiresAt).Format(time.RFC3339))
	}
	tm.invalidateCache()
	return updated.AccessToken, nil
}

// Status returns a snapshot of the keychain-backed token state for UI.
func (tm *TokenManager) Status() TokenStatus {
	creds, err := tm.readKeychain()
	if err != nil {
		return TokenStatus{Valid: false, Error: err}
	}
	if creds.AccessToken == "" {
		return TokenStatus{Valid: false, Error: fmt.Errorf("no token loaded")}
	}
	remaining := time.Until(time.UnixMilli(creds.ExpiresAt))
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

// ReloadFromKeychain invalidates the in-memory cache and re-reads the
// keychain entry. Used by the tray "Reload Token" action and tests.
func (tm *TokenManager) ReloadFromKeychain() error {
	tm.invalidateCache()
	_, err := tm.readKeychainFresh()
	return err
}

// ForceRefresh runs an OAuth refresh under the cross-process lock and writes
// the rotated credentials back to keychain. Used by the admin
// `/api/token/refresh` endpoint.
func (tm *TokenManager) ForceRefresh(ctx context.Context) error {
	unlock, err := tm.lock()
	if err != nil {
		return err
	}
	defer unlock()
	tm.invalidateCache()
	current, err := tm.readKeychainFresh()
	if err != nil {
		return err
	}
	if current.RefreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}
	updated, err := tm.doRefresh(ctx, current)
	if err != nil {
		return err
	}
	if err := keychain.WriteClaudeKeychainCredentials(tm.keychainSuffix, updated); err != nil {
		return fmt.Errorf("keychain write-back: %w", err)
	}
	tm.invalidateCache()
	return nil
}

// readKeychain returns a cached snapshot (up to keychainCacheTTL old) to avoid
// shelling out to `security` on every single request.
func (tm *TokenManager) readKeychain() (*keychain.KeychainCredentials, error) {
	tm.mu.Lock()
	if tm.snapshot != nil && time.Since(tm.snapAt) < keychainCacheTTL {
		creds := tm.snapshot.creds
		tm.mu.Unlock()
		if creds != nil {
			return creds, nil
		}
	} else {
		tm.mu.Unlock()
	}
	return tm.readKeychainFresh()
}

func (tm *TokenManager) readKeychainFresh() (*keychain.KeychainCredentials, error) {
	creds, err := keychain.ReadClaudeKeychainCredentials(tm.keychainSuffix)
	tm.mu.Lock()
	if err != nil {
		tm.loadError = err
		tm.snapshot = nil
		tm.snapAt = time.Now()
		tm.mu.Unlock()
		return nil, err
	}
	tm.snapshot = &credsSnapshot{creds: creds}
	tm.snapAt = time.Now()
	tm.loadError = nil
	tm.mu.Unlock()
	return creds, nil
}

func (tm *TokenManager) invalidateCache() {
	tm.mu.Lock()
	tm.snapshot = nil
	tm.snapAt = time.Time{}
	tm.mu.Unlock()
}

// lock acquires a process-wide advisory lock on a file shared by all processes
// touching the same keychain suffix. Returns an unlock closure.
func (tm *TokenManager) lock() (func(), error) {
	dir := filepath.Join(os.TempDir(), "amp-proxy-claude-locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	suffix := tm.keychainSuffix
	if suffix == "" {
		suffix = "default"
	}
	path := filepath.Join(dir, "claude-"+suffix+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// doRefresh performs the actual OAuth token refresh HTTP request and returns
// a brand-new KeychainCredentials with rotated tokens + preserved metadata.
func (tm *TokenManager) doRefresh(ctx context.Context, current *keychain.KeychainCredentials) (*keychain.KeychainCredentials, error) {
	body := map[string]string{
		"client_id":     anthropicClientID,
		"grant_type":    "refresh_token",
		"refresh_token": current.RefreshToken,
	}
	bodyJSON, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicTokenURL, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := tm.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh failed (status %d): %s", resp.StatusCode, string(respBody))
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("refresh returned empty access token")
	}
	newRefresh := result.RefreshToken
	if newRefresh == "" {
		newRefresh = current.RefreshToken
	}
	return &keychain.KeychainCredentials{
		AccessToken:      result.AccessToken,
		RefreshToken:     newRefresh,
		ExpiresAt:        time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UnixMilli(),
		Scopes:           current.Scopes,
		SubscriptionType: current.SubscriptionType,
		RateLimitTier:    current.RateLimitTier,
	}, nil
}

package keychain

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

const keychainServiceBase = "Claude Code-credentials"

// KeychainCredentials represents the Claude Code OAuth credentials stored in macOS Keychain.
type KeychainCredentials struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // Unix milliseconds
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType"`
	RateLimitTier    string   `json:"rateLimitTier"`
}

// keychainWrapper is the outer JSON structure stored in Keychain.
type keychainWrapper struct {
	ClaudeAiOauth *KeychainCredentials `json:"claudeAiOauth"`
}

// keychainServiceName builds the macOS Keychain service name for a given suffix.
// Empty suffix → "Claude Code-credentials" (Claude Code's default profile).
// Non-empty suffix → "Claude Code-credentials-<suffix>" (additional profiles).
func KeychainServiceName(suffix string) string {
	suffix = strings.TrimSpace(suffix)
	suffix = strings.TrimPrefix(suffix, "-")
	if suffix == "" {
		return keychainServiceBase
	}
	return keychainServiceBase + "-" + suffix
}

// ReadClaudeKeychainCredentials reads Claude Code OAuth credentials from macOS Keychain.
// Pass an empty suffix to read the default Claude Code credential entry, or a
// non-empty suffix (e.g. "f61a06c7") to read a profile-specific entry.
// Uses the `security` CLI tool to avoid CGO dependencies.
func ReadClaudeKeychainCredentials(suffix string) (*KeychainCredentials, error) {
	account, err := currentUsername()
	if err != nil {
		return nil, fmt.Errorf("get current user: %w", err)
	}

	service := KeychainServiceName(suffix)
	cmd := exec.Command("security", "find-generic-password",
		"-s", service,
		"-a", account,
		"-w",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("keychain read failed (service=%q, account=%q): %w", service, account, err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, fmt.Errorf("keychain entry is empty")
	}

	var wrapper keychainWrapper
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		return nil, fmt.Errorf("parse keychain JSON: %w", err)
	}

	if wrapper.ClaudeAiOauth == nil {
		return nil, fmt.Errorf("keychain entry missing claudeAiOauth field")
	}

	creds := wrapper.ClaudeAiOauth
	if creds.AccessToken == "" {
		return nil, fmt.Errorf("keychain entry has empty access token")
	}

	return creds, nil
}

// WriteClaudeKeychainCredentials persists Claude Code OAuth credentials back to
// the macOS Keychain entry identified by suffix. This lets multiple processes
// (Claude.app, legacy amp-proxy, amp-proxy-neo) share the latest rotated
// refresh token: whichever process refreshes successfully writes back, and the
// next cold-starting process picks up the fresh entry instead of the stale one.
//
// Uses `security add-generic-password -U` (update if exists) to avoid CGO.
func WriteClaudeKeychainCredentials(suffix string, creds *KeychainCredentials) error {
	if creds == nil || creds.AccessToken == "" {
		return fmt.Errorf("refusing to write empty credentials")
	}
	account, err := currentUsername()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}
	service := KeychainServiceName(suffix)
	payload, err := json.Marshal(keychainWrapper{ClaudeAiOauth: creds})
	if err != nil {
		return fmt.Errorf("marshal keychain payload: %w", err)
	}
	cmd := exec.Command("security", "add-generic-password",
		"-U",
		"-s", service,
		"-a", account,
		"-w", string(payload),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("keychain write failed (service=%q): %w: %s", service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func currentUsername() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

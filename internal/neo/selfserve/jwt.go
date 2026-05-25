package selfserve

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const jwtSecretFile = "jwt.key"

// LoadOrCreateSecret loads the self-serve HS256 JWT secret from dir/jwt.key,
// creating a new 32-byte random secret on first run. The persisted format is
// base64 text so it can be copied into config if a future UI needs that.
func LoadOrCreateSecret(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create selfserve dir: %w", err)
	}
	path := filepath.Join(dir, jwtSecretFile)
	if data, err := os.ReadFile(path); err == nil {
		secret, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, fmt.Errorf("decode jwt secret: %w", err)
		}
		if len(secret) == 0 {
			return nil, fmt.Errorf("jwt secret is empty")
		}
		_ = os.Chmod(path, 0o600)
		return secret, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read jwt secret: %w", err)
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate jwt secret: %w", err)
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(secret) + "\n")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return nil, fmt.Errorf("write jwt secret: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return secret, nil
}

// Sign creates the self-signed Rivet wsToken used by the local self-serve
// actor. Amp CLI currently reads the payload fields and does not validate the
// signature, but we still emit a valid HS256 JWT for future compatibility.
func Sign(threadID, agentMode, userID string) (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	secret, err := LoadOrCreateSecret(dir)
	if err != nil {
		return "", err
	}
	return SignWithSecret(secret, threadID, agentMode, userID)
}

// SignWithSecret is used by tests and handlers that already loaded the secret.
func SignWithSecret(secret []byte, threadID, agentMode, userID string) (string, error) {
	if len(secret) == 0 {
		return "", fmt.Errorf("missing jwt secret")
	}
	if agentMode == "" {
		agentMode = "smart"
	}
	now := time.Now().Unix()
	header := map[string]any{"alg": "HS256", "typ": "JWT"}
	payload := map[string]any{
		"tid": threadID,
		"oid": userID,
		"am":  agentMode,
		"sub": userID,
		"iss": "amp-proxy",
		"aud": "amp-dtw",
		"iat": now,
		"exp": now + int64(24*time.Hour/time.Second),
	}
	head, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

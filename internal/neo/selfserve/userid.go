package selfserve

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const userIDFile = "user-id"

var selfServeUserIDPattern = regexp.MustCompile(`^user_[0-9a-f]{32}_account_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}_session_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".amp-proxy-neo"), nil
}

// LoadOrCreateUserID persists a local Amp-shaped user id in dir/user-id.
func LoadOrCreateUserID(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create selfserve dir: %w", err)
	}
	path := filepath.Join(dir, userIDFile)
	if data, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(data))
		if selfServeUserIDPattern.MatchString(id) {
			_ = os.Chmod(path, 0o600)
			return id, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read user id: %w", err)
	}

	id := fmt.Sprintf("user_%s_account_%s_session_%s", randomHex(16), uuidV4(), uuidV4())
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write user id: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return id, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func uuidV4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func IsValidUserID(id string) bool {
	return selfServeUserIDPattern.MatchString(id)
}

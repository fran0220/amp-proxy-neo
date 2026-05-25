package selfserve

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJWTSignRoundTrip(t *testing.T) {
	secret, err := LoadOrCreateSecret(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSecret: %v", err)
	}
	token, err := SignWithSecret(secret, "T-test", "smart", "user_test")
	if err != nil {
		t.Fatalf("SignWithSecret: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token parts = %d", len(parts))
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	wantSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if parts[2] != wantSig {
		t.Fatalf("signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if claims["tid"] != "T-test" || claims["am"] != "smart" || claims["iss"] != "amp-proxy" || claims["aud"] != "amp-dtw" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
}

func TestUserIDPersistence(t *testing.T) {
	dir := t.TempDir()
	id1, err := LoadOrCreateUserID(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreateUserID: %v", err)
	}
	if !IsValidUserID(id1) {
		t.Fatalf("invalid generated user id: %s", id1)
	}
	id2, err := LoadOrCreateUserID(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateUserID: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("user id not persisted: %s != %s", id1, id2)
	}
	assertMode0600(t, filepath.Join(dir, userIDFile))
}

func TestMetadataHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	MetadataHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metadata", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["region"] != "local" {
		t.Fatalf("region = %#v", body["region"])
	}
	if targets, ok := body["actor_targets"].([]any); !ok || len(targets) < 2 {
		t.Fatalf("actor_targets = %#v", body["actor_targets"])
	}
}

func TestActorsCreateReturnsActorID(t *testing.T) {
	rec := httptest.NewRecorder()
	ActorsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/actors", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["actorId"] == "" {
		t.Fatalf("missing actorId: %#v", body)
	}
}

func TestAuthSignInSetsCookie(t *testing.T) {
	dir := t.TempDir()
	userID, err := LoadOrCreateUserID(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	AuthHandler(AuthHandlerConfig{Dir: dir, UserID: userID}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/sign-in", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Result().Cookies(); len(got) == 0 || got[0].Name != "amp-session" || got[0].Value == "" {
		t.Fatalf("missing amp-session cookie: %#v", got)
	}
	assertMode0600(t, filepath.Join(dir, jwtSecretFile))
}

func TestBootstrapSynthesize(t *testing.T) {
	frames := SynthesizeStartupFrames("T-test", "smart", "user_test")
	if len(frames) < 2 {
		t.Fatalf("frames len = %d", len(frames))
	}
	if frames[0]["type"] != "executor_connected" || frames[1]["type"] != "agent_state" {
		t.Fatalf("unexpected frames: %#v", frames)
	}
	if frames[0]["seq"] == frames[1]["seq"] {
		t.Fatalf("seq values should differ: %#v", frames)
	}
}

func TestSecretFileMode0600(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateSecret(dir); err != nil {
		t.Fatal(err)
	}
	assertMode0600(t, filepath.Join(dir, jwtSecretFile))
}

func assertMode0600(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %o, want 600", path, got)
	}
}

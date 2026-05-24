package token

import (
	"testing"

	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	"github.com/fran0220/amp-proxy-neo/pkg/keychain"
)

func TestKeychainServiceName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "Claude Code-credentials"},
		{"  ", "Claude Code-credentials"},
		{"f61a06c7", "Claude Code-credentials-f61a06c7"},
		{"-f61a06c7", "Claude Code-credentials-f61a06c7"},
	}
	for _, c := range cases {
		if got := keychain.KeychainServiceName(c.in); got != c.want {
			t.Errorf("keychainServiceName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClaudeProfileList_DefaultWhenEmpty(t *testing.T) {
	cfg := DefaultConfig()
	profiles := cfg.ClaudeProfileList()
	if len(profiles) != 1 {
		t.Fatalf("want 1 synthetic default profile, got %d", len(profiles))
	}
	if profiles[0].ID != DefaultClaudeProfileID {
		t.Errorf("synthetic profile ID = %q, want %q", profiles[0].ID, DefaultClaudeProfileID)
	}
	if profiles[0].KeychainSuffix != "" {
		t.Errorf("synthetic profile suffix should be empty, got %q", profiles[0].KeychainSuffix)
	}
}

func TestActiveClaudeProfile_FallbackWhenIDStale(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Claude.Profiles = []ClaudeProfile{
		{ID: "personal", Label: "Personal", KeychainSuffix: ""},
		{ID: "corp", Label: "Corp", KeychainSuffix: "abc12345"},
	}
	cfg.Claude.ActiveProfile = "ghost-id-not-in-list"

	got := cfg.ActiveClaudeProfile()
	if got.ID != "personal" {
		t.Errorf("stale active id should fall back to first profile, got %q", got.ID)
	}
}

func TestSetActiveClaudeProfile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Claude.Profiles = []ClaudeProfile{
		{ID: "personal", Label: "Personal"},
		{ID: "corp", Label: "Corp", KeychainSuffix: "abc12345"},
	}

	if !cfg.SetActiveClaudeProfile("corp") {
		t.Fatal("SetActiveClaudeProfile(corp) returned false")
	}
	if got := cfg.ActiveClaudeProfile().ID; got != "corp" {
		t.Errorf("active = %q, want corp", got)
	}

	if cfg.SetActiveClaudeProfile("nonexistent") {
		t.Error("SetActiveClaudeProfile(nonexistent) should return false")
	}
	if got := cfg.ActiveClaudeProfile().ID; got != "corp" {
		t.Errorf("after failed switch active = %q, want corp", got)
	}
}

func TestProfileLabel(t *testing.T) {
	cases := []struct {
		p    ClaudeProfile
		want string
	}{
		{ClaudeProfile{ID: "personal", Label: "Personal Max"}, "Personal Max"},
		{ClaudeProfile{ID: "personal"}, "personal"},
		{ClaudeProfile{}, "Default"},
	}
	for _, c := range cases {
		if got := ProfileLabel(c.p); got != c.want {
			t.Errorf("profileLabel(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}

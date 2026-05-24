package token

import (
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/fran0220/amp-proxy-neo/pkg/config"
	log "github.com/sirupsen/logrus"
)

// ClaudeProfileManager owns one TokenManager per configured Claude profile and
// tracks which profile is currently active. The active profile is the one used
// for "local + keychain" routed Claude requests; switching it takes effect on
// the next request without restarting the proxy.
//
// When the config has no profiles, a single synthetic "_default" profile is
// used and behavior matches the legacy single-token-manager path exactly.
type ClaudeProfileManager struct {
	cfg *Config

	mu       sync.Mutex
	managers map[string]*TokenManager // keyed by profile id
}

// ProfileStatus describes a single profile for UI / API exposure.
type ProfileStatus struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	KeychainSuffix string `json:"keychain_suffix"`
	UserID         string `json:"user_id,omitempty"`
	Active         bool   `json:"active"`
	Valid          bool   `json:"valid"`
	ExpiresIn      string `json:"expires_in,omitempty"`
	Error          string `json:"error,omitempty"`
}

// NewClaudeProfileManager builds the manager and eagerly loads the active
// profile (so its token state is ready for the first request) plus any other
// profiles the user has configured.
func NewClaudeProfileManager(cfg *Config) *ClaudeProfileManager {
	pm := &ClaudeProfileManager{
		cfg:      cfg,
		managers: make(map[string]*TokenManager),
	}
	for _, p := range cfg.ClaudeProfileList() {
		pm.ensureManager(p)
	}
	active := cfg.ActiveClaudeProfile()
	log.Infof("[CLAUDE-PROFILE] active=%s (suffix=%q)", active.ID, active.KeychainSuffix)
	return pm
}

// ensureManager constructs a TokenManager for the given profile if missing.
func (pm *ClaudeProfileManager) ensureManager(p ClaudeProfile) *TokenManager {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if mgr, ok := pm.managers[p.ID]; ok {
		return mgr
	}
	mgr := NewTokenManagerForSuffix(p.KeychainSuffix)
	pm.managers[p.ID] = mgr
	return mgr
}

// Active returns the active profile and its TokenManager. Always returns a
// non-nil manager — falls back to the synthetic default profile if needed.
func (pm *ClaudeProfileManager) Active() (ClaudeProfile, *TokenManager) {
	p := pm.cfg.ActiveClaudeProfile()
	return p, pm.ensureManager(p)
}

// ActiveManager is a convenience accessor for code paths that don't need the
// profile metadata.
func (pm *ClaudeProfileManager) ActiveManager() *TokenManager {
	if pm == nil {
		return nil
	}
	_, mgr := pm.Active()
	return mgr
}

// Switch changes the active profile and persists the change. Returns an error
// if id is unknown.
func (pm *ClaudeProfileManager) Switch(id string) error {
	if !pm.cfg.SetActiveClaudeProfile(id) {
		return fmt.Errorf("unknown claude profile: %s", id)
	}
	if err := pm.cfg.Save(); err != nil {
		log.Warnf("[CLAUDE-PROFILE] failed to persist active profile: %v", err)
	}
	// Eagerly construct the manager so the next request doesn't pay the load cost.
	active := pm.cfg.ActiveClaudeProfile()
	pm.ensureManager(active)
	log.Infof("[CLAUDE-PROFILE] switched active profile → %s", active.ID)
	return nil
}

// ReloadActive forces a fresh Keychain read for the active profile (used by
// the tray "Reload Token" action when the user re-logged in via Claude Code).
func (pm *ClaudeProfileManager) ReloadActive() error {
	_, mgr := pm.Active()
	return mgr.loadFromKeychain()
}

// RefreshActive forces an OAuth refresh for the active profile.
func (pm *ClaudeProfileManager) RefreshActive(ctx context.Context) error {
	_, mgr := pm.Active()
	return mgr.refresh(ctx)
}

// List returns one ProfileStatus per configured profile (or a single synthetic
// default when nothing is configured).
func (pm *ClaudeProfileManager) List() []ProfileStatus {
	profiles := pm.cfg.ClaudeProfileList()
	active := pm.cfg.ActiveClaudeProfile()

	out := make([]ProfileStatus, 0, len(profiles))
	for _, p := range profiles {
		mgr := pm.ensureManager(p)
		st := mgr.Status()
		ps := ProfileStatus{
			ID:             p.ID,
			Label:          ProfileLabel(p),
			KeychainSuffix: p.KeychainSuffix,
			UserID:         p.UserID,
			Active:         p.ID == active.ID,
			Valid:          st.Valid,
		}
		if st.Valid {
			ps.ExpiresIn = st.ExpiresIn.Round(time.Second).String()
		}
		if st.Error != nil {
			ps.Error = st.Error.Error()
		}
		out = append(out, ps)
	}
	return out
}

func ProfileLabel(p ClaudeProfile) string {
	if p.Label != "" {
		return p.Label
	}
	if p.ID != "" {
		return p.ID
	}
	return "Default"
}

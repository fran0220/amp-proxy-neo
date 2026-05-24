package config

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fran0220/amp-proxy-neo/pkg/identity"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func GenerateID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// APIKeyEntry represents a single API key configuration for a provider.
type APIKeyEntry struct {
	ID      string `yaml:"id" json:"id"`
	Label   string `yaml:"label,omitempty" json:"label,omitempty"`
	APIKey  string `yaml:"api-key" json:"api_key"`
	BaseURL string `yaml:"base-url,omitempty" json:"base_url,omitempty"`
}

// CustomProvider represents a custom OpenAI-compatible API provider.
type CustomProvider struct {
	ID      string        `yaml:"id" json:"id"`
	Name    string        `yaml:"name" json:"name"`
	BaseURL string        `yaml:"base-url" json:"base_url"`
	Entries []APIKeyEntry `yaml:"entries,omitempty" json:"entries,omitempty"`
	Models  []ModelEntry  `yaml:"models,omitempty" json:"models,omitempty"`
}

type Config struct {
	Mu             sync.RWMutex      `yaml:"-"`
	path           string            `yaml:"-"`
	Listen         string            `yaml:"listen"`
	UserID         string            `yaml:"user-id,omitempty"` // stable user ID for prompt caching
	Amp            AmpConfig         `yaml:"amp"`
	Claude         ClaudeConfig      `yaml:"claude"`
	OpenAI         OpenAIConfig      `yaml:"openai"`
	Gemini         GeminiConfig      `yaml:"gemini"`
	Custom         []CustomProvider  `yaml:"custom,omitempty"`
	ModelRedirects map[string]string `yaml:"model-redirects,omitempty"` // e.g. "claude-opus-4-6" -> "claude-opus-4-7"
	Retry          RetryConfig       `yaml:"retry"`
}

type AmpConfig struct {
	UpstreamURL string `yaml:"upstream-url"`
	APIKey      string `yaml:"api-key"`
	// RivetHost is the Amp Neo actor gateway hostname (default "actors.ampcode.com").
	RivetHost string `yaml:"rivet-host,omitempty"`
	// RivetUser / RivetPass are the Rivet ACL credentials used when the proxy
	// forwards Neo WS/HTTP traffic to the gateway. Read from RIVET_PUBLIC_ENDPOINT
	// env if blank.
	RivetUser string `yaml:"rivet-user,omitempty"`
	RivetPass string `yaml:"rivet-pass,omitempty"`
}

type ClaudeConfig struct {
	Source        string          `yaml:"source"`                   // "keychain" or "manual"
	AuthToken     string          `yaml:"auth-token,omitempty"`     // manual Claude OAuth access token for local route
	APIKey        string          `yaml:"api-key"`                  // legacy single key
	Entries       []APIKeyEntry   `yaml:"entries,omitempty"`        // multiple keys
	Profiles      []ClaudeProfile `yaml:"profiles,omitempty"`       // multiple Claude Code Keychain profiles
	ActiveProfile string          `yaml:"active-profile,omitempty"` // currently active profile id
	Models        []ModelEntry    `yaml:"models"`
}

// ClaudeProfile represents a single Claude Code login (one Keychain entry +
// optional per-profile metadata user_id). Profiles let users isolate distinct
// Max accounts (e.g. personal + corp) on the same machine and switch between
// them at runtime without overlapping tokens or session metadata.
type ClaudeProfile struct {
	ID             string `yaml:"id" json:"id"`
	Label          string `yaml:"label,omitempty" json:"label,omitempty"`
	KeychainSuffix string `yaml:"keychain-suffix,omitempty" json:"keychain_suffix,omitempty"`
	UserID         string `yaml:"user-id,omitempty" json:"user_id,omitempty"`
}

type OpenAIConfig struct {
	APIKey  string        `yaml:"api-key"`
	BaseURL string        `yaml:"base-url"`
	Entries []APIKeyEntry `yaml:"entries,omitempty"`
	Models  []ModelEntry  `yaml:"models"`
}

type GeminiConfig struct {
	APIKey  string        `yaml:"api-key"`
	BaseURL string        `yaml:"base-url"`
	Entries []APIKeyEntry `yaml:"entries,omitempty"`
	Models  []ModelEntry  `yaml:"models"`
}

type ModelEntry struct {
	Name  string `yaml:"name" json:"name"`
	Route string `yaml:"route" json:"route"` // "amp", "local", "apikey"
}

type RetryConfig struct {
	MaxAttempts  int           `yaml:"max-attempts"`
	InitialDelay time.Duration `yaml:"initial-delay"`
}

func DefaultConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".amp-proxy")
}

func defaultConfigPath() string {
	return configPathInDir(DefaultConfigDir())

}

func configPathInDir(dir string) string {
	return filepath.Join(dir, "config.yaml")
}

func LoadConfig() *Config {
	return LoadConfigFromDir(DefaultConfigDir())
}

func LoadConfigFromDir(dir string) *Config {
	return LoadConfigFromDirWithDefault(dir, DefaultConfig())
}

func LoadConfigFromDirWithDefault(dir string, cfg *Config) *Config {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	cfgPath := configPathInDir(dir)
	cfg.path = cfgPath

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Infof("config not found at %s, creating default", cfgPath)
			if err := cfg.Save(); err != nil {
				log.Warnf("failed to save default config: %v", err)
			}
			return cfg
		}
		log.Warnf("failed to read config: %v, using defaults", err)
		return cfg
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		log.Warnf("failed to parse config: %v, using defaults", err)
		return DefaultConfig()
	}
	cfg.path = cfgPath

	// Merge: ensure all default models exist in config and remove invalid entries
	changed := mergeDefaults(cfg)

	// Generate stable user ID if not yet persisted (for prompt caching)
	if cfg.UserID == "" || !identity.IsValidClaudeUserID(cfg.UserID) {
		cfg.UserID = identity.GenerateClaudeUserID()
		changed = true
		log.Info("generated stable user ID for prompt caching")
	}

	if changed {
		log.Info("config migrated with updated models, saving")
		if err := cfg.Save(); err != nil {
			log.Warnf("failed to save migrated config: %v", err)
		}
	}

	return cfg
}

// mergeDefaults ensures all default models are present and removes invalid entries.
// Returns true if any changes were made.
func mergeDefaults(cfg *Config) bool {
	defaults := DefaultConfig()
	changed := false
	changed = mergeModelList(&cfg.Claude.Models, defaults.Claude.Models) || changed
	changed = mergeModelList(&cfg.OpenAI.Models, defaults.OpenAI.Models) || changed
	changed = mergeModelList(&cfg.Gemini.Models, defaults.Gemini.Models) || changed
	return changed
}

func mergeModelList(models *[]ModelEntry, defaults []ModelEntry) bool {
	changed := false

	// Remove invalid entries (empty or "undefined" names)
	var clean []ModelEntry
	for _, m := range *models {
		if m.Name == "" || m.Name == "undefined" {
			changed = true
			continue
		}
		clean = append(clean, m)
	}

	// Build set of existing model names
	existing := make(map[string]bool, len(clean))
	for _, m := range clean {
		existing[m.Name] = true
	}

	// Add missing defaults
	for _, d := range defaults {
		if !existing[d.Name] {
			clean = append(clean, d)
			changed = true
		}
	}

	// Fix empty routes (default to "amp")
	for i := range clean {
		if clean[i].Route == "" {
			clean[i].Route = "amp"
			changed = true
		}
	}

	*models = clean
	return changed
}

func (c *Config) Save() error {
	c.Mu.RLock()
	defer c.Mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	return os.WriteFile(c.path, data, 0o644)
}

func (c *Config) ClaudeLocalAuthConfig() (source, authToken string) {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	return c.Claude.Source, c.Claude.AuthToken
}

func (c *Config) ModelsForProvider(provider string) []ModelEntry {
	switch provider {
	case "anthropic", "claude":
		return c.Claude.Models
	case "openai", "codex":
		return c.OpenAI.Models
	case "google", "gemini":
		return c.Gemini.Models
	default:
		return nil
	}
}

func (c *Config) ModelsRefForProvider(provider string) *[]ModelEntry {
	switch provider {
	case "anthropic", "claude":
		return &c.Claude.Models
	case "openai", "codex":
		return &c.OpenAI.Models
	case "google", "gemini":
		return &c.Gemini.Models
	default:
		return nil
	}
}

// ModelRoute returns the route for a model ("amp", "local", "apikey").
// Unknown models default to "amp".
func (c *Config) ModelRoute(provider, model string) string {
	c.Mu.RLock()
	defer c.Mu.RUnlock()

	for _, m := range c.ModelsForProvider(provider) {
		if m.Name == model {
			if m.Route == "" {
				return "amp"
			}
			return m.Route
		}
	}
	return "amp"
}

// IsModelEnabled returns true if the model route is not "amp" (backward compat).
func (c *Config) IsModelEnabled(provider, model string) bool {
	return c.ModelRoute(provider, model) != "amp"
}

// SetModelRoute sets the route for a model.
func (c *Config) SetModelRoute(provider, model, route string) {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	models := c.ModelsRefForProvider(provider)
	if models == nil {
		return
	}

	for i, m := range *models {
		if m.Name == model {
			(*models)[i].Route = route
			return
		}
	}
	*models = append(*models, ModelEntry{Name: model, Route: route})
}

// SetModelEnabled is backward compat: enabled=true sets "local", enabled=false sets "amp".
func (c *Config) SetModelEnabled(provider, model string, enabled bool) {
	route := "amp"
	if enabled {
		route = "local"
	}
	c.SetModelRoute(provider, model, route)
}

// AllAPIKeysUnlocked returns all API key entries for a provider (caller must hold lock).
func (c *Config) AllAPIKeysUnlocked(provider string) []APIKeyEntry {
	var entries []APIKeyEntry
	switch provider {
	case "anthropic", "claude":
		entries = append(entries, c.Claude.Entries...)
		if c.Claude.APIKey != "" {
			found := false
			for _, e := range entries {
				if e.APIKey == c.Claude.APIKey {
					found = true
					break
				}
			}
			if !found {
				entries = append([]APIKeyEntry{{ID: "_legacy", Label: "Default", APIKey: c.Claude.APIKey}}, entries...)
			}
		}
	case "openai", "codex":
		entries = append(entries, c.OpenAI.Entries...)
		if c.OpenAI.APIKey != "" {
			found := false
			for _, e := range entries {
				if e.APIKey == c.OpenAI.APIKey {
					found = true
					break
				}
			}
			if !found {
				entries = append([]APIKeyEntry{{ID: "_legacy", Label: "Default", APIKey: c.OpenAI.APIKey, BaseURL: c.OpenAI.BaseURL}}, entries...)
			}
		}
	case "google", "gemini":
		entries = append(entries, c.Gemini.Entries...)
		if c.Gemini.APIKey != "" {
			found := false
			for _, e := range entries {
				if e.APIKey == c.Gemini.APIKey {
					found = true
					break
				}
			}
			if !found {
				entries = append([]APIKeyEntry{{ID: "_legacy", Label: "Default", APIKey: c.Gemini.APIKey, BaseURL: c.Gemini.BaseURL}}, entries...)
			}
		}
	}
	return entries
}

// AllAPIKeys returns all API key entries for a provider, including the legacy single key.
func (c *Config) AllAPIKeys(provider string) []APIKeyEntry {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	return c.AllAPIKeysUnlocked(provider)
}

func (c *Config) APIKey(provider, id string) (APIKeyEntry, bool) {
	c.Mu.RLock()
	defer c.Mu.RUnlock()

	for _, entry := range c.AllAPIKeysUnlocked(provider) {
		if entry.ID == id {
			return entry, true
		}
	}
	return APIKeyEntry{}, false
}

// PreferredAPIKey returns the API key entry that should be used by default.
// When multiple keys exist, prefer the most recently added explicit entry.
func (c *Config) PreferredAPIKey(provider string) (APIKeyEntry, bool) {
	c.Mu.RLock()
	defer c.Mu.RUnlock()

	entries := c.AllAPIKeysUnlocked(provider)
	if len(entries) == 0 {
		return APIKeyEntry{}, false
	}
	return entries[len(entries)-1], true
}

func (c *Config) UpdateAPIKey(provider, id, label, baseURL string, apiKey *string) bool {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	apply := func(entry *APIKeyEntry) {
		entry.Label = label
		entry.BaseURL = baseURL
		if apiKey != nil && *apiKey != "" {
			entry.APIKey = *apiKey
		}
	}

	switch provider {
	case "anthropic", "claude":
		if id == "_legacy" {
			if apiKey != nil && *apiKey != "" {
				c.Claude.APIKey = *apiKey
			}
			return true
		}
		for i := range c.Claude.Entries {
			if c.Claude.Entries[i].ID == id {
				apply(&c.Claude.Entries[i])
				return true
			}
		}
	case "openai", "codex":
		if id == "_legacy" {
			if apiKey != nil && *apiKey != "" {
				c.OpenAI.APIKey = *apiKey
			}
			c.OpenAI.BaseURL = baseURL
			return true
		}
		for i := range c.OpenAI.Entries {
			if c.OpenAI.Entries[i].ID == id {
				apply(&c.OpenAI.Entries[i])
				return true
			}
		}
	case "google", "gemini":
		if id == "_legacy" {
			if apiKey != nil && *apiKey != "" {
				c.Gemini.APIKey = *apiKey
			}
			c.Gemini.BaseURL = baseURL
			return true
		}
		for i := range c.Gemini.Entries {
			if c.Gemini.Entries[i].ID == id {
				apply(&c.Gemini.Entries[i])
				return true
			}
		}
	}

	return false
}

func (c *Config) CustomProvider(id string) (CustomProvider, bool) {
	c.Mu.RLock()
	defer c.Mu.RUnlock()

	for _, cp := range c.Custom {
		if cp.ID == id {
			return cp, true
		}
	}
	return CustomProvider{}, false
}

func (c *Config) UpdateCustomProvider(id, name, baseURL string, apiKey *string) bool {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	for i := range c.Custom {
		if c.Custom[i].ID != id {
			continue
		}
		c.Custom[i].Name = name
		c.Custom[i].BaseURL = baseURL
		if apiKey != nil && *apiKey != "" {
			if len(c.Custom[i].Entries) == 0 {
				c.Custom[i].Entries = []APIKeyEntry{{ID: GenerateID(), APIKey: *apiKey}}
			} else {
				c.Custom[i].Entries[0].APIKey = *apiKey
			}
		}
		return true
	}

	return false
}

// AddAPIKey adds an API key entry to a provider.
func (c *Config) AddAPIKey(provider string, entry APIKeyEntry) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if entry.ID == "" {
		entry.ID = GenerateID()
	}
	switch provider {
	case "anthropic", "claude":
		c.Claude.Entries = append(c.Claude.Entries, entry)
	case "openai", "codex":
		c.OpenAI.Entries = append(c.OpenAI.Entries, entry)
	case "google", "gemini":
		c.Gemini.Entries = append(c.Gemini.Entries, entry)
	}
}

// RemoveAPIKey removes an API key entry by ID.
func (c *Config) RemoveAPIKey(provider, id string) {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	remove := func(entries []APIKeyEntry) []APIKeyEntry {
		var result []APIKeyEntry
		for _, e := range entries {
			if e.ID != id {
				result = append(result, e)
			}
		}
		return result
	}

	switch provider {
	case "anthropic", "claude":
		if id == "_legacy" {
			c.Claude.APIKey = ""
			return
		}
		c.Claude.Entries = remove(c.Claude.Entries)
	case "openai", "codex":
		if id == "_legacy" {
			c.OpenAI.APIKey = ""
			c.OpenAI.BaseURL = ""
			return
		}
		c.OpenAI.Entries = remove(c.OpenAI.Entries)
	case "google", "gemini":
		if id == "_legacy" {
			c.Gemini.APIKey = ""
			c.Gemini.BaseURL = ""
			return
		}
		c.Gemini.Entries = remove(c.Gemini.Entries)
	}
}

// DefaultClaudeProfileID is the synthetic profile id used when no profiles are
// configured (so resolver / UI always have something to point at). It binds to
// the default Keychain entry "Claude Code-credentials".
const DefaultClaudeProfileID = "_default"

// ClaudeProfileList returns the configured profiles, or a single synthetic
// "_default" profile bound to the default Keychain entry when none are set.
// The returned slice is a copy and safe to mutate.
func (c *Config) ClaudeProfileList() []ClaudeProfile {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	return c.claudeProfileListUnlocked()
}

func (c *Config) claudeProfileListUnlocked() []ClaudeProfile {
	if len(c.Claude.Profiles) == 0 {
		return []ClaudeProfile{{
			ID:    DefaultClaudeProfileID,
			Label: "Default",
		}}
	}
	out := make([]ClaudeProfile, len(c.Claude.Profiles))
	copy(out, c.Claude.Profiles)
	return out
}

// ActiveClaudeProfile returns the currently active profile (or the default
// synthetic profile when none are configured / the active id is stale).
func (c *Config) ActiveClaudeProfile() ClaudeProfile {
	c.Mu.RLock()
	defer c.Mu.RUnlock()

	profiles := c.claudeProfileListUnlocked()
	active := c.Claude.ActiveProfile
	if active == "" {
		return profiles[0]
	}
	for _, p := range profiles {
		if p.ID == active {
			return p
		}
	}
	return profiles[0]
}

// SetActiveClaudeProfile changes the active profile. Returns false if the id
// is not present in the configured profile list.
func (c *Config) SetActiveClaudeProfile(id string) bool {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	for _, p := range c.claudeProfileListUnlocked() {
		if p.ID == id {
			c.Claude.ActiveProfile = id
			return true
		}
	}
	return false
}

// ResolveModelRedirect checks if a model has a redirect configured and returns the target model.
// Returns the original model if no redirect is configured.
func (c *Config) ResolveModelRedirect(model string) (target string, redirected bool) {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if c.ModelRedirects == nil {
		return model, false
	}
	if target, ok := c.ModelRedirects[model]; ok && target != "" {
		return target, true
	}
	return model, false
}

// SetModelRedirect sets or removes a model redirect.
func (c *Config) SetModelRedirect(from, to string) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if to == "" {
		delete(c.ModelRedirects, from)
		if len(c.ModelRedirects) == 0 {
			c.ModelRedirects = nil
		}
	} else {
		if c.ModelRedirects == nil {
			c.ModelRedirects = make(map[string]string)
		}
		c.ModelRedirects[from] = to
	}
}

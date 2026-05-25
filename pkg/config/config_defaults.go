package config

import "time"

// AmpModelRole describes a model's role in the Amp ecosystem.
type AmpModelRole struct {
	Model       string   `json:"model"`
	Provider    string   `json:"provider"`
	Role        string   `json:"role"`
	Description string   `json:"description"`
	Tiers       []string `json:"tiers"` // which auth sources support this model
}

// Model tiers: which auth sources can serve a model.
const (
	TierAmp    = "amp"    // always available via AMP upstream
	TierLocal  = "local"  // available via local CLI credentials
	TierAPIKey = "apikey" // available via API key
)

// AmpModelRoles lists all models used by Amp and their roles.
var AmpModelRoles = []AmpModelRole{
	{Model: "claude-opus-4-7", Provider: "anthropic", Role: "Smart", Description: "Latest state-of-the-art model", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "claude-opus-4-6", Provider: "anthropic", Role: "Smart", Description: "Unconstrained state-of-the-art model", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "claude-haiku-4-5-20251001", Provider: "anthropic", Role: "Rush / Titling", Description: "Fast and cheap for small tasks", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "claude-sonnet-4-6", Provider: "anthropic", Role: "Librarian", Description: "Large-scale retrieval & research", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "gpt-5.4", Provider: "openai", Role: "Deep / Oracle", Description: "Deep reasoning with extended thinking", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "gpt-5.5", Provider: "openai", Role: "Deep / Oracle", Description: "Next-gen deep reasoning", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "gpt-5.3-codex", Provider: "openai", Role: "Deep (legacy)", Description: "Deep reasoning (previous oracle)", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "gemini-3.1-pro-preview", Provider: "google", Role: "Review", Description: "Bug identification & code review", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "gemini-3-flash-preview", Provider: "google", Role: "Search / Look At / Handoff", Description: "Fast codebase retrieval & analysis", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "gemini-3-pro-image-preview", Provider: "google", Role: "Painter", Description: "Image generation and editing", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "gemini-2.5-flash", Provider: "google", Role: "Frontier fallback", Description: "Frontier agentMode fallback (consumer Gemini API)", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
	{Model: "gemini-3.5-flash", Provider: "google", Role: "Frontier (Vertex AI)", Description: "Vertex AI Gemini 3.5 Flash — frontier primary", Tiers: []string{TierAmp, TierLocal, TierAPIKey}},
}

// ModelRoleMap provides quick lookup of model role info by model name.
var ModelRoleMap = func() map[string]AmpModelRole {
	m := make(map[string]AmpModelRole, len(AmpModelRoles))
	for _, r := range AmpModelRoles {
		m[r.Model] = r
	}
	return m
}()

// ModelSupportsTier checks if a model supports a given auth tier.
func ModelSupportsTier(model, tier string) bool {
	r, ok := ModelRoleMap[model]
	if !ok {
		return tier == TierAmp // unknown models only via amp
	}
	for _, t := range r.Tiers {
		if t == tier {
			return true
		}
	}
	return false
}

func DefaultConfig() *Config {
	return &Config{
		Listen: ":9319",
		Neo: NeoConfig{
			SelfServe: true,
			Update: NeoUpdateConfig{
				Channel: "stable",
			},
		},
		Amp: AmpConfig{
			UpstreamURL: "https://ampcode.com",
		},
		Claude: ClaudeConfig{
			Source: "keychain",
			Models: []ModelEntry{
				{Name: "claude-opus-4-7", Route: "local"},
				{Name: "claude-opus-4-6", Route: "local"},
				{Name: "claude-sonnet-4-6", Route: "local"},
				{Name: "claude-haiku-4-5-20251001", Route: "local"},
			},
		},
		OpenAI: OpenAIConfig{
			Models: []ModelEntry{
				{Name: "gpt-5.5", Route: "local"},
				{Name: "gpt-5.4", Route: "local"},
				{Name: "gpt-5.3-codex", Route: "local"},
			},
		},
		Gemini: GeminiConfig{
			Models: []ModelEntry{
				{Name: "gemini-3.1-pro-preview", Route: "local"},
				{Name: "gemini-3-flash-preview", Route: "local"},
				{Name: "gemini-3-pro-image-preview", Route: "local"},
			},
		},
		Retry: RetryConfig{
			MaxAttempts:  5,
			InitialDelay: 1 * time.Second,
		},
	}
}

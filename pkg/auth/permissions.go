package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// PermissionRule mirrors one entry in amp's `amp.permissions` setting:
//
//	{ "tool": "Bash", "action": "ask",
//	  "matches": { "cmd": ["git push*", "rm -rf*"] } }
//
// action ∈ {allow, ask, deny}. matches.cmd is a list of glob patterns
// matched against the tool's `cmd` input field. Empty matches = unconditional.
type PermissionRule struct {
	Tool    string              `json:"tool"`
	Action  string              `json:"action"`
	Matches map[string][]string `json:"matches,omitempty"`
}

// PermissionsConfig is the parsed view from ~/.config/amp/settings.json.
type PermissionsConfig struct {
	mu       sync.RWMutex
	rules    []PermissionRule
	loadedAt time.Time
}

var globalPermissions = &PermissionsConfig{}

// LoadPermissions parses ~/.config/amp/settings.json on demand (cached 30s).
// On any failure returns an empty rule set (= auto-allow), preserving prior
// behavior so we never harden by accident.
func LoadPermissions() *PermissionsConfig {
	globalPermissions.mu.RLock()
	fresh := time.Since(globalPermissions.loadedAt) < 30*time.Second
	globalPermissions.mu.RUnlock()
	if fresh {
		return globalPermissions
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return globalPermissions
	}
	path := filepath.Join(home, ".config", "amp", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		globalPermissions.mu.Lock()
		globalPermissions.loadedAt = time.Now()
		globalPermissions.mu.Unlock()
		return globalPermissions
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return globalPermissions
	}
	raw, ok := doc["amp.permissions"]
	if !ok {
		globalPermissions.mu.Lock()
		globalPermissions.rules = nil
		globalPermissions.loadedAt = time.Now()
		globalPermissions.mu.Unlock()
		return globalPermissions
	}
	var rules []PermissionRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		log.Warnf("[PERMISSIONS] parse failed: %v", err)
		return globalPermissions
	}
	globalPermissions.mu.Lock()
	globalPermissions.rules = rules
	globalPermissions.loadedAt = time.Now()
	globalPermissions.mu.Unlock()
	log.Infof("[PERMISSIONS] loaded %d rule(s)", len(rules))
	return globalPermissions
}

// Decide returns the action ("allow", "ask", "deny") for a tool call.
// First matching rule wins. Default action when no rule matches is "allow"
// (matches the --dangerously-allow-all behavior we had before).
//
// args is the JSON-decoded tool input (typically map[string]any). For Bash
// the matched field is args.cmd.
func (p *PermissionsConfig) Decide(toolName string, args map[string]any) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, rule := range p.rules {
		if !equalFold(rule.Tool, toolName) {
			continue
		}
		if matchesArgs(rule.Matches, args) {
			if rule.Action == "" {
				return "allow"
			}
			return rule.Action
		}
	}
	return "allow"
}

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }

// matchesArgs returns true if all of rule.matches are satisfied by args.
// Empty matches map = always matches (unconditional rule).
func matchesArgs(matches map[string][]string, args map[string]any) bool {
	if len(matches) == 0 {
		return true
	}
	for field, patterns := range matches {
		val, ok := args[field]
		if !ok {
			return false
		}
		valStr, ok := val.(string)
		if !ok {
			b, _ := json.Marshal(val)
			valStr = string(b)
		}
		matched := false
		for _, p := range patterns {
			if globMatch(p, valStr) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// globMatch supports * (any chars) anywhere in the pattern.
func globMatch(pattern, s string) bool {
	// Convert glob to regex: escape regex meta, then * → .*
	re := strings.Builder{}
	re.WriteString("^")
	for _, r := range pattern {
		if r == '*' {
			re.WriteString(".*")
			continue
		}
		re.WriteString(regexp.QuoteMeta(string(r)))
	}
	re.WriteString("$")
	matched, err := regexp.MatchString(re.String(), s)
	return err == nil && matched
}

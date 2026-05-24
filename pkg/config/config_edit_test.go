package config

import "testing"

func TestUpdateAPIKeyUpdatesStoredEntry(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.OpenAI.Entries = []APIKeyEntry{{ID: "key-1", Label: "old", APIKey: "sk-old", BaseURL: "https://old.example.com"}}
	newKey := "sk-new"

	if !cfg.UpdateAPIKey("openai", "key-1", "new", "https://new.example.com", &newKey) {
		t.Fatal("expected update to succeed")
	}

	entry, ok := cfg.APIKey("openai", "key-1")
	if !ok {
		t.Fatal("expected updated key to exist")
	}
	if entry.Label != "new" || entry.APIKey != "sk-new" || entry.BaseURL != "https://new.example.com" {
		t.Fatalf("unexpected entry after update: %#v", entry)
	}
}

func TestUpdateCustomProviderKeepsExistingKeyWhenBlank(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Custom = []CustomProvider{{
		ID:      "cp-1",
		Name:    "Old",
		BaseURL: "https://old.example.com/v1",
		Entries: []APIKeyEntry{{ID: "entry-1", APIKey: "sk-old"}},
	}}

	if !cfg.UpdateCustomProvider("cp-1", "New", "https://new.example.com/v1", nil) {
		t.Fatal("expected custom provider update to succeed")
	}

	cp, ok := cfg.CustomProvider("cp-1")
	if !ok {
		t.Fatal("expected custom provider to exist")
	}
	if cp.Name != "New" || cp.BaseURL != "https://new.example.com/v1" {
		t.Fatalf("unexpected custom provider metadata: %#v", cp)
	}
	if len(cp.Entries) != 1 || cp.Entries[0].APIKey != "sk-old" {
		t.Fatalf("expected existing key to be preserved: %#v", cp.Entries)
	}
	newKey := "sk-new"
	if !cfg.UpdateCustomProvider("cp-1", "Newer", "https://newer.example.com/v1", &newKey) {
		t.Fatal("expected second update to succeed")
	}
	cp, _ = cfg.CustomProvider("cp-1")
	if cp.Entries[0].APIKey != "sk-new" {
		t.Fatalf("expected key replacement, got %#v", cp.Entries)
	}
}

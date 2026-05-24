package logger

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestDBStore(t *testing.T) *DBStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &DBStore{db: db}
}

func TestQueryStatsKeepsProviderModelDimension(t *testing.T) {
	store := newTestDBStore(t)
	defer store.Close()

	ts := time.Now().UTC()
	err := store.InsertLog(RequestLog{
		ID:        "a1",
		Timestamp: ts,
		Model:     "gpt-5.4",
		Provider:  "openai",
		Route:     "local/codex-file",
		Status:    200,
		Tokens: TokenUsage{
			InputTokens:  10,
			OutputTokens: 5,
		},
	})
	if err != nil {
		t.Fatalf("insert openai log: %v", err)
	}
	err = store.InsertLog(RequestLog{
		ID:        "a2",
		Timestamp: ts,
		Model:     "gpt-5.4",
		Provider:  "upstream",
		Route:     "UPSTREAM",
		Status:    200,
		Tokens: TokenUsage{
			InputTokens:  20,
			OutputTokens: 8,
		},
	})
	if err != nil {
		t.Fatalf("insert upstream log: %v", err)
	}

	stats, err := store.QueryStats(StatsFilter{})
	if err != nil {
		t.Fatalf("query stats: %v", err)
	}
	if got := len(stats.ByModel); got != 2 {
		t.Fatalf("expected 2 by_model entries, got %d", got)
	}
	if stats.ByModel["openai|gpt-5.4"] == nil {
		t.Fatal("missing openai model entry")
	}
	if stats.ByModel["upstream|gpt-5.4"] == nil {
		t.Fatal("missing upstream model entry")
	}
}

func TestQueryTokenTotalsNormalizesOpenAIAndAnthropicCacheSemantics(t *testing.T) {
	store := newTestDBStore(t)
	defer store.Close()

	ts := time.Now().UTC()
	entries := []RequestLog{
		{
			ID:        "o1",
			Timestamp: ts,
			Model:     "gpt-5.4",
			Provider:  "openai",
			Route:     "apikey",
			Status:    200,
			Tokens: TokenUsage{
				InputTokens:     100,
				OutputTokens:    20,
				CacheReadTokens: 30,
			},
		},
		{
			ID:        "c1",
			Timestamp: ts,
			Model:     "claude-opus-4-6",
			Provider:  "anthropic",
			Route:     "local/keychain",
			Status:    200,
			Tokens: TokenUsage{
				InputTokens:       10,
				OutputTokens:      5,
				CacheReadTokens:   80,
				CacheCreateTokens: 15,
			},
		},
	}

	for _, entry := range entries {
		if err := store.InsertLog(entry); err != nil {
			t.Fatalf("insert log %s: %v", entry.ID, err)
		}
	}

	totals, err := store.QueryTokenTotals(StatsFilter{})
	if err != nil {
		t.Fatalf("query token totals: %v", err)
	}

	if totals.Input != 110 {
		t.Fatalf("expected raw input 110, got %d", totals.Input)
	}
	if totals.DirectInput != 80 {
		t.Fatalf("expected direct input 80, got %d", totals.DirectInput)
	}
	if totals.FreshInput != 95 {
		t.Fatalf("expected fresh input 95, got %d", totals.FreshInput)
	}
	if totals.LogicalInput != 205 {
		t.Fatalf("expected logical input 205, got %d", totals.LogicalInput)
	}
	if totals.TotalTokens != 230 {
		t.Fatalf("expected total tokens 230, got %d", totals.TotalTokens)
	}

	stats, err := store.QueryStats(StatsFilter{})
	if err != nil {
		t.Fatalf("query stats: %v", err)
	}
	if stats.TotalLogicalInputTokens != 205 {
		t.Fatalf("expected stats logical input 205, got %d", stats.TotalLogicalInputTokens)
	}
	if stats.TotalFreshInputTokens != 95 {
		t.Fatalf("expected stats fresh input 95, got %d", stats.TotalFreshInputTokens)
	}
	if stats.TotalTokens != 230 {
		t.Fatalf("expected stats total tokens 230, got %d", stats.TotalTokens)
	}

	openAI := stats.ByModel["openai|gpt-5.4"]
	if openAI == nil {
		t.Fatal("missing openai model stats")
	}
	if openAI.TotalLogicalInput != 100 || openAI.TotalFreshInput != 70 || openAI.TotalTokens != 120 {
		t.Fatalf("unexpected openai model stats: %+v", *openAI)
	}

	claude := stats.ByModel["anthropic|claude-opus-4-6"]
	if claude == nil {
		t.Fatal("missing anthropic model stats")
	}
	if claude.TotalLogicalInput != 105 || claude.TotalFreshInput != 25 || claude.TotalTokens != 110 {
		t.Fatalf("unexpected anthropic model stats: %+v", *claude)
	}
}

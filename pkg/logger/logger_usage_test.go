package logger

import "testing"

func TestParseOpenAIUsagePromptCompletionAndCache(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":120,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":15}}}`)
	got := ParseOpenAIUsage(data)
	if got.InputTokens != 120 {
		t.Fatalf("expected input_tokens=120, got %d", got.InputTokens)
	}
	if got.OutputTokens != 40 {
		t.Fatalf("expected output_tokens=40, got %d", got.OutputTokens)
	}
	if got.CacheReadTokens != 15 {
		t.Fatalf("expected cache_read_tokens=15, got %d", got.CacheReadTokens)
	}
}

func TestParseOpenAIUsageResponsesAPI(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":88,"output_tokens":22,"input_tokens_details":{"cached_tokens":9}}}`)
	got := ParseOpenAIUsage(data)
	if got.InputTokens != 88 {
		t.Fatalf("expected input_tokens=88, got %d", got.InputTokens)
	}
	if got.OutputTokens != 22 {
		t.Fatalf("expected output_tokens=22, got %d", got.OutputTokens)
	}
	if got.CacheReadTokens != 9 {
		t.Fatalf("expected cache_read_tokens=9, got %d", got.CacheReadTokens)
	}
}

func TestParseOpenAIUsageResponsesCompletedEvent(t *testing.T) {
	data := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":88,"output_tokens":22,"input_tokens_details":{"cached_tokens":9}}}}`)
	got := ParseOpenAIUsage(data)
	if got.InputTokens != 88 {
		t.Fatalf("expected input_tokens=88, got %d", got.InputTokens)
	}
	if got.OutputTokens != 22 {
		t.Fatalf("expected output_tokens=22, got %d", got.OutputTokens)
	}
	if got.CacheReadTokens != 9 {
		t.Fatalf("expected cache_read_tokens=9, got %d", got.CacheReadTokens)
	}
}

func TestParseOpenAIUsageRawUsageObject(t *testing.T) {
	data := []byte(`{"input_tokens":64,"output_tokens":16,"input_tokens_details":{"cached_tokens":7}}`)
	got := ParseOpenAIUsage(data)
	if got.InputTokens != 64 {
		t.Fatalf("expected input_tokens=64, got %d", got.InputTokens)
	}
	if got.OutputTokens != 16 {
		t.Fatalf("expected output_tokens=16, got %d", got.OutputTokens)
	}
	if got.CacheReadTokens != 7 {
		t.Fatalf("expected cache_read_tokens=7, got %d", got.CacheReadTokens)
	}
}

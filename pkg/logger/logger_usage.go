package logger

import "github.com/tidwall/gjson"

// ParseClaudeUsage extracts token usage from a Claude API response or SSE data event.
// Claude format: {"usage":{"input_tokens":N,"output_tokens":N,"cache_read_input_tokens":N,"cache_creation_input_tokens":N}}
func ParseClaudeUsage(data []byte) TokenUsage {
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() {
		return TokenUsage{}
	}
	return TokenUsage{
		InputTokens:       usage.Get("input_tokens").Int(),
		OutputTokens:      usage.Get("output_tokens").Int(),
		CacheReadTokens:   usage.Get("cache_read_input_tokens").Int(),
		CacheCreateTokens: usage.Get("cache_creation_input_tokens").Int(),
	}
}

// ParseOpenAIUsage extracts token usage from an OpenAI API response or SSE data event.
// OpenAI format: {"usage":{"prompt_tokens":N,"completion_tokens":N,"total_tokens":N}}
// Responses API: {"usage":{"input_tokens":N,"output_tokens":N}}
// Streaming Responses API events often nest usage under {"response":{"usage":...}}.
// Some callers also pass the raw usage object directly.
func ParseOpenAIUsage(data []byte) TokenUsage {
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() {
		usage = gjson.GetBytes(data, "response.usage")
	}
	if !usage.Exists() {
		root := gjson.ParseBytes(data)
		if root.Get("input_tokens").Exists() || root.Get("prompt_tokens").Exists() ||
			root.Get("output_tokens").Exists() || root.Get("completion_tokens").Exists() {
			usage = root
		} else {
			return TokenUsage{}
		}
	}

	input := usage.Get("input_tokens").Int()
	if input == 0 {
		input = usage.Get("prompt_tokens").Int()
	}
	output := usage.Get("output_tokens").Int()
	if output == 0 {
		output = usage.Get("completion_tokens").Int()
	}
	cacheRead := usage.Get("prompt_tokens_details.cached_tokens").Int()
	if cacheRead == 0 {
		cacheRead = usage.Get("input_tokens_details.cached_tokens").Int()
	}

	return TokenUsage{
		InputTokens:     input,
		OutputTokens:    output,
		CacheReadTokens: cacheRead,
	}
}

// ParseGeminiUsage extracts token usage from a Gemini API response.
// Gemini format: {"usageMetadata":{"promptTokenCount":N,"candidatesTokenCount":N,"cachedContentTokenCount":N}}
func ParseGeminiUsage(data []byte) TokenUsage {
	usage := gjson.GetBytes(data, "usageMetadata")
	if !usage.Exists() {
		return TokenUsage{}
	}
	return TokenUsage{
		InputTokens:     usage.Get("promptTokenCount").Int(),
		OutputTokens:    usage.Get("candidatesTokenCount").Int(),
		CacheReadTokens: usage.Get("cachedContentTokenCount").Int(),
	}
}

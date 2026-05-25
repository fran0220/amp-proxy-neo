package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type ToolDef map[string]any

type ToolUse struct {
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// PrepareToolsRequest converts Anthropic/OpenAI/generic tool definitions into
// either an OpenAI-compatible tools field, or a system prompt appendix for
// endpoints without native tool calling. Set prefersJSONMode for fallback mode.
func PrepareToolsRequest(tools []ToolDef, prefersJSONMode bool) (toolsField any, systemAppend string) {
	if len(tools) == 0 {
		return nil, ""
	}
	openAITools := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		name, _ := t["name"].(string)
		if name == "" {
			if fn, ok := t["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		if name == "" {
			continue
		}
		desc, _ := t["description"].(string)
		params := t["parameters"]
		if params == nil {
			params = t["input_schema"]
		}
		if params == nil {
			if fn, ok := t["function"].(map[string]any); ok {
				desc, _ = fn["description"].(string)
				params = fn["parameters"]
			}
		}
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		openAITools = append(openAITools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": desc,
				"parameters":  params,
			},
		})
	}
	if !prefersJSONMode {
		return openAITools, ""
	}
	b, _ := json.MarshalIndent(openAITools, "", "  ")
	return nil, "\n\nTool calling is available, but this endpoint has no native function-calling API. If you need a tool, respond with ONLY JSON in this shape (no markdown unless wrapping the JSON is unavoidable):\n{\"tool_uses\":[{\"name\":\"tool_name\",\"input\":{...}}]}\nIf no tool is needed, answer normally. Available tools:\n" + string(b)
}

var fencedJSONRE = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

// ParseToolsResponse extracts JSON-mode fallback tool calls from local-model
// assistant text. It tolerates markdown fences and prose before/after JSON.
func ParseToolsResponse(assistantText string) (text string, toolUses []ToolUse) {
	candidate := strings.TrimSpace(assistantText)
	if m := fencedJSONRE.FindStringSubmatch(candidate); len(m) == 2 {
		candidate = strings.TrimSpace(m[1])
	} else if start := strings.Index(candidate, "{"); start >= 0 {
		if end := strings.LastIndex(candidate, "}"); end > start {
			candidate = candidate[start : end+1]
		}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(candidate), &obj); err != nil {
		return assistantText, nil
	}
	parseOne := func(v any) (ToolUse, bool) {
		m, ok := v.(map[string]any)
		if !ok {
			return ToolUse{}, false
		}
		name, _ := m["name"].(string)
		if name == "" {
			name, _ = m["tool"].(string)
		}
		if name == "" {
			return ToolUse{}, false
		}
		input := map[string]any{}
		if im, ok := m["input"].(map[string]any); ok {
			input = im
		}
		if im, ok := m["arguments"].(map[string]any); ok {
			input = im
		}
		id, _ := m["id"].(string)
		return ToolUse{ID: id, Name: name, Input: input}, true
	}
	if arr, ok := obj["tool_uses"].([]any); ok {
		for _, v := range arr {
			if tu, ok := parseOne(v); ok {
				toolUses = append(toolUses, tu)
			}
		}
	}
	if arr, ok := obj["tool_calls"].([]any); ok {
		for _, v := range arr {
			if tu, ok := parseOne(v); ok {
				toolUses = append(toolUses, tu)
			}
		}
	}
	if len(toolUses) == 0 {
		if tu, ok := parseOne(obj); ok {
			toolUses = append(toolUses, tu)
		}
	}
	if len(toolUses) == 0 {
		return assistantText, nil
	}
	if s, _ := obj["text"].(string); s != "" {
		text = s
	}
	return text, toolUses
}

func ToolPromptSample(tools []ToolDef) string {
	_, s := PrepareToolsRequest(tools, true)
	return fmt.Sprintf("%s", s)
}

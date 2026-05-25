package provider

import (
	"strings"
	"testing"
)

func TestToolsCompatAnthropicRoundTrip(t *testing.T) {
	tools := []ToolDef{{
		"name":        "read_file",
		"description": "read a file",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{
			"path": map[string]any{"type": "string"},
		}},
	}}
	field, prompt := PrepareToolsRequest(tools, true)
	if field != nil {
		t.Fatalf("fallback tools field = %#v, want nil", field)
	}
	if prompt == "" || !strings.Contains(prompt, "read_file") || !strings.Contains(prompt, "tool_uses") {
		t.Fatalf("prompt missing tool instructions: %s", prompt)
	}
	text, uses := ParseToolsResponse("```json\n{\"tool_uses\":[{\"name\":\"read_file\",\"input\":{\"path\":\"README.md\"}}]}\n```")
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	if len(uses) != 1 || uses[0].Name != "read_file" || uses[0].Input["path"] != "README.md" {
		t.Fatalf("bad tool uses: %#v", uses)
	}
}

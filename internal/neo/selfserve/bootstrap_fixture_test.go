package selfserve

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LoadBootstrapFrames loads a redacted Rivet bootstrap JSONL fixture by name.
// Lines beginning with PENDING: or # are ignored so tests can skip gracefully
// before a real capture is available.
func LoadBootstrapFrames(name string) ([]map[string]any, error) {
	if name == "" || strings.Contains(name, string(os.PathSeparator)) {
		return nil, fmt.Errorf("invalid fixture name %q", name)
	}

	path := filepath.Join("..", "..", "..", "testdata", "bootstrap", name+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var frames []map[string]any
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "PENDING:") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		frames = append(frames, frame)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return frames, nil
}

func TestLoadBootstrapFrames(t *testing.T) {
	frames, err := LoadBootstrapFrames("smart-mode")
	if err != nil {
		t.Fatalf("LoadBootstrapFrames: %v", err)
	}
	if len(frames) == 0 {
		t.Skip("bootstrap fixture is pending; run scripts/capture-bootstrap.sh to populate testdata/bootstrap/smart-mode.jsonl")
	}
	firstType, _ := frames[0]["type"].(string)
	if firstType == "" {
		t.Fatalf("first frame type is empty: %#v", frames[0])
	}
}

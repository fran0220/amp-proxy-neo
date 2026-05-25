package rivet

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestNewStandaloneSessionDefaults(t *testing.T) {
	s := NewStandaloneSession("", "", "user-1")
	if s.threadID == "" || s.threadID[:2] != "T-" {
		t.Fatalf("threadID = %q, want T-*", s.threadID)
	}
	if s.agentMode != "smart" {
		t.Fatalf("agentMode = %q, want smart", s.agentMode)
	}
	if !s.remoteDriven {
		t.Fatalf("remoteDriven = false, want true")
	}
	if len(s.anthroTools) != 0 {
		t.Fatalf("tools = %d, want 0", len(s.anthroTools))
	}
}

func TestRunStandaloneStubProducesFrames(t *testing.T) {
	t.Setenv(standaloneStubResponseEnv, "hello from stub")
	s := NewStandaloneSession("T-019e0000-0000-7000-8000-000000000001", "smart", "user-1")
	frames, err := RunStandalone(context.Background(), s, "say hi")
	if err != nil {
		t.Fatalf("RunStandalone: %v", err)
	}
	seenDelta := false
	seenMessage := false
	for frame := range frames {
		switch frame["type"] {
		case "delta":
			seenDelta = true
		case "message_added":
			seenMessage = true
		}
	}
	if !seenDelta || !seenMessage {
		t.Fatalf("seen delta=%v message=%v, want both", seenDelta, seenMessage)
	}
	raw, err := BuildStandaloneThreadJSON(s)
	if err != nil {
		t.Fatalf("BuildStandaloneThreadJSON: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("empty thread json")
	}
}

func TestRunStandaloneStubCanCancel(t *testing.T) {
	_ = os.Setenv(standaloneStubResponseEnv, "this response has enough text to be chunked and then cancelled")
	defer os.Unsetenv(standaloneStubResponseEnv)
	ctx, cancel := context.WithCancel(context.Background())
	s := NewStandaloneSession("T-019e0000-0000-7000-8000-000000000002", "smart", "user-1")
	frames, err := RunStandalone(ctx, s, "say hi")
	if err != nil {
		t.Fatalf("RunStandalone: %v", err)
	}
	cancel()
	select {
	case <-frames:
	case <-time.After(time.Second):
		t.Fatalf("frames did not unblock after cancel")
	}
}

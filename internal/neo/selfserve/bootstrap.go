package selfserve

import "fmt"

// SynthesizeStartupFrames returns the minimal server→client frames needed to
// unblock Amp CLI after a Rivet WS upgrade. TODO: replace/extend this with the
// exact captured sequence once testdata/bootstrap/smart-mode.jsonl contains a
// real actors.ampcode.com capture instead of the current placeholder.
func SynthesizeStartupFrames(threadID, agentMode, userID string) []map[string]any {
	if agentMode == "" {
		agentMode = "smart"
	}
	actorID := "local-executor"
	if threadID != "" {
		actorID = fmt.Sprintf("local-executor-%s", threadID)
	}
	return []map[string]any{
		{
			"type":      "executor_connected",
			"seq":       1,
			"actorId":   actorID,
			"threadId":  threadID,
			"userId":    userID,
			"selfServe": true,
		},
		{
			"type":      "agent_state",
			"seq":       2,
			"state":     "idle",
			"agentMode": agentMode,
			"threadId":  threadID,
			"actorId":   actorID,
		},
	}
}

package remoteagent

import "sync"

// Registry of amp subprocess PIDs that were spawned by RemoteAgent for
// Browser-driven sessions. Our Rivet WS observer consults this so it
// always routes the user's message LOCALLY (overriding the default
// Provider 分流 which would passthrough rush/deep/frontier to amp server).
//
// The browser user has no BYOK billing setup on the amp side; if we
// passed through they'd get 402. Local routing uses their own Claude/
// OpenAI/Gemini credentials.

var (
	remotePIDMu sync.RWMutex
	remotePIDs  = map[int]bool{}
)

func RegisterRemotePID(pid int) {
	remotePIDMu.Lock()
	remotePIDs[pid] = true
	remotePIDMu.Unlock()
}

func UnregisterRemotePID(pid int) {
	remotePIDMu.Lock()
	delete(remotePIDs, pid)
	remotePIDMu.Unlock()
}

func IsRemotePID(pid int) bool {
	if pid <= 0 {
		return false
	}
	remotePIDMu.RLock()
	defer remotePIDMu.RUnlock()
	return remotePIDs[pid]
}

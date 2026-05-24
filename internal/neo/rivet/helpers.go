package rivet

import (
	"net/http"
	"strings"
	"sync"

	"github.com/fran0220/amp-proxy-neo/pkg/identity"
	"github.com/fran0220/amp-proxy-neo/pkg/util"
)

func truncateStr(s string, n int) string { return identity.TruncateStr(s, n) }

func injectClaudeCodeIdentity(body []byte, stableUserID string) []byte {
	return identity.InjectClaudeCodeIdentity(body, stableUserID)
}

func dumpWSFrame(connID uint64, direction string, msgType int, data []byte) {
	util.DumpWSFrame(connID, direction, msgType, data)
}

// isWebSocketUpgrade checks if a request is a WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

var remotePIDRegistry = struct {
	sync.RWMutex
	pids map[int]bool
}{pids: map[int]bool{}}

func registerRemotePID(pid int) {
	remotePIDRegistry.Lock()
	remotePIDRegistry.pids[pid] = true
	remotePIDRegistry.Unlock()
}

func unregisterRemotePID(pid int) {
	remotePIDRegistry.Lock()
	delete(remotePIDRegistry.pids, pid)
	remotePIDRegistry.Unlock()
}

func isRemotePID(pid int) bool {
	if pid <= 0 {
		return false
	}
	remotePIDRegistry.RLock()
	defer remotePIDRegistry.RUnlock()
	return remotePIDRegistry.pids[pid]
}

func notifyPendingNewThread(threadID string) {}

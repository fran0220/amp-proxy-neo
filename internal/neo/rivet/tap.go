package rivet

import "sync"

// Frame-tap registry: allows other components (e.g. RemoteAgent for the
// browser WebUI) to subscribe to Rivet WS frames flowing for a specific
// thread, without changing the forwarder code paths.
//
// Used by RemoteAgent.handleSendMessage: when the Browser sends a message,
// the Mac spawns `amp threads continue THREAD -x TEXT` which opens its own
// Rivet WS to our proxy. Our forwarder runs local inference, injects
// delta/message_added/tool_use frames. The tap captures the same frames
// going to amp's UI and forwards them to the Browser — so the WebUI sees
// EXACTLY what the TUI sees, including tool calls, thinking, etc.

var (
	tapMu sync.RWMutex
	taps  = map[string][]tapEntry{}
)

type tapEntry struct {
	id int64
	cb func(direction string, frame map[string]any)
}

var tapIDCounter int64

// RegisterThreadTap registers a callback that receives a copy of every
// Rivet frame flowing for the given thread (both server→client injection
// from our local inference AND any pass-through frames). Returns an
// unregister function.
//
// direction: "inject" (we emitted to amp UI), "client" (amp→server), or
// "server" (server→amp pass-through).
func RegisterThreadTap(threadID string, cb func(direction string, frame map[string]any)) func() {
	if threadID == "" || cb == nil {
		return func() {}
	}
	tapMu.Lock()
	tapIDCounter++
	id := tapIDCounter
	taps[threadID] = append(taps[threadID], tapEntry{id: id, cb: cb})
	tapMu.Unlock()
	return func() {
		tapMu.Lock()
		defer tapMu.Unlock()
		list := taps[threadID]
		for i, e := range list {
			if e.id == id {
				taps[threadID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(taps[threadID]) == 0 {
			delete(taps, threadID)
		}
	}
}

// EmitFrameTap notifies all subscribers for a given thread. Async: each
// callback runs in its own goroutine so a slow / blocked subscriber can
// never wedge the forwarder or inference path.
func EmitFrameTap(threadID, direction string, frame map[string]any) {
	if threadID == "" {
		return
	}
	tapMu.RLock()
	list := append([]tapEntry(nil), taps[threadID]...)
	tapMu.RUnlock()
	for _, e := range list {
		go func(cb func(string, map[string]any)) {
			defer func() { _ = recover() }()
			cb(direction, frame)
		}(e.cb)
	}
}

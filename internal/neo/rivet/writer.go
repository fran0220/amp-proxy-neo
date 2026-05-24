package rivet

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

// frameWriter decouples the SSE-reading goroutine from the WebSocket write
// goroutine, and coalesces high-frequency streaming text/thinking deltas
// into larger frames. This is the fix for the "TUI feels chunky" symptom:
//
//   - Anthropic Sonnet emits 50-200 text_deltas/sec, each 1-5 chars.
//   - Previously each delta was synchronously json.Marshal'd, INFO-logged,
//     tap-fanned-out, and WS-written under a mutex — so the SSE reader was
//     entirely paced by the slowest of those steps, with no batching.
//   - Now: streaming text/thinking deltas accumulate into a strings.Builder
//     and are flushed on a 16ms timer / 48-char size / newline / on any
//     non-streaming frame. All other frames are enqueued directly.
//   - A single writer goroutine owns client.WriteMessage, so the SSE reader
//     never blocks on TCP backpressure (until the bounded queue fills).
//
// All non-text/thinking frames (message_added, agent_state, tool_use, plugin
// messages, etc.) flush any pending coalesced text first so ordering is
// preserved exactly as amp's UI expects.
type frameWriter struct {
	client   *websocket.Conn
	clientMu *sync.Mutex // shared with pipeUpstream / pipeClient
	connID   uint64
	threadID string

	out       chan []byte
	done      chan struct{}
	failed    atomic.Bool
	closeOnce sync.Once

	coalMu       sync.Mutex
	coalMsgID    string
	coalBlockIdx int
	coalKind     string // "text" or "thinking"
	coalBuf      strings.Builder
	coalTimer    *time.Timer
}

const (
	// coalesceWindow is the max time we hold a streaming chunk before flushing.
	// 16ms ≈ 60fps; balances latency vs. frame count.
	coalesceWindow = 16 * time.Millisecond
	// coalesceBytes is the max chars to buffer before forcing a flush, so
	// long uninterrupted text doesn't sit in the buffer waiting for the timer.
	coalesceBytes = 48
	// outQueue is the bounded channel size. If the client stalls badly the
	// queue fills and producers block — better than unbounded memory growth.
	outQueue = 256
)

func newFrameWriter(client *websocket.Conn, clientMu *sync.Mutex, connID uint64, threadID string) *frameWriter {
	fw := &frameWriter{
		client:   client,
		clientMu: clientMu,
		connID:   connID,
		threadID: threadID,
		out:      make(chan []byte, outQueue),
		done:     make(chan struct{}),
	}
	go fw.run()
	return fw
}

func (fw *frameWriter) run() {
	defer close(fw.done)
	for buf := range fw.out {
		fw.clientMu.Lock()
		err := fw.client.WriteMessage(websocket.TextMessage, buf)
		fw.clientMu.Unlock()
		if err != nil {
			fw.failed.Store(true)
			log.Debugf("[RIVET %d] writer goroutine: %v (draining)", fw.connID, err)
			// Drain remaining frames so Write/Close don't block.
			for range fw.out {
			}
			return
		}
	}
}

// Write is the drop-in replacement for the per-session writeFrameRaw closure.
// Streaming text/thinking deltas (blockState=="streaming") are coalesced.
// All other frames are enqueued immediately, after flushing any pending text
// so amp's UI receives frames in the same logical order as before.
func (fw *frameWriter) Write(obj map[string]any) error {
	if fw.failed.Load() {
		return nil // best-effort; client gone
	}

	if isCoalescableDelta(obj) {
		msgID, _ := obj["messageId"].(string)
		blockIdx, _ := obj["blockIndex"].(int)
		blocks, _ := obj["blocks"].([]map[string]any)
		blk := blocks[0]
		kind, _ := blk["type"].(string)
		var txt string
		if kind == "text" {
			txt, _ = blk["text"].(string)
		} else {
			txt, _ = blk["thinking"].(string)
		}
		if txt == "" {
			return nil
		}
		fw.appendDelta(msgID, blockIdx, kind, txt)
		return nil
	}

	// Non-coalescable: flush pending and enqueue.
	fw.flush()
	buf, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	dumpWSFrame(fw.connID, "INJ", websocket.TextMessage, buf)
	EmitFrameTap(fw.threadID, "inject", obj)
	if log.IsLevelEnabled(log.DebugLevel) {
		log.Debugf("[RIVET %d INJECT] %s", fw.connID, truncateStr(string(buf), 160))
	}
	return fw.enqueueBytes(buf)
}

// Close flushes any pending coalesced text and signals the writer goroutine
// to exit. Safe to call multiple times.
func (fw *frameWriter) Close() {
	fw.closeOnce.Do(func() {
		fw.flush()
		close(fw.out)
	})
	<-fw.done
}

func isCoalescableDelta(obj map[string]any) bool {
	if t, _ := obj["type"].(string); t != "delta" {
		return false
	}
	blocks, ok := obj["blocks"].([]map[string]any)
	if !ok || len(blocks) != 1 {
		return false
	}
	blk := blocks[0]
	if state, _ := blk["blockState"].(string); state != "streaming" {
		return false
	}
	kind, _ := blk["type"].(string)
	return kind == "text" || kind == "thinking"
}

func (fw *frameWriter) appendDelta(msgID string, blockIdx int, kind, txt string) {
	fw.coalMu.Lock()
	defer fw.coalMu.Unlock()

	// Flush if block identity changed (different msg, block, or kind).
	if fw.coalBuf.Len() > 0 && (fw.coalMsgID != msgID || fw.coalBlockIdx != blockIdx || fw.coalKind != kind) {
		fw.flushLocked()
	}
	fw.coalMsgID = msgID
	fw.coalBlockIdx = blockIdx
	fw.coalKind = kind
	fw.coalBuf.WriteString(txt)

	// Size-based flush: don't hold large buffers for the timer.
	if fw.coalBuf.Len() >= coalesceBytes {
		fw.flushLocked()
		return
	}
	// Newline-based flush: flush at line boundaries so paragraphs feel fluid.
	if strings.ContainsAny(txt, "\n") {
		fw.flushLocked()
		return
	}
	// Timer-based flush: 16ms max latency.
	if fw.coalTimer == nil {
		fw.coalTimer = time.AfterFunc(coalesceWindow, func() {
			fw.coalMu.Lock()
			fw.flushLocked()
			fw.coalMu.Unlock()
		})
	} else {
		fw.coalTimer.Reset(coalesceWindow)
	}
}

func (fw *frameWriter) flush() {
	fw.coalMu.Lock()
	fw.flushLocked()
	fw.coalMu.Unlock()
}

// flushLocked emits the pending coalesced delta. Caller must hold coalMu.
func (fw *frameWriter) flushLocked() {
	if fw.coalBuf.Len() == 0 {
		return
	}
	if fw.coalTimer != nil {
		fw.coalTimer.Stop()
		fw.coalTimer = nil
	}
	txt := fw.coalBuf.String()
	fw.coalBuf.Reset()

	var blocks []map[string]any
	if fw.coalKind == "thinking" {
		blocks = []map[string]any{{"type": "thinking", "thinking": txt, "blockState": "streaming"}}
	} else {
		blocks = []map[string]any{{"type": "text", "text": txt, "blockState": "streaming"}}
	}
	obj := map[string]any{
		"type":       "delta",
		"messageId":  fw.coalMsgID,
		"role":       "assistant",
		"blocks":     blocks,
		"blockIndex": fw.coalBlockIdx,
		"state":      "generating",
	}
	buf, err := json.Marshal(obj)
	if err != nil {
		return
	}
	dumpWSFrame(fw.connID, "INJ", websocket.TextMessage, buf)
	EmitFrameTap(fw.threadID, "inject", obj)
	_ = fw.enqueueBytes(buf)
}

// enqueueBytes blocks if the queue is full. If the writer goroutine has
// failed, returns nil (best-effort drop).
func (fw *frameWriter) enqueueBytes(buf []byte) error {
	if fw.failed.Load() {
		return nil
	}
	// If closeOnce already ran, fw.out is closed and a send would panic.
	// Detect via the done channel.
	select {
	case <-fw.done:
		return nil
	default:
	}
	select {
	case fw.out <- buf:
		return nil
	case <-fw.done:
		return nil
	}
}

// Manages the streaming state of an in-flight assistant turn. Buffers delta
// chunks via requestAnimationFrame so React only re-renders at ~60fps even
// when delta frames arrive faster.

import { useEffect, useRef, useState, useCallback } from "react";
import type { Message, ServerFrame, ContentBlock } from "../lib/types";

export interface StreamingState {
  text: string;                              // current assistant text accumulator
  thinking: string;                           // current thinking accumulator
  toolCalls: Map<string, ToolCallState>;     // toolCallId → state
  active: boolean;                            // currently streaming
  done: boolean;                              // turn complete
}

export interface ToolCallState {
  id: string;
  name: string;
  input: any;
  status: "pending" | "running" | "done" | "error";
  result?: any;
}

const emptyState = (): StreamingState => ({
  text: "",
  thinking: "",
  toolCalls: new Map(),
  active: false,
  done: false,
});

/**
 * useStreamingChat consumes frames from the coordinator and maintains the
 * live "current turn" state. Once `done` arrives, the assembled message is
 * pushed to `committedMessages` and state resets.
 */
export function useStreamingChat(
  subscribe: (cb: (frame: ServerFrame) => void) => () => void,
  reqIdRef: React.MutableRefObject<string | null>,
  onMessageCommitted: (m: Message) => void,
  onThreadCreated: (id: string) => void,
  onTitleUpdated: (title: string) => void,
) {
  const [streaming, setStreaming] = useState<StreamingState>(emptyState);

  // Buffered updates flushed on RAF for smooth streaming.
  const pendingRef = useRef<{
    text: string;
    thinking: string;
    toolMutations: Array<(map: Map<string, ToolCallState>) => void>;
    becameActive: boolean;
    becameDone: boolean;
    finalMsg?: Message;
  } | null>(null);
  const rafRef = useRef<number | null>(null);

  const scheduleFlush = useCallback(() => {
    if (rafRef.current != null) return;
    rafRef.current = requestAnimationFrame(() => {
      rafRef.current = null;
      const p = pendingRef.current;
      pendingRef.current = null;
      if (!p) return;
      setStreaming((prev) => {
        const next: StreamingState = {
          text: prev.text + p.text,
          thinking: prev.thinking + p.thinking,
          toolCalls: new Map(prev.toolCalls),
          active: prev.active || p.becameActive,
          done: prev.done || p.becameDone,
        };
        for (const fn of p.toolMutations) fn(next.toolCalls);
        return next;
      });
      // If final message arrived, commit it after state flushed.
      if (p.finalMsg) {
        // Queue commit AFTER the state flush paints
        const finalMsg = p.finalMsg;
        queueMicrotask(() => {
          onMessageCommitted(finalMsg);
          // Reset for next round (don't reset if more rounds incoming).
          if (p.becameDone) {
            setStreaming(emptyState());
          } else {
            // tool round may continue — reset text/thinking but keep tools.
            setStreaming((prev) => ({
              ...prev,
              text: "",
              thinking: "",
              active: false,
            }));
          }
        });
      } else if (p.becameDone) {
        queueMicrotask(() => setStreaming(emptyState()));
      }
    });
  }, [onMessageCommitted]);

  const pushPending = useCallback(
    (patch: Partial<NonNullable<typeof pendingRef.current>>) => {
      if (!pendingRef.current) {
        pendingRef.current = {
          text: "",
          thinking: "",
          toolMutations: [],
          becameActive: false,
          becameDone: false,
        };
      }
      const p = pendingRef.current;
      if (patch.text) p.text += patch.text;
      if (patch.thinking) p.thinking += patch.thinking;
      if (patch.toolMutations) p.toolMutations.push(...patch.toolMutations);
      if (patch.becameActive) p.becameActive = true;
      if (patch.becameDone) p.becameDone = true;
      if (patch.finalMsg) p.finalMsg = patch.finalMsg;
      scheduleFlush();
    },
    [scheduleFlush],
  );

  useEffect(() => {
    const unsub = subscribe((frame) => {
      // Always handle global frames (no reqId).
      if (frame.type === "thread_created" && frame.threadId) {
        onThreadCreated(frame.threadId);
      }
      if (frame.type === "thread_title" && frame.title) {
        onTitleUpdated(frame.title);
      }
      // Stream frames must match our active reqId — but read it through a
      // ref so we always see the latest value without re-subscribing.
      const reqId = reqIdRef.current;
      if (!reqId || frame.reqId !== reqId) return;

      switch (frame.type) {
        case "delta": {
          if (frame.text) {
            pushPending({ text: frame.text, becameActive: true });
          } else if (Array.isArray(frame.blocks)) {
            for (const b of frame.blocks) handleStreamBlock(b, pushPending);
          }
          break;
        }
        case "agent_state":
          if (frame.state === "streaming" || frame.state === "running_tools") {
            pushPending({ becameActive: true });
          }
          break;
        case "tool_lease": {
          const id = frame.toolCallId || "";
          if (!id) break;
          pushPending({
            toolMutations: [
              (map) => {
                map.set(id, {
                  id,
                  name: frame.toolName || "tool",
                  input: frame.args ?? {},
                  status: "running",
                });
              },
            ],
            becameActive: true,
          });
          break;
        }
        case "tool_progress": {
          const id = frame.toolCallId || "";
          if (!id) break;
          const status = frame.progress?.value?.status;
          const result = frame.progress?.value?.result ?? frame.progress?.value;
          if (status === "done") {
            pushPending({
              toolMutations: [
                (map) => {
                  const t = map.get(id);
                  if (t) {
                    t.status = "done";
                    t.result = result;
                    map.set(id, t);
                  }
                },
              ],
            });
          }
          break;
        }
        case "message_added": {
          if (!frame.message) break;
          const m = frame.message;
          // Skip role=user message_added: amp echoes the user prompt back via
          // protocol bookkeeping, but the WebUI already displayed it locally
          // when the user pressed Send. (Tool-result user messages are also
          // role=user and don't need rendering — they're hidden anyway.)
          if (m.role === "user") break;
          pushPending({ finalMsg: m });
          break;
        }
        case "done":
          pushPending({ becameDone: true });
          break;
        case "error":
        case "error_set":
          pushPending({
            finalMsg: {
              role: "info",
              content: [
                {
                  type: "text",
                  text: `Error: ${frame.error?.message || (frame as any).message || "unknown"}`,
                },
              ],
            },
            becameDone: true,
          });
          break;
      }
    });
    return unsub;
  }, [subscribe, reqIdRef, pushPending, onThreadCreated, onTitleUpdated]);

  return streaming;
}

function handleStreamBlock(
  block: any,
  push: (patch: any) => void,
) {
  if (!block || typeof block !== "object") return;
  if (block.type === "text" && block.text) {
    push({ text: block.text, becameActive: true });
  } else if (block.type === "thinking" && block.thinking) {
    push({ thinking: block.thinking, becameActive: true });
  } else if (block.type === "tool_use") {
    const id = block.id || "";
    if (!id) return;
    push({
      toolMutations: [
        (map: Map<string, ToolCallState>) => {
          const existing = map.get(id) || { id, name: "", input: {}, status: "pending" };
          map.set(id, {
            ...existing,
            name: block.name || existing.name || "tool",
            input: block.input ?? existing.input,
            status: block.blockState === "complete" ? "running" : "pending",
          });
        },
      ],
      becameActive: true,
    });
  }
}

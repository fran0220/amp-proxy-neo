import { useEffect, useRef } from "react";
import type { Message } from "../../lib/types";
import type { StreamingState } from "../../hooks/useStreamingChat";
import { MessageView } from "./Message";
import { StreamingMessage } from "./StreamingMessage";
import styles from "./MessageList.module.css";

interface Props {
  messages: Message[];
  streaming: StreamingState;
}

export function MessageList({ messages, streaming }: Props) {
  const ref = useRef<HTMLDivElement>(null);
  const lastScrollAtBottom = useRef(true);

  // Sticky auto-scroll: only follow if user already near bottom.
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (lastScrollAtBottom.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [messages, streaming.text, streaming.thinking, streaming.toolCalls.size]);

  const onScroll = () => {
    const el = ref.current;
    if (!el) return;
    lastScrollAtBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 100;
  };

  // Build tool_result map so user-msgs of tool_results merge into the
  // corresponding assistant tool_use card.
  const toolResultsByUseID: Record<string, any> = {};
  for (const m of messages) {
    if (m.role !== "user" || !Array.isArray(m.content)) continue;
    for (const c of m.content) {
      if (c.type === "tool_result") {
        const id = c.toolUseID || c.tool_use_id;
        if (id) toolResultsByUseID[id] = c;
      }
    }
  }

  const visibleMessages = messages.filter((m) => {
    // Hide user messages that only contain tool_results — they're not human input.
    if (
      m.role === "user" &&
      Array.isArray(m.content) &&
      m.content.length > 0 &&
      m.content.every((c) => c.type === "tool_result")
    ) {
      return false;
    }
    return true;
  });

  const showStreaming =
    streaming.active ||
    streaming.text.length > 0 ||
    streaming.thinking.length > 0 ||
    streaming.toolCalls.size > 0;

  return (
    <div ref={ref} className={styles.list} onScroll={onScroll}>
      {visibleMessages.length === 0 && !showStreaming && (
        <div className={styles.empty}>
          <p>Start a new conversation or pick a thread.</p>
        </div>
      )}
      {visibleMessages.map((m, i) => (
        <MessageView key={i} message={m} toolResultsByUseID={toolResultsByUseID} />
      ))}
      {showStreaming && <StreamingMessage state={streaming} />}
    </div>
  );
}

import { useCallback, useState, useRef } from "react";
import { Menu } from "lucide-react";
import type { Message, ServerFrame, ThreadData } from "../../lib/types";
import { MessageList } from "./MessageList";
import { Composer } from "./Composer";
import { useStreamingChat } from "../../hooks/useStreamingChat";
import styles from "./ChatPanel.module.css";

interface Props {
  activeThread: ThreadData | null;
  activeThreadId: string | null;
  messages: Message[];
  online: boolean;
  send: (envelope: any) => boolean;
  subscribe: (cb: (f: ServerFrame) => void) => () => void;
  onMessageCommitted: (m: Message) => void;
  onThreadCreated: (id: string) => void;
  onTitleUpdated: (title: string) => void;
  onUserSent: (text: string) => void;
  onOpenSidebar: () => void;
}

export function ChatPanel({
  activeThread,
  activeThreadId,
  messages,
  online,
  send,
  subscribe,
  onMessageCommitted,
  onThreadCreated,
  onTitleUpdated,
  onUserSent,
  onOpenSidebar,
}: Props) {
  const reqIdRef = useRef<string | null>(null);
  const [mode, setMode] = useState<string>(
    () => localStorage.getItem("amp.defaultMode") || "smart",
  );

  const streaming = useStreamingChat(
    subscribe,
    reqIdRef,
    (m) => {
      onMessageCommitted(m);
    },
    onThreadCreated,
    onTitleUpdated,
  );

  const handleSend = useCallback(
    (text: string) => {
      const id = crypto.randomUUID();
      // Set ref BEFORE sending so streaming hook sees it on the first
      // arriving frame (no React render cycle delay).
      reqIdRef.current = id;
      const ok = send({
        type: "send_message",
        reqId: id,
        threadId: activeThreadId || "",
        text,
        agentMode: mode,
      });
      if (ok) {
        onUserSent(text);
      } else {
        reqIdRef.current = null;
      }
    },
    [send, activeThreadId, mode, onUserSent],
  );

  const handleModeChange = useCallback((m: string) => {
    setMode(m);
    localStorage.setItem("amp.defaultMode", m);
  }, []);

  return (
    <main className={styles.chat}>
      <header className={styles.header}>
        <button onClick={onOpenSidebar} className={styles.mobileMenu} title="Threads">
          <Menu size={18} />
        </button>
        <div className={styles.threadInfo}>
          <div className={styles.title}>{activeThread?.title || "New thread"}</div>
          {activeThread && (
            <div className={styles.cwd} title={getCwd(activeThread)}>
              {prettyCwd(getCwd(activeThread))}
            </div>
          )}
        </div>
        <select
          className={styles.modePicker}
          value={mode}
          onChange={(e) => handleModeChange(e.target.value)}
          disabled={streaming.active}
        >
          <option value="smart">smart</option>
          <option value="large">large</option>
          <option value="rush">rush</option>
          <option value="deep">deep</option>
          <option value="frontier">frontier</option>
        </select>
      </header>

      <MessageList messages={messages} streaming={streaming} />

      <Composer
        onSend={handleSend}
        disabled={!online || streaming.active}
        placeholder={online ? "Message…" : "Agent offline"}
      />
    </main>
  );
}

function getCwd(t: ThreadData): string {
  const e = t.env?.initial;
  if (!e) return "";
  if (e.workingDirectory) return e.workingDirectory;
  if (e.trees?.[0]?.uri) return e.trees[0].uri;
  if (e.workspaceRoot) return e.workspaceRoot;
  return "";
}

function prettyCwd(cwd: string): string {
  return cwd.replace(/^file:\/\//, "").replace(/^\/private\//, "/");
}

import { useCallback, useEffect, useState } from "react";
import { Sidebar } from "./components/Sidebar/Sidebar";
import { ChatPanel } from "./components/Chat/ChatPanel";
import { SettingsDrawer } from "./components/Settings/SettingsDrawer";
import { useCoordinator } from "./hooks/useCoordinator";
import { apiFetch } from "./lib/api";
import type { Message, ThreadData, ThreadSummary } from "./lib/types";

export function App() {
  const { online, send, subscribe } = useCoordinator();
  const [threads, setThreads] = useState<ThreadSummary[]>([]);
  const [activeThreadId, setActiveThreadId] = useState<string | null>(null);
  const [activeThread, setActiveThread] = useState<ThreadData | null>(null);
  const [messages, setMessages] = useState<Message[]>([]);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(false);

  const refreshThreads = useCallback(async () => {
    try {
      const data = await apiFetch("/api/threads?limit=50");
      setThreads(data.threads || []);
    } catch {
      // silently ignore — agent may be offline
    }
  }, []);

  useEffect(() => {
    refreshThreads();
    const t = setInterval(refreshThreads, 30000);
    return () => clearInterval(t);
  }, [refreshThreads]);

  // When agent comes online, refresh the thread list so the user isn't
  // stuck looking at a stale "agent offline" screen.
  useEffect(() => {
    if (online) refreshThreads();
  }, [online, refreshThreads]);

  const selectThread = useCallback(async (id: string) => {
    setActiveThreadId(id);
    setActiveThread(null);
    setMessages([]);
    setSidebarOpen(false);
    try {
      const data = await apiFetch(`/api/threads/${id}`);
      setActiveThread(data);
      setMessages(data.messages || []);
    } catch (e) {
      console.warn("load thread:", e);
    }
  }, []);

  const newThread = useCallback(() => {
    setActiveThreadId(null);
    setActiveThread(null);
    setMessages([]);
    setSidebarOpen(false);
  }, []);

  const onMessageCommitted = useCallback((m: Message) => {
    setMessages((prev) => [...prev, m]);
  }, []);

  const onThreadCreated = useCallback(
    (id: string) => {
      setActiveThreadId(id);
      // Optimistically add to top of list, full data will come on next refresh.
      setThreads((prev) => {
        const exists = prev.find((t) => t.id === id);
        if (exists) return prev;
        return [
          { id, title: "Untitled", created: Date.now(), userLastInteractedAt: Date.now() },
          ...prev,
        ];
      });
      // Refresh shortly after to pick up workspace metadata.
      setTimeout(refreshThreads, 1500);
      setTimeout(async () => {
        try {
          const data = await apiFetch(`/api/threads/${id}`);
          setActiveThread(data);
        } catch {}
      }, 2000);
    },
    [refreshThreads],
  );

  const onTitleUpdated = useCallback(
    (title: string) => {
      if (!activeThreadId) return;
      setThreads((prev) =>
        prev.map((t) => (t.id === activeThreadId ? { ...t, title } : t)),
      );
      setActiveThread((prev) => (prev ? { ...prev, title } : prev));
    },
    [activeThreadId],
  );

  const onUserSent = useCallback((text: string) => {
    setMessages((prev) => [
      ...prev,
      { role: "user", content: [{ type: "text", text }], meta: { sentAt: Date.now() } },
    ]);
  }, []);

  return (
    <div className="app">
      <Sidebar
        threads={threads}
        activeThreadId={activeThreadId}
        online={online}
        open={sidebarOpen}
        onSelect={selectThread}
        onNewThread={newThread}
        onOpenSettings={() => setSettingsOpen(true)}
        onClose={() => setSidebarOpen(false)}
      />
      <ChatPanel
        activeThread={activeThread}
        activeThreadId={activeThreadId}
        messages={messages}
        online={online}
        send={send}
        subscribe={subscribe}
        onMessageCommitted={onMessageCommitted}
        onThreadCreated={onThreadCreated}
        onTitleUpdated={onTitleUpdated}
        onUserSent={onUserSent}
        onOpenSidebar={() => setSidebarOpen(true)}
      />
      <SettingsDrawer open={settingsOpen} onClose={() => setSettingsOpen(false)} />
    </div>
  );
}

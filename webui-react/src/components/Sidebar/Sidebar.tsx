import { Settings, Plus, X, Sparkles } from "lucide-react";
import type { ThreadSummary } from "../../lib/types";
import { ThreadList } from "./ThreadList";
import styles from "./Sidebar.module.css";

interface Props {
  threads: ThreadSummary[];
  activeThreadId: string | null;
  online: boolean;
  open: boolean;
  onSelect: (id: string) => void;
  onNewThread: () => void;
  onOpenSettings: () => void;
  onClose: () => void;
}

export function Sidebar({
  threads,
  activeThreadId,
  online,
  open,
  onSelect,
  onNewThread,
  onOpenSettings,
  onClose,
}: Props) {
  return (
    <aside className={`${styles.sidebar} ${open ? styles.open : ""}`}>
      <header className={styles.header}>
        <div className={styles.brand}>
          <Sparkles size={16} className={styles.brandIcon} />
          <h1>amp</h1>
        </div>
        <div className={styles.actions}>
          <button onClick={onOpenSettings} title="Settings" className={styles.iconBtn}>
            <Settings size={16} />
          </button>
          <button onClick={onClose} title="Close" className={`${styles.iconBtn} ${styles.mobileOnly}`}>
            <X size={16} />
          </button>
        </div>
      </header>

      <button onClick={onNewThread} className={styles.newThreadBtn}>
        <Plus size={14} /> New thread
      </button>

      <div className={styles.status} data-online={online}>
        <span className={styles.statusDot} />
        {online ? "Agent online" : "Agent offline"}
      </div>

      <ThreadList threads={threads} activeId={activeThreadId} onSelect={onSelect} />
    </aside>
  );
}

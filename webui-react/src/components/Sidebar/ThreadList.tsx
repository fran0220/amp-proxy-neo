import type { ThreadSummary } from "../../lib/types";
import styles from "./ThreadList.module.css";

interface Props {
  threads: ThreadSummary[];
  activeId: string | null;
  onSelect: (id: string) => void;
}

const modeColors: Record<string, string> = {
  smart: "#00d563",
  large: "#9b87f5",
  rush: "#f5a524",
  deep: "#1de9b6",
  frontier: "#ff7733",
};

export function ThreadList({ threads, activeId, onSelect }: Props) {
  return (
    <div className={styles.list}>
      {threads.length === 0 && <div className={styles.empty}>No threads yet</div>}
      {threads.map((t) => (
        <button
          key={t.id}
          onClick={() => onSelect(t.id)}
          className={`${styles.item} ${activeId === t.id ? styles.active : ""}`}
        >
          {t.agentMode && (
            <span
              className={styles.modeBadge}
              style={{ background: modeColors[t.agentMode] || "#999" }}
              title={t.agentMode}
            />
          )}
          <div className={styles.itemBody}>
            <div className={styles.title}>{t.title || "Untitled"}</div>
            <div className={styles.meta}>
              {formatTime(t.userLastInteractedAt || t.created)}
              {t.messageCount != null && (
                <span className={styles.metaSep}>· {t.messageCount} msg</span>
              )}
            </div>
          </div>
        </button>
      ))}
    </div>
  );
}

function formatTime(ts?: number): string {
  if (!ts) return "";
  const diff = (Date.now() - ts) / 1000;
  if (diff < 60) return "just now";
  if (diff < 3600) return Math.floor(diff / 60) + "m ago";
  if (diff < 86400) return Math.floor(diff / 3600) + "h ago";
  return Math.floor(diff / 86400) + "d ago";
}

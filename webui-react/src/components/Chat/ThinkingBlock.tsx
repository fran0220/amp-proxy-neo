import { useState } from "react";
import { ChevronRight, Brain } from "lucide-react";
import { MarkdownView } from "../Markdown/MarkdownView";
import styles from "./ToolCard.module.css";

interface Props {
  text: string;
  streaming?: boolean;
}

export function ThinkingBlock({ text, streaming }: Props) {
  const [open, setOpen] = useState(false);
  return (
    <div className={`${styles.card} ${styles.thinking} ${streaming ? styles.streaming : ""}`}>
      <button onClick={() => setOpen(!open)} className={styles.header}>
        <ChevronRight size={14} className={`${styles.chevron} ${open ? styles.open : ""}`} />
        <Brain size={13} className={styles.icon} />
        <span className={styles.name}>{streaming ? "thinking…" : "thinking"}</span>
      </button>
      {open && (
        <div className={styles.body}>
          <div className={styles.section}>
            <MarkdownView content={text} />
          </div>
        </div>
      )}
    </div>
  );
}

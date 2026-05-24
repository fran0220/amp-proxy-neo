import { useState } from "react";
import { ChevronRight, Wrench, Check, Clock } from "lucide-react";
import type { ContentBlock } from "../../lib/types";
import styles from "./ToolCard.module.css";

interface Props {
  toolUse: ContentBlock;
  result?: ContentBlock;
  status?: "pending" | "running" | "done" | "error";
}

export function ToolCard({ toolUse, result, status }: Props) {
  const [open, setOpen] = useState(false);
  const name = toolUse.name || "tool";
  const argsPreview = formatPreview(name, toolUse.input);
  const resolved = result || (toolUse as any).result;
  const actualStatus =
    status ||
    (resolved ? "done" : "pending");

  const resultText = resolved ? extractResultText(resolved.run) : "";
  const resultPreview = resultText.split("\n")[0]?.slice(0, 80);

  return (
    <div className={`${styles.card} ${styles[actualStatus]}`}>
      <button onClick={() => setOpen(!open)} className={styles.header}>
        <ChevronRight size={14} className={`${styles.chevron} ${open ? styles.open : ""}`} />
        <Wrench size={13} className={styles.icon} />
        <span className={styles.name}>{name}</span>
        {argsPreview && <span className={styles.preview}>{argsPreview}</span>}
        <span className={styles.status}>
          {actualStatus === "done" ? (
            <Check size={13} />
          ) : actualStatus === "running" ? (
            <span className={styles.spinner} />
          ) : (
            <Clock size={13} />
          )}
        </span>
      </button>
      {open && (
        <div className={styles.body}>
          <div className={styles.section}>
            <div className={styles.sectionLabel}>arguments</div>
            <pre className={styles.code}>{JSON.stringify(toolUse.input, null, 2)}</pre>
          </div>
          {resolved && (
            <div className={styles.section}>
              <div className={styles.sectionLabel}>result</div>
              <pre className={styles.code}>{resultText}</pre>
            </div>
          )}
        </div>
      )}
      {!open && resultPreview && actualStatus === "done" && (
        <div className={styles.resultLine}>{resultPreview}{resultText.length > 80 ? "…" : ""}</div>
      )}
    </div>
  );
}

function formatPreview(name: string, input: any): string {
  if (!input || typeof input !== "object") return "";
  switch (name) {
    case "Bash":
    case "shell_command":
      return String(input.cmd || input.command || "").slice(0, 80);
    case "Read":
    case "read_file":
      return String(input.path || input.file_path || "").slice(0, 80);
    case "Edit":
    case "edit_file":
    case "create_file":
    case "write_file":
      return String(input.path || input.file_path || "").slice(0, 80);
    case "Grep":
    case "grep":
      return String(input.pattern || input.query || "").slice(0, 80);
    case "Glob":
    case "glob":
      return String(input.pattern || "").slice(0, 80);
    default:
      // Pick first string-typed value as preview
      for (const k of Object.keys(input)) {
        const v = input[k];
        if (typeof v === "string" && v) return v.slice(0, 80);
      }
      return "";
  }
}

function extractResultText(run: any): string {
  if (!run) return "";
  if (typeof run === "string") return run;
  const r = run.result;
  if (typeof r === "string") return r;
  if (r && typeof r === "object") {
    if (typeof r.output === "string") return r.output;
    return JSON.stringify(r, null, 2);
  }
  return JSON.stringify(run, null, 2);
}

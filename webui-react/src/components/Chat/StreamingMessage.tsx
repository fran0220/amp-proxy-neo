import type { StreamingState } from "../../hooks/useStreamingChat";
import { MarkdownView } from "../Markdown/MarkdownView";
import { ToolCard } from "./ToolCard";
import { ThinkingBlock } from "./ThinkingBlock";
import styles from "./Message.module.css";

interface Props {
  state: StreamingState;
}

export function StreamingMessage({ state }: Props) {
  const tools = Array.from(state.toolCalls.values());
  return (
    <div className={`${styles.message} ${styles.assistant} ${styles.streaming}`}>
      <div className={styles.body}>
        {state.thinking && <ThinkingBlock text={state.thinking} streaming />}
        {state.text && <MarkdownView content={state.text} streaming />}
        {tools.map((t) => (
          <ToolCard
            key={t.id}
            toolUse={{ type: "tool_use", id: t.id, name: t.name, input: t.input }}
            result={
              t.status === "done"
                ? { type: "tool_result", run: { result: t.result, status: "done" } }
                : undefined
            }
            status={t.status}
          />
        ))}
        {state.active && <span className={styles.cursor}>▍</span>}
      </div>
    </div>
  );
}

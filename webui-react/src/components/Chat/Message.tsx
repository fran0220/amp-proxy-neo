import type { Message, ContentBlock } from "../../lib/types";
import { MarkdownView } from "../Markdown/MarkdownView";
import { ToolCard } from "./ToolCard";
import { ThinkingBlock } from "./ThinkingBlock";
import styles from "./Message.module.css";

interface Props {
  message: Message;
  toolResultsByUseID: Record<string, ContentBlock>;
}

export function MessageView({ message, toolResultsByUseID }: Props) {
  const isUser = message.role === "user";
  const isInfo = message.role === "info";
  const textParts = (message.content || []).filter((c) => c.type === "text");
  const toolUses = (message.content || []).filter((c) => c.type === "tool_use");
  const thinking = (message.content || []).filter((c) => c.type === "thinking");
  const text = textParts.map((c) => c.text || "").join("");

  return (
    <div className={`${styles.message} ${isUser ? styles.user : isInfo ? styles.info : styles.assistant}`}>
      <div className={styles.body}>
        {text && (
          isUser ? (
            <div className={styles.userText}>{text}</div>
          ) : (
            <MarkdownView content={text} />
          )
        )}
        {thinking.map((t, i) => (
          <ThinkingBlock key={`th-${i}`} text={t.thinking || ""} />
        ))}
        {toolUses.map((tu, i) => (
          <ToolCard
            key={tu.id || `tu-${i}`}
            toolUse={tu}
            result={tu.id ? toolResultsByUseID[tu.id] : undefined}
          />
        ))}
      </div>
    </div>
  );
}

import { memo } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";
import styles from "./MarkdownView.module.css";

interface Props {
  content: string;
  streaming?: boolean;
}

// Memoize so re-renders during streaming only re-parse when content changes.
export const MarkdownView = memo(function MarkdownView({ content, streaming }: Props) {
  return (
    <div className={`${styles.md} ${streaming ? styles.streaming : ""}`}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[[rehypeHighlight, { detect: true, ignoreMissing: true }]]}
        components={{
          a: ({ node, ...props }) => <a {...props} target="_blank" rel="noopener noreferrer" />,
        }}
      >
        {content}
      </ReactMarkdown>
    </div>
  );
});

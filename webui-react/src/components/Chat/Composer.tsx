import { useState, useRef, KeyboardEvent } from "react";
import { Send } from "lucide-react";
import styles from "./Composer.module.css";

interface Props {
  onSend: (text: string) => void;
  disabled: boolean;
  placeholder?: string;
}

export function Composer({ onSend, disabled, placeholder }: Props) {
  const [text, setText] = useState("");
  const ref = useRef<HTMLTextAreaElement>(null);

  const send = () => {
    const t = text.trim();
    if (!t || disabled) return;
    onSend(t);
    setText("");
    // Reset height
    if (ref.current) ref.current.style.height = "auto";
  };

  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      send();
    } else if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  };

  const autoResize = () => {
    const el = ref.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = Math.min(el.scrollHeight, 200) + "px";
  };

  return (
    <div className={styles.composer}>
      <textarea
        ref={ref}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          autoResize();
        }}
        onKeyDown={onKey}
        placeholder={placeholder || "Message…"}
        rows={1}
        disabled={disabled}
        className={styles.input}
      />
      <button onClick={send} disabled={disabled || !text.trim()} className={styles.sendBtn}>
        <Send size={16} />
      </button>
    </div>
  );
}

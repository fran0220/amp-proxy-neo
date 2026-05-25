import { useCallback, useEffect, useRef, useState } from "react";
import type { ServerFrame } from "../lib/types";

type Listener = (frame: ServerFrame) => void;

export function useChat() {
  const [online, setOnline] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const listenersRef = useRef<Set<Listener>>(new Set());
  const reconnectTimerRef = useRef<number | null>(null);
  const backoffRef = useRef(500);

  const connect = useCallback(() => {
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${protocol}//${window.location.host}/api/chat/ws`);
    wsRef.current = ws;
    ws.onopen = () => {
      setOnline(true);
      setError(null);
      backoffRef.current = 500;
    };
    ws.onclose = () => {
      setOnline(false);
      wsRef.current = null;
      const delay = Math.min(backoffRef.current, 8000);
      backoffRef.current = delay * 2;
      reconnectTimerRef.current = window.setTimeout(connect, delay);
    };
    ws.onerror = () => {
      setError("WebSocket connection failed");
    };
    ws.onmessage = (event) => {
      let frame: ServerFrame;
      try {
        frame = JSON.parse(event.data);
      } catch {
        return;
      }
      if (frame.type === "agent_online" && typeof frame.online === "boolean") {
        setOnline(frame.online);
      }
      listenersRef.current.forEach((listener) => listener(frame));
    };
  }, []);

  useEffect(() => {
    connect();
    return () => {
      if (reconnectTimerRef.current) clearTimeout(reconnectTimerRef.current);
      wsRef.current?.close();
    };
  }, [connect]);

  const send = useCallback((envelope: any) => {
    const ws = wsRef.current;
    if (ws?.readyState !== WebSocket.OPEN) return false;
    ws.send(JSON.stringify(envelope));
    return true;
  }, []);

  const cancel = useCallback((reqId: string | null) => {
    if (!reqId) return false;
    return send({ type: "cancel", reqId });
  }, [send]);

  const subscribe = useCallback((listener: Listener) => {
    listenersRef.current.add(listener);
    return () => listenersRef.current.delete(listener);
  }, []);

  return { online, error, send, cancel, subscribe };
}

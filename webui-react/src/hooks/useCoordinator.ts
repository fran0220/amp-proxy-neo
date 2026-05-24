// Maintains a WebSocket to the CF Worker coordinator with auto-reconnect.
// Exposes: status (online/offline), send(envelope), and onMessage subscription.

import { useEffect, useRef, useState, useCallback } from "react";
import { getCoordinatorURL, getClientSecret } from "../lib/api";
import type { ServerFrame } from "../lib/types";

type Listener = (frame: ServerFrame) => void;

export function useCoordinator() {
  const [online, setOnline] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const listenersRef = useRef<Set<Listener>>(new Set());
  const reconnectTimerRef = useRef<number | null>(null);

  const connect = useCallback(async () => {
    const url = await getCoordinatorURL();
    if (!url) return;
    const wssURL = url.replace(/^http/, "ws") + "/ws/client";
    const u = new URL(wssURL);
    const secret = getClientSecret();
    if (secret) u.searchParams.set("token", secret);
    try {
      const ws = new WebSocket(u.toString());
      wsRef.current = ws;
      ws.onopen = () => {
        // agent_online frame from server will set status; nothing to do here
      };
      ws.onclose = () => {
        setOnline(false);
        wsRef.current = null;
        reconnectTimerRef.current = window.setTimeout(connect, 2000);
      };
      ws.onerror = () => {
        // close handler will fire
      };
      ws.onmessage = (e) => {
        let parsed: ServerFrame;
        try {
          parsed = JSON.parse(e.data);
        } catch {
          return;
        }
        if (parsed.type === "agent_online" && typeof parsed.online === "boolean") {
          setOnline(parsed.online);
        }
        listenersRef.current.forEach((l) => l(parsed));
      };
    } catch (e) {
      reconnectTimerRef.current = window.setTimeout(connect, 2000);
    }
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
    if (ws?.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(envelope));
      return true;
    }
    return false;
  }, []);

  const subscribe = useCallback((listener: Listener) => {
    listenersRef.current.add(listener);
    return () => {
      listenersRef.current.delete(listener);
    };
  }, []);

  return { online, send, subscribe };
}

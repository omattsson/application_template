import { useEffect, useRef, useState, useCallback } from 'react';
import ReconnectingWebSocket from 'reconnecting-websocket';
import { WS_BASE_URL } from '../api/config';

export type ConnectionStatus = 'connecting' | 'open' | 'closing' | 'closed';

/** Shape of every message the backend sends over the WebSocket. */
export interface WebSocketMessage {
  type: string;
  payload: unknown;
}

export interface UseWebSocketOptions {
  /** Called whenever a message arrives, after JSON parsing. */
  onMessage?: (message: WebSocketMessage) => void;
  /** URL path, e.g. '/ws'. Defaults to '/ws'. */
  path?: string;
  /**
   * Access token for an authenticated socket, sent as `?token=` (browsers
   * cannot set headers on a WebSocket; the backend strips it before logging).
   * A new token closes the socket and opens a new one, so the socket always
   * carries the current identity (the server closes it at token expiry).
   */
  token?: string | null;
  /** False: no socket (for example auth on but nobody signed in). Defaults to true. */
  enabled?: boolean;
}

/**
 * Build the socket URL.
 * @param path - URL path (may have a query)
 * @param token - Access token, or null/undefined for none
 * @returns The absolute ws:// or wss:// URL
 */
export function webSocketUrl(path: string, token?: string | null): string {
  const base = `${WS_BASE_URL}${path}`;
  if (!token) return base;
  return `${base}${path.includes('?') ? '&' : '?'}token=${encodeURIComponent(token)}`;
}

export interface UseWebSocketResult {
  lastMessage: WebSocketMessage | null;
  connectionStatus: ConnectionStatus;
  /**
   * How many times a socket opened after the first open since the socket was
   * last wanted (a sign-out resets that): a reconnect, or the new socket of a
   * new token. Messages may have been missed
   * in between, so a page that shows live data loads it again when this grows.
   */
  reconnectCount: number;
  sendMessage: (data: unknown) => void;
}

export function useWebSocket(options: UseWebSocketOptions = {}): UseWebSocketResult {
  const { onMessage, path = '/ws', token = null, enabled = true } = options;
  const wsRef = useRef<ReconnectingWebSocket | null>(null);
  const [lastMessage, setLastMessage] = useState<WebSocketMessage | null>(null);
  const [connectionStatus, setConnectionStatus] = useState<ConnectionStatus>('connecting');
  const [reconnectCount, setReconnectCount] = useState(0);
  const hasOpenedRef = useRef(false);
  const onMessageRef = useRef(onMessage);

  // Keep the callback ref up-to-date without re-creating the socket
  useEffect(() => {
    onMessageRef.current = onMessage;
  }, [onMessage]);

  // One socket per (path, token): a refreshed token reconnects, a logout closes.
  useEffect(() => {
    if (!enabled) {
      wsRef.current = null;
      // The next open after a sign-in is a first open, not a reconnect, and
      // the status must not show the old `open` until the new socket starts.
      hasOpenedRef.current = false;
      setConnectionStatus('connecting');
      return;
    }
    const rws = new ReconnectingWebSocket(webSocketUrl(path, token));
    wsRef.current = rws;
    setConnectionStatus('connecting');

    const handleOpen = () => {
      setConnectionStatus('open');
      if (hasOpenedRef.current) setReconnectCount((count) => count + 1);
      hasOpenedRef.current = true;
    };
    const handleClose = () => setConnectionStatus('closed');
    const handleMessage = (event: MessageEvent) => {
      try {
        const parsed: WebSocketMessage = JSON.parse(event.data as string);
        setLastMessage(parsed);
        onMessageRef.current?.(parsed);
      } catch {
        // Ignore non-JSON frames
      }
    };

    rws.addEventListener('open', handleOpen);
    rws.addEventListener('close', handleClose);
    rws.addEventListener('message', handleMessage);

    return () => {
      rws.removeEventListener('open', handleOpen);
      rws.removeEventListener('close', handleClose);
      rws.removeEventListener('message', handleMessage);
      rws.close();
      if (wsRef.current === rws) wsRef.current = null;
    };
  }, [path, token, enabled]);

  const sendMessage = useCallback((data: unknown) => {
    wsRef.current?.send(JSON.stringify(data));
  }, []);

  return { lastMessage, connectionStatus: enabled ? connectionStatus : 'closed', reconnectCount, sendMessage };
}

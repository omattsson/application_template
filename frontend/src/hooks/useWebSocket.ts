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
}

export interface UseWebSocketResult {
  lastMessage: WebSocketMessage | null;
  connectionStatus: ConnectionStatus;
  sendMessage: (data: unknown) => void;
}

export function useWebSocket(options: UseWebSocketOptions = {}): UseWebSocketResult {
  const { onMessage, path = '/ws' } = options;
  const wsRef = useRef<ReconnectingWebSocket | null>(null);
  const [lastMessage, setLastMessage] = useState<WebSocketMessage | null>(null);
  const [connectionStatus, setConnectionStatus] = useState<ConnectionStatus>('connecting');
  const onMessageRef = useRef(onMessage);

  // Keep the callback ref up-to-date without re-creating the socket
  useEffect(() => {
    onMessageRef.current = onMessage;
  }, [onMessage]);

  useEffect(() => {
    const url = `${WS_BASE_URL}${path}`;
    const rws = new ReconnectingWebSocket(url);
    wsRef.current = rws;

    const handleOpen = () => setConnectionStatus('open');
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
    };
  }, [path]);

  const sendMessage = useCallback((data: unknown) => {
    wsRef.current?.send(JSON.stringify(data));
  }, []);

  return { lastMessage, connectionStatus, sendMessage };
}

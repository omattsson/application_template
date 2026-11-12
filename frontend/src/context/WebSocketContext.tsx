import React, { createContext, useContext, useCallback, useRef, useSyncExternalStore } from 'react';
import { useWebSocket } from '../hooks/useWebSocket';
import type { WebSocketMessage, ConnectionStatus } from '../hooks/useWebSocket';
import { getSession, subscribeSession } from '../api/session';

interface WebSocketContextValue {
  lastMessage: WebSocketMessage | null;
  connectionStatus: ConnectionStatus;
  subscribe: (type: string, handler: (msg: WebSocketMessage) => void) => () => void;
}

const WebSocketContext = createContext<WebSocketContextValue | null>(null);

export function WebSocketProvider({ children }: { children: React.ReactNode }) {
  const handlersRef = useRef<Map<string, Set<(msg: WebSocketMessage) => void>>>(new Map());

  const handleMessage = useCallback((msg: WebSocketMessage) => {
    handlersRef.current.get(msg.type)?.forEach((h) => h(msg));
  }, []);

  // The socket follows the session: none until the auth mode is known, none
  // for an anonymous visitor with auth on, and a new socket for every new
  // access token (login, refresh, a token from another tab).
  const session = useSyncExternalStore(subscribeSession, getSession);
  const token = session.mode === 'enabled' ? session.token : null;
  const enabled = session.mode === 'disabled' || (session.mode === 'enabled' && token !== null);

  const { lastMessage, connectionStatus } = useWebSocket({ onMessage: handleMessage, token, enabled });

  const subscribe = useCallback(
    (type: string, handler: (msg: WebSocketMessage) => void) => {
      const map = handlersRef.current;
      if (!map.has(type)) map.set(type, new Set());
      map.get(type)!.add(handler);
      return () => {
        map.get(type)?.delete(handler);
      };
    },
    []
  );

  return (
    <WebSocketContext.Provider value={{ lastMessage, connectionStatus, subscribe }}>
      {children}
    </WebSocketContext.Provider>
  );
}

export function useWebSocketContext(): WebSocketContextValue {
  const ctx = useContext(WebSocketContext);
  if (!ctx) throw new Error('useWebSocketContext must be used within <WebSocketProvider>');
  return ctx;
}

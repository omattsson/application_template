import React, { createContext, useContext, useCallback, useRef } from 'react';
import { useWebSocket } from '../hooks/useWebSocket';
import type { WebSocketMessage, ConnectionStatus } from '../hooks/useWebSocket';

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

  const { lastMessage, connectionStatus } = useWebSocket({ onMessage: handleMessage });

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

import { describe, it, expect, vi, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import React from 'react';

interface MockWSInstance extends EventTarget {
  open: () => void;
  close: () => void;
  receive: (data: string) => void;
}

// vi.hoisted ensures the class is defined before vi.mock hoists the factory
const { MockRWS, getInstance } = vi.hoisted(() => {
  let _instance: MockWSInstance | null = null;

  class MockRWS extends EventTarget implements MockWSInstance {
    constructor() {
      super();
      _instance = this;
    }
    close() { this.dispatchEvent(new CloseEvent('close')); }
    open() { this.dispatchEvent(new Event('open')); }
    receive(data: string) { this.dispatchEvent(new MessageEvent('message', { data })); }
  }

  return { MockRWS, getInstance: (): MockWSInstance => _instance! };
});

vi.mock('reconnecting-websocket', () => ({ default: MockRWS }));

import { WebSocketProvider, useWebSocketContext } from '../context/WebSocketContext';

function wrapper({ children }: { children: React.ReactNode }) {
  return React.createElement(WebSocketProvider, null, children);
}

describe('WebSocketContext', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('throws when used outside provider', () => {
    // Suppress expected console.error from React
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => renderHook(() => useWebSocketContext())).toThrow(
      'useWebSocketContext must be used within <WebSocketProvider>'
    );
    consoleSpy.mockRestore();
  });

  it('starts in connecting status', () => {
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });
    expect(result.current.connectionStatus).toBe('connecting');
  });

  it('updates connectionStatus to open when socket opens', () => {
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });
    act(() => getInstance().open());
    expect(result.current.connectionStatus).toBe('open');
  });

  it('exposes lastMessage after a message is received', () => {
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });
    act(() =>
      getInstance().receive('{"type":"item.created","payload":{"id":42}}')
    );
    expect(result.current.lastMessage).toEqual({
      type: 'item.created',
      payload: { id: 42 },
    });
  });

  it('subscribe receives events of the subscribed type', () => {
    const handler = vi.fn();
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });

    act(() => {
      result.current.subscribe('item.created', handler);
    });

    act(() =>
      getInstance().receive('{"type":"item.created","payload":{"id":1}}')
    );

    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler).toHaveBeenCalledWith({ type: 'item.created', payload: { id: 1 } });
  });

  it('subscribe does not fire for other message types', () => {
    const handler = vi.fn();
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });

    act(() => {
      result.current.subscribe('item.created', handler);
    });

    act(() =>
      getInstance().receive('{"type":"item.deleted","payload":{"id":1}}')
    );

    expect(handler).not.toHaveBeenCalled();
  });

  it('unsubscribe returned from subscribe deregisters the handler', () => {
    const handler = vi.fn();
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });

    let unsubscribe!: () => void;
    act(() => {
      unsubscribe = result.current.subscribe('item.updated', handler);
    });

    act(() => unsubscribe());

    act(() =>
      getInstance().receive('{"type":"item.updated","payload":{}}')
    );

    expect(handler).not.toHaveBeenCalled();
  });

  it('updates connectionStatus to closed when socket closes', () => {
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });
    act(() => getInstance().open());
    expect(result.current.connectionStatus).toBe('open');

    act(() => getInstance().close());
    expect(result.current.connectionStatus).toBe('closed');
  });

  it('ignores non-JSON frames without updating lastMessage', () => {
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });
    act(() => getInstance().receive('not valid json at all'));
    expect(result.current.lastMessage).toBeNull();
  });

  it('calls all subscribers when multiple handlers are registered for the same type', () => {
    const handlerA = vi.fn();
    const handlerB = vi.fn();
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });

    act(() => {
      result.current.subscribe('item.created', handlerA);
      result.current.subscribe('item.created', handlerB);
    });

    act(() =>
      getInstance().receive('{"type":"item.created","payload":{"id":2}}')
    );

    expect(handlerA).toHaveBeenCalledTimes(1);
    expect(handlerB).toHaveBeenCalledTimes(1);
    expect(handlerA).toHaveBeenCalledWith({ type: 'item.created', payload: { id: 2 } });
    expect(handlerB).toHaveBeenCalledWith({ type: 'item.created', payload: { id: 2 } });
  });

  it('unsubscribe only removes the specific handler leaving others intact', () => {
    const handlerA = vi.fn();
    const handlerB = vi.fn();
    const { result } = renderHook(() => useWebSocketContext(), { wrapper });

    let unsubscribeA!: () => void;
    act(() => {
      unsubscribeA = result.current.subscribe('item.deleted', handlerA);
      result.current.subscribe('item.deleted', handlerB);
    });

    act(() => unsubscribeA());

    act(() =>
      getInstance().receive('{"type":"item.deleted","payload":{"id":5}}')
    );

    expect(handlerA).not.toHaveBeenCalled();
    expect(handlerB).toHaveBeenCalledTimes(1);
    expect(handlerB).toHaveBeenCalledWith({ type: 'item.deleted', payload: { id: 5 } });
  });
});

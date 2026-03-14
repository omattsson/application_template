import { describe, it, expect, vi, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';

interface MockWSInstance extends EventTarget {
  url: string;
  sent: string[];
  open: () => void;
  close: () => void;
  receive: (data: string) => void;
  send: (data: string) => void;
}

// vi.hoisted ensures the class is defined before vi.mock hoists the factory
const { MockRWS, getInstance } = vi.hoisted(() => {
  let _instance: MockWSInstance | null = null;

  class MockRWS extends EventTarget implements MockWSInstance {
    url: string;
    sent: string[] = [];
    constructor(url: string) {
      super();
      _instance = this;
      this.url = url;
    }
    send(data: string) { this.sent.push(data); }
    close() { this.dispatchEvent(new CloseEvent('close')); }
    open() { this.dispatchEvent(new Event('open')); }
    receive(data: string) { this.dispatchEvent(new MessageEvent('message', { data })); }
  }

  return { MockRWS, getInstance: (): MockWSInstance => _instance! };
});

vi.mock('reconnecting-websocket', () => ({ default: MockRWS }));

import { useWebSocket } from '../hooks/useWebSocket';

describe('useWebSocket', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('starts in connecting status', () => {
    const { result } = renderHook(() => useWebSocket());
    expect(result.current.connectionStatus).toBe('connecting');
  });

  it('updates status to open when socket opens', () => {
    const { result } = renderHook(() => useWebSocket());
    act(() => getInstance().open());
    expect(result.current.connectionStatus).toBe('open');
  });

  it('updates status to closed when socket closes', () => {
    const { result } = renderHook(() => useWebSocket());
    act(() => getInstance().open());
    act(() => getInstance().close());
    expect(result.current.connectionStatus).toBe('closed');
  });

  it('parses and exposes lastMessage', () => {
    const { result } = renderHook(() => useWebSocket());
    act(() => getInstance().receive('{"type":"item.created","payload":{"id":1}}'));
    expect(result.current.lastMessage).toEqual({ type: 'item.created', payload: { id: 1 } });
  });

  it('calls onMessage callback', () => {
    const onMessage = vi.fn();
    renderHook(() => useWebSocket({ onMessage }));
    act(() => getInstance().receive('{"type":"item.updated","payload":{}}'));
    expect(onMessage).toHaveBeenCalledWith({ type: 'item.updated', payload: {} });
  });

  it('ignores non-JSON frames', () => {
    const onMessage = vi.fn();
    const { result } = renderHook(() => useWebSocket({ onMessage }));
    act(() => getInstance().receive('not-valid-json'));
    expect(result.current.lastMessage).toBeNull();
    expect(onMessage).not.toHaveBeenCalled();
  });

  it('sendMessage serialises and sends data', () => {
    const { result } = renderHook(() => useWebSocket());
    act(() => result.current.sendMessage({ hello: 'world' }));
    expect(getInstance().sent).toContain('{"hello":"world"}');
  });

  it('closes the socket on unmount', () => {
    const { unmount } = renderHook(() => useWebSocket());
    const closeSpy = vi.spyOn(getInstance(), 'close');
    unmount();
    expect(closeSpy).toHaveBeenCalled();
  });

  it('uses the custom path option in the WebSocket URL', () => {
    renderHook(() => useWebSocket({ path: '/ws/events' }));
    expect(getInstance().url).toContain('/ws/events');
  });

  it('does not recreate the socket when onMessage callback changes between renders', () => {
    const onMessage1 = vi.fn();
    const onMessage2 = vi.fn();
    const { rerender } = renderHook(
      ({ cb }: { cb: (msg: { type: string; payload: unknown }) => void }) =>
        useWebSocket({ onMessage: cb }),
      { initialProps: { cb: onMessage1 } }
    );
    const firstInstance = getInstance();
    const closeSpy = vi.spyOn(firstInstance, 'close');

    rerender({ cb: onMessage2 });

    // Socket must not be closed and re-created
    expect(closeSpy).not.toHaveBeenCalled();
    expect(getInstance()).toBe(firstInstance);

    // And the NEW callback should be invoked (ref is up-to-date)
    act(() => getInstance().receive('{"type":"item.updated","payload":{}}'));
    expect(onMessage2).toHaveBeenCalledTimes(1);
    expect(onMessage1).not.toHaveBeenCalled();
  });

  it('returns connectionStatus to open after a reconnect', () => {
    const { result } = renderHook(() => useWebSocket());
    act(() => getInstance().open());
    expect(result.current.connectionStatus).toBe('open');

    act(() => getInstance().close());
    expect(result.current.connectionStatus).toBe('closed');

    // Simulate ReconnectingWebSocket successfully re-establishing the connection
    act(() => getInstance().open());
    expect(result.current.connectionStatus).toBe('open');
  });
});

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, act } from '@testing-library/react';

interface MockSocket {
  url: string;
  closed: boolean;
}

const { MockRWS, sockets } = vi.hoisted(() => {
  const sockets: MockSocket[] = [];
  class MockRWS extends EventTarget implements MockSocket {
    url: string;
    closed = false;
    constructor(url: string) {
      super();
      this.url = url;
      // Test mock: record every socket the hook opens.
      sockets.push(this);
    }
    send() {}
    close() {
      this.closed = true;
      this.dispatchEvent(new CloseEvent('close'));
    }
  }
  return { MockRWS, sockets };
});

vi.mock('reconnecting-websocket', () => ({ default: MockRWS }));

import { WebSocketProvider } from '../context/WebSocketContext';
import { setAuthMode, startSession, endSession, resetSessionForTesting, applyRefreshedToken, sessionEpoch } from '../api/session';
import { tokenFor } from '../test/mockHttp';

const open = () => sockets.filter((s) => !s.closed);
const renderProvider = () => render(<WebSocketProvider>{null}</WebSocketProvider>);

describe('WebSocket and auth', () => {
  afterEach(() => {
    resetSessionForTesting();
    sockets.length = 0;
    vi.clearAllMocks();
  });

  it('does not connect before the auth mode is known', () => {
    renderProvider();
    expect(sockets).toHaveLength(0);
  });

  it('auth disabled: connects without a token', () => {
    setAuthMode('disabled');
    renderProvider();
    expect(open()).toHaveLength(1);
    expect(open()[0].url).toMatch(/\/ws$/);
    expect(open()[0].url).not.toContain('token');
  });

  it('auth enabled: does not connect anonymously', () => {
    setAuthMode('enabled');
    renderProvider();
    expect(sockets).toHaveLength(0);
  });

  it('auth enabled: connects with the access token in ?token=', () => {
    setAuthMode('enabled');
    const token = tokenFor('alice');
    startSession(token, null);
    renderProvider();
    expect(open()).toHaveLength(1);
    expect(open()[0].url).toContain(`/ws?token=${encodeURIComponent(token)}`);
  });

  it('reconnects with the new token after each refresh', () => {
    setAuthMode('enabled');
    const first = tokenFor('alice', 'user', 900, { jti: 'first' });
    startSession(first, null);
    renderProvider();
    const firstSocket = open()[0];

    const second = tokenFor('alice', 'user', 900, { jti: 'second' });
    act(() => {
      applyRefreshedToken(second, sessionEpoch());
    });

    expect(firstSocket.closed).toBe(true);
    expect(open()).toHaveLength(1);
    expect(open()[0].url).toContain(encodeURIComponent(second));

    const third = tokenFor('alice', 'user', 900, { jti: 'third' });
    act(() => {
      applyRefreshedToken(third, sessionEpoch());
    });
    expect(open()).toHaveLength(1);
    expect(open()[0].url).toContain(encodeURIComponent(third));
  });

  it('logout closes the socket and does not reconnect anonymously', () => {
    setAuthMode('enabled');
    startSession(tokenFor('alice'), null);
    renderProvider();
    expect(open()).toHaveLength(1);

    act(() => endSession());

    expect(open()).toHaveLength(0);
    expect(sockets).toHaveLength(1);
  });
});

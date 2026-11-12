import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { waitFor } from '@testing-library/react';
import {
  authApi,
  refreshAccessToken,
  isAuthRejection,
  REFRESH_LOCK_NAME,
  resetRefreshForTesting,
  SessionChangedError,
} from '../refresh';
import {
  getSession,
  setAuthMode,
  startSession,
  endSession,
  resetSessionForTesting,
  subscribeSession,
  SESSION_CHANNEL_NAME,
} from '../session';
import { deferred, headerOf, installMockBackend, pathOf, tokenFor } from '../../test/mockHttp';
import type { MockReply } from '../../test/mockHttp';

type LockCallback = (lock: unknown) => Promise<unknown>;

/** A Web Locks stand-in: one exclusive lock per name, granted in request order. */
function installFakeLocks() {
  let tail: Promise<unknown> = Promise.resolve();
  const request = vi.fn((_name: string, callback: LockCallback) => {
    const run = tail.then(() => callback({ name: _name }));
    tail = run.then(
      () => undefined,
      () => undefined,
    );
    return run;
  });
  /** Simulate another tab that holds the lock until the returned function is called. */
  const holdByOtherTab = () => {
    const held = deferred<void>();
    tail = tail.then(() => held.promise);
    return () => held.resolve();
  };
  Object.defineProperty(navigator, 'locks', { configurable: true, value: { request } });
  return { request, holdByOtherTab };
}

function removeLocks() {
  Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined });
}

describe('refreshAccessToken', () => {
  beforeEach(() => {
    setAuthMode('enabled');
  });

  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    removeLocks();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('posts {} with X-Requested-With and Content-Type application/json, with credentials', async () => {
    const fresh = tokenFor('alice');
    const adapter = installMockBackend([authApi], () => ({ status: 200, data: { token: fresh } }));

    await expect(refreshAccessToken()).resolves.toBe(fresh);

    expect(adapter).toHaveBeenCalledTimes(1);
    const config = adapter.mock.calls[0][0];
    expect(config.method).toBe('post');
    expect(pathOf(config)).toBe('/api/v1/auth/refresh');
    expect(config.baseURL).toBe('/api');
    expect(config.data).toBe('{}');
    expect(headerOf(config, 'X-Requested-With')).toBe('XMLHttpRequest');
    expect(headerOf(config, 'Content-Type')).toMatch(/^application\/json/);
    expect(config.withCredentials).toBe(true);
    expect(getSession().token).toBe(fresh);
    expect(getSession().user?.username).toBe('alice');
  });

  it('can never be aborted: no AbortSignal, no cancel token and no timeout', async () => {
    const adapter = installMockBackend([authApi], () => ({ status: 200, data: { token: tokenFor('alice') } }));
    await refreshAccessToken();
    const config = adapter.mock.calls[0][0];
    expect(config.signal).toBeUndefined();
    expect(config.cancelToken).toBeUndefined();
    expect(config.timeout ?? 0).toBe(0);
  });

  it('shares one request between concurrent callers in one tab', async () => {
    const reply = deferred<MockReply>();
    const fresh = tokenFor('alice');
    const adapter = installMockBackend([authApi], () => reply.promise);

    const a = refreshAccessToken();
    const b = refreshAccessToken();
    reply.resolve({ status: 200, data: { token: fresh } });

    await expect(Promise.all([a, b])).resolves.toEqual([fresh, fresh]);
    expect(adapter).toHaveBeenCalledTimes(1);
  });

  it('holds the cross-tab Web Lock while it calls the API', async () => {
    const { request } = installFakeLocks();
    installMockBackend([authApi], () => ({ status: 200, data: { token: tokenFor('alice') } }));

    await refreshAccessToken();

    expect(request).toHaveBeenCalledTimes(1);
    expect(request.mock.calls[0][0]).toBe(REFRESH_LOCK_NAME);
  });

  it('waits for another tab that holds the lock and reuses the token it broadcast instead of refreshing again', async () => {
    const { holdByOtherTab } = installFakeLocks();
    const old = tokenFor('alice', 'user', -5);
    startSession(old, null);
    const adapter = installMockBackend([authApi], () => ({ status: 200, data: { token: tokenFor('alice') } }));
    const otherTab = new BroadcastChannel(SESSION_CHANNEL_NAME);

    const release = holdByOtherTab();
    const pending = refreshAccessToken();

    // The other tab refreshed and shares its new token, then releases the lock.
    const fromOtherTab = tokenFor('alice', 'user', 900, { jti: 'other-tab' });
    otherTab.postMessage({ type: 'token', token: fromOtherTab });
    await waitFor(() => expect(getSession().token).toBe(fromOtherTab));
    release();

    await expect(pending).resolves.toBe(fromOtherTab);
    expect(adapter).not.toHaveBeenCalled();
    otherTab.close();
  });

  it('refreshes after waiting for the lock when no other tab shared a token (the cookie was rotated by then)', async () => {
    const { holdByOtherTab } = installFakeLocks();
    const fresh = tokenFor('alice');
    const adapter = installMockBackend([authApi], () => ({ status: 200, data: { token: fresh } }));

    const release = holdByOtherTab();
    const pending = refreshAccessToken();
    await Promise.resolve();
    expect(adapter).not.toHaveBeenCalled();
    release();

    await expect(pending).resolves.toBe(fresh);
    expect(adapter).toHaveBeenCalledTimes(1);
  });

  it('broadcasts the new token to the other tabs', async () => {
    const fresh = tokenFor('alice');
    installMockBackend([authApi], () => ({ status: 200, data: { token: fresh } }));
    const otherTab = new BroadcastChannel(SESSION_CHANNEL_NAME);
    const received = new Promise((resolve) => {
      otherTab.onmessage = (event: MessageEvent) => resolve(event.data);
    });

    await refreshAccessToken();

    await expect(received).resolves.toEqual({ type: 'token', token: fresh });
    otherTab.close();
  });

  it('returns the current token without a request when it is newer than the rejected one', async () => {
    const current = tokenFor('alice');
    startSession(current, null);
    const adapter = installMockBackend([authApi], () => ({ status: 200, data: { token: tokenFor('alice') } }));

    await expect(refreshAccessToken(tokenFor('alice', 'user', 900, { jti: 'older' }))).resolves.toBe(current);
    expect(adapter).not.toHaveBeenCalled();
  });

  it('works without Web Locks (in-tab single flight only)', async () => {
    removeLocks();
    const fresh = tokenFor('alice');
    const adapter = installMockBackend([authApi], () => ({ status: 200, data: { token: fresh } }));
    await expect(Promise.all([refreshAccessToken(), refreshAccessToken()])).resolves.toEqual([fresh, fresh]);
    expect(adapter).toHaveBeenCalledTimes(1);
  });

  it('ends the session when the server rejects the refresh (401)', async () => {
    startSession(tokenFor('alice', 'user', -5), null);
    installMockBackend([authApi], () => ({ status: 401, data: { error: 'Invalid session' } }));

    await expect(refreshAccessToken()).rejects.toSatisfy(isAuthRejection);
    expect(getSession().token).toBeNull();
    expect(getSession().user).toBeNull();
  });

  it('keeps the session on a network error or a 5xx (a later request retries)', async () => {
    const old = tokenFor('alice', 'user', -5);
    startSession(old, null);
    installMockBackend([authApi], () => ({ networkError: true }));
    await expect(refreshAccessToken()).rejects.toBeDefined();
    expect(getSession().token).toBe(old);

    installMockBackend([authApi], () => ({ status: 503 }));
    await expect(refreshAccessToken()).rejects.toBeDefined();
    expect(getSession().token).toBe(old);
  });

  it('rejects a 200 answer without a token and keeps the state unchanged', async () => {
    installMockBackend([authApi], () => ({ status: 200, data: '<html>' }));
    await expect(refreshAccessToken()).rejects.toThrow(/token/i);
    expect(getSession().token).toBeNull();
  });

  it('discards the result of a refresh that finishes after a logout', async () => {
    const reply = deferred<MockReply>();
    startSession(tokenFor('alice', 'user', -5), null);
    installMockBackend([authApi], () => reply.promise);

    const pending = refreshAccessToken();
    endSession();
    reply.resolve({ status: 200, data: { token: tokenFor('alice') } });

    await expect(pending).rejects.toThrow(/ended/i);
    expect(getSession().token).toBeNull();
  });

  it('sends no request when the session ended (logout in another tab) while waiting for the lock', async () => {
    const { holdByOtherTab } = installFakeLocks();
    startSession(tokenFor('alice', 'user', -5), null);
    const adapter = installMockBackend([authApi], () => ({ status: 200, data: { token: tokenFor('alice') } }));
    const otherTab = new BroadcastChannel(SESSION_CHANNEL_NAME);

    const release = holdByOtherTab();
    const pending = refreshAccessToken();
    otherTab.postMessage({ type: 'logout' });
    await waitFor(() => expect(getSession().token).toBeNull());
    release();

    await expect(pending).rejects.toBeInstanceOf(SessionChangedError);
    expect(adapter).not.toHaveBeenCalled();
    expect(getSession().token).toBeNull();
    otherTab.close();
  });

  it('sends no request when a login in this tab happened while waiting for the lock', async () => {
    const { holdByOtherTab } = installFakeLocks();
    const adapter = installMockBackend([authApi], () => ({ status: 200, data: { token: tokenFor('alice') } }));

    const release = holdByOtherTab();
    const pending = refreshAccessToken();
    const loggedIn = tokenFor('bob');
    startSession(loggedIn, null);
    release();

    // The login's token is newer and usable: it is returned, nothing is sent.
    await expect(pending).resolves.toBe(loggedIn);
    expect(adapter).not.toHaveBeenCalled();
    expect(getSession().token).toBe(loggedIn);
  });

  it('does not call the API when auth is disabled', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([authApi], () => ({ status: 200, data: { token: tokenFor('alice') } }));
    await expect(refreshAccessToken()).rejects.toThrow(/disabled/i);
    expect(adapter).not.toHaveBeenCalled();
  });
});

describe('isAuthRejection', () => {
  it('is true for 401 and 403 answers only', () => {
    expect(isAuthRejection({ response: { status: 401 } })).toBe(true);
    expect(isAuthRejection({ response: { status: 403 } })).toBe(true);
    expect(isAuthRejection({ response: { status: 500 } })).toBe(false);
    expect(isAuthRejection(new Error('network'))).toBe(false);
    expect(isAuthRejection(null)).toBe(false);
  });
});

describe('session broadcast from other tabs', () => {
  afterEach(() => {
    resetSessionForTesting();
    vi.clearAllMocks();
  });

  it('adopts a token and ends the session on a logout message', async () => {
    setAuthMode('enabled');
    const otherTab = new BroadcastChannel(SESSION_CHANNEL_NAME);
    const shared = tokenFor('carol', 'admin');

    otherTab.postMessage({ type: 'token', token: shared });
    await waitFor(() => expect(getSession().token).toBe(shared));
    expect(getSession().user?.role).toBe('admin');

    otherTab.postMessage({ type: 'logout' });
    await waitFor(() => expect(getSession().token).toBeNull());
    otherTab.close();
  });

  it('ignores malformed, expired and unknown messages', async () => {
    setAuthMode('enabled');
    const otherTab = new BroadcastChannel(SESSION_CHANNEL_NAME);
    const marker = tokenFor('dave');
    const emits: Array<string | null> = [];
    const unsubscribe = subscribeSession(() => emits.push(getSession().token));
    otherTab.postMessage({ type: 'token', token: 'garbage' });
    otherTab.postMessage({ type: 'token', token: tokenFor('eve', 'user', -5) });
    otherTab.postMessage({ type: 'token', token: 42 });
    otherTab.postMessage({ type: 'something' });
    otherTab.postMessage('text');
    otherTab.postMessage(null);
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(getSession().token).toBeNull();
    expect(emits).toEqual([]);

    // Messages arrive in order: the marker is the first (and only) accepted one.
    otherTab.postMessage({ type: 'token', token: marker });
    await waitFor(() => expect(getSession().token).toBe(marker));
    expect(emits).toEqual([marker]);
    unsubscribe();
    otherTab.close();
  });

  it('uses a namespaced channel name', () => {
    expect(SESSION_CHANNEL_NAME).toBe('app-template:auth-session:v1');
  });

  it('ignores broadcasts while auth is disabled', async () => {
    setAuthMode('disabled');
    const otherTab = new BroadcastChannel(SESSION_CHANNEL_NAME);
    otherTab.postMessage({ type: 'token', token: tokenFor('frank') });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(getSession().token).toBeNull();
    otherTab.close();
  });
});

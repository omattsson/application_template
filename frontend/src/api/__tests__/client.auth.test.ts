import { describe, it, expect, afterEach, vi } from 'vitest';
import api, { authService, itemService, uiConfigService } from '../client';
import { authApi, resetRefreshForTesting } from '../refresh';
import { getSession, setAuthMode, startSession, resetSessionForTesting } from '../session';
import { deferred, headerOf, installMockBackend, pathOf, reachableText, tokenFor } from '../../test/mockHttp';
import type { MockReply } from '../../test/mockHttp';
import type { InternalAxiosRequestConfig } from 'axios';

const isRefresh = (config: InternalAxiosRequestConfig) => pathOf(config) === '/api/v1/auth/refresh';

describe('API client with auth', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('auth disabled: sends no Authorization header and never refreshes on 401', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('disabled');
    const adapter = installMockBackend([api, authApi], (config) =>
      pathOf(config) === '/api/v1/items' ? { status: 401 } : { status: 200, data: [] },
    );

    await expect(itemService.list()).rejects.toMatchObject({ response: { status: 401 } });

    expect(adapter).toHaveBeenCalledTimes(1);
    expect(headerOf(adapter.mock.calls[0][0], 'Authorization')).toBeUndefined();
    expect(adapter.mock.calls.some(([c]) => isRefresh(c))).toBe(false);
  });

  it('auth enabled: adds the in-memory bearer token', async () => {
    setAuthMode('enabled');
    const token = tokenFor('alice');
    startSession(token, null);
    const adapter = installMockBackend([api, authApi], () => ({ status: 200, data: [] }));

    await itemService.list();

    expect(headerOf(adapter.mock.calls[0][0], 'Authorization')).toBe(`Bearer ${token}`);
  });

  it('two concurrent 401s trigger exactly one refresh, then both requests are retried with the new token', async () => {
    setAuthMode('enabled');
    const old = tokenFor('alice', 'user', -5);
    const fresh = tokenFor('alice', 'user', 900, { jti: 'fresh' });
    startSession(old, null);
    const refreshReply = deferred<MockReply>();
    const adapter = installMockBackend([api, authApi], (config) => {
      if (isRefresh(config)) return refreshReply.promise;
      const auth = headerOf(config, 'Authorization');
      return auth === `Bearer ${fresh}` ? { status: 200, data: [{ id: 1 }] } : { status: 401 };
    });

    const a = itemService.list();
    const b = itemService.get(1);
    await vi.waitFor(() => expect(adapter.mock.calls.filter(([c]) => isRefresh(c))).toHaveLength(1));
    refreshReply.resolve({ status: 200, data: { token: fresh } });

    await expect(a).resolves.toEqual({ items: [{ id: 1 }], total: 1 });
    await expect(b).resolves.toEqual([{ id: 1 }]);
    expect(adapter.mock.calls.filter(([c]) => isRefresh(c))).toHaveLength(1);
    expect(getSession().token).toBe(fresh);
  });

  it('a 401 for a request sent with an older token retries with the current token without refreshing', async () => {
    setAuthMode('enabled');
    const fresh = tokenFor('alice');
    startSession(fresh, null);
    // The retry reuses the request config object, so record the header when each request is sent.
    const sentAuth: Array<string | undefined> = [];
    const adapter = installMockBackend([api, authApi], (config) => {
      sentAuth.push(headerOf(config, 'Authorization'));
      return sentAuth.length === 1 ? { status: 401 } : { status: 200, data: [] };
    });
    // The request went out with an older token (set explicitly here).
    const older = tokenFor('alice', 'user', -5);
    await api.get('/api/v1/items', { headers: { Authorization: `Bearer ${older}` } });

    expect(adapter.mock.calls.some(([c]) => isRefresh(c))).toBe(false);
    expect(sentAuth).toEqual([`Bearer ${older}`, `Bearer ${fresh}`]);
  });

  it('failed refresh logs out: the session is cleared and the original error is returned', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('enabled');
    startSession(tokenFor('alice', 'user', -5), null);
    const adapter = installMockBackend([api, authApi], (config) =>
      isRefresh(config) ? { status: 401, data: { error: 'Invalid session' } } : { status: 401 },
    );

    await expect(itemService.list()).rejects.toMatchObject({ response: { status: 401 } });

    expect(getSession().token).toBeNull();
    expect(adapter.mock.calls.filter(([c]) => isRefresh(c))).toHaveLength(1);
  });

  it('does not retry a request twice when the retried request is also refused', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('enabled');
    startSession(tokenFor('alice', 'user', -5), null);
    const adapter = installMockBackend([api, authApi], (config) =>
      isRefresh(config) ? { status: 200, data: { token: tokenFor('alice', 'user', 900, { jti: 'n' }) } } : { status: 401 },
    );

    await expect(itemService.list()).rejects.toMatchObject({ response: { status: 401 } });
    expect(adapter.mock.calls.filter(([c]) => !isRefresh(c))).toHaveLength(2);
    expect(adapter.mock.calls.filter(([c]) => isRefresh(c))).toHaveLength(1);
  });

  it('calls the API same-origin through the /api proxy', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api, authApi], () => ({ status: 200, data: [] }));
    await itemService.list();
    expect(adapter.mock.calls[0][0].baseURL).toBe('/api');
  });
});

describe('authService', () => {
  afterEach(() => {
    resetSessionForTesting();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('login posts the credentials as JSON with X-Requested-With and credentials', async () => {
    const token = tokenFor('alice');
    const adapter = installMockBackend([api, authApi], () => ({
      status: 200,
      data: { token, user: { id: 'u1', username: 'alice', display_name: 'Alice', role: 'user' } },
    }));

    const result = await authService.login('alice', 's3cret');

    expect(result.token).toBe(token);
    const config = adapter.mock.calls[0][0];
    expect(pathOf(config)).toBe('/api/v1/auth/login');
    expect(config.method).toBe('post');
    expect(JSON.parse(config.data as string)).toEqual({ username: 'alice', password: 's3cret' });
    expect(headerOf(config, 'X-Requested-With')).toBe('XMLHttpRequest');
    expect(headerOf(config, 'Content-Type')).toMatch(/^application\/json/);
    expect(config.withCredentials).toBe(true);
  });

  it('logout posts {} with X-Requested-With and the bearer token of the session', async () => {
    const token = tokenFor('alice');
    const adapter = installMockBackend([api, authApi], () => ({ status: 200, data: { message: 'Logged out' } }));

    await authService.logout(token);

    const config = adapter.mock.calls[0][0];
    expect(pathOf(config)).toBe('/api/v1/auth/logout');
    expect(config.data).toBe('{}');
    expect(headerOf(config, 'X-Requested-With')).toBe('XMLHttpRequest');
    expect(headerOf(config, 'Content-Type')).toMatch(/^application\/json/);
    expect(headerOf(config, 'Authorization')).toBe(`Bearer ${token}`);
    expect(config.withCredentials).toBe(true);
  });

  it('logout without a token sends no Authorization header', async () => {
    const adapter = installMockBackend([api, authApi], () => ({ status: 200, data: {} }));
    await authService.logout(null);
    expect(headerOf(adapter.mock.calls[0][0], 'Authorization')).toBeUndefined();
  });
});

describe('error logging never leaks credentials', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  const PASSWORD = 'pw-Secret-Value-123';

  it('a failed login logs a summary without the password', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([api, authApi], () => ({ status: 401, data: { error: 'Invalid username or password' } }));

    await expect(authService.login('alice', PASSWORD)).rejects.toBeDefined();

    expect(errorSpy).toHaveBeenCalled();
    const logged = reachableText(errorSpy.mock.calls.flat());
    expect(logged).not.toContain(PASSWORD);
    expect(logged).toContain('401');
    expect(logged).toContain('/api/v1/auth/login');
  });

  it('a failed authenticated request logs a summary without the bearer token or the query', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('enabled');
    const token = tokenFor('alice');
    startSession(token, null);
    installMockBackend([api, authApi], () => ({ status: 500, data: { error: 'boom' } }));

    await expect(api.get('/api/v1/items?secret=q1')).rejects.toBeDefined();

    const logged = reachableText(errorSpy.mock.calls.flat());
    expect(logged).not.toContain(token);
    expect(logged).not.toContain('secret=q1');
    expect(logged).toContain('500');
    expect(logged).toContain('/api/v1/items');
  });

  it('a failed authenticated request that was retried after a refresh logs no token', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('enabled');
    const old = tokenFor('alice', 'user', -5);
    const fresh = tokenFor('alice', 'user', 900, { jti: 'fresh' });
    startSession(old, null);
    installMockBackend([api, authApi], (config) =>
      isRefresh(config) ? { status: 200, data: { token: fresh } } : { status: 401 },
    );

    await expect(itemService.list()).rejects.toBeDefined();

    const logged = reachableText(errorSpy.mock.calls.flat());
    expect(logged).not.toContain(old);
    expect(logged).not.toContain(fresh);
  });

  it('a failed logout logs a summary without the bearer token', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const token = tokenFor('alice');
    installMockBackend([api, authApi], () => ({ networkError: true }));

    await expect(authService.logout(token)).rejects.toBeDefined();

    expect(errorSpy).toHaveBeenCalled();
    const logged = reachableText(errorSpy.mock.calls.flat());
    expect(logged).not.toContain(token);
    expect(logged).toContain('ERR_NETWORK');
  });
});

describe('uiConfigService', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('reads GET /api/v1/ui-config', async () => {
    const adapter = installMockBackend([api, authApi], () => ({
      status: 200,
      data: { title: 'My App', auth_enabled: true, oidc_enabled: true, local_auth_enabled: false, password_reset_enabled: false },
    }));
    await expect(uiConfigService.get()).resolves.toEqual({
      title: 'My App',
      auth_enabled: true,
      oidc_enabled: true,
      local_auth_enabled: false,
      password_reset_enabled: false,
    });
    expect(pathOf(adapter.mock.calls[0][0])).toBe('/api/v1/ui-config');
  });

  it('rejects an answer that is not a UI config (for example the index.html of a misrouted proxy)', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([api, authApi], () => ({ status: 200, data: '<!doctype html>' }));
    await expect(uiConfigService.get()).rejects.toThrow();
  });
});

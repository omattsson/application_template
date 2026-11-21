import { describe, it, expect, afterEach, vi } from 'vitest';
import api, { authService } from '../client';
import { authApi, resetRefreshForTesting } from '../refresh';
import { getSession, resetSessionForTesting, setAuthMode, startSession } from '../session';
import { headerOf, installMockBackend, pathOf, reachableText, tokenFor } from '../../test/mockHttp';
import type { InternalAxiosRequestConfig } from 'axios';

const CURRENT = 'old-pass-phrase';
const NEW_PASSWORD = 'a-new-pass-phrase';
const WRONG = { status: 401, data: { error: 'Current password is incorrect' } };
const EXPIRED = { status: 401, data: { error: 'Invalid or expired token' } };

const isRefresh = (config: InternalAxiosRequestConfig) => pathOf(config) === '/api/v1/auth/refresh';
const isChange = (config: InternalAxiosRequestConfig) => pathOf(config) === '/api/v1/auth/me/password';

describe('authService.changePassword', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('puts both passwords to /api/v1/auth/me/password on the auth client with the CSRF and bearer headers', async () => {
    setAuthMode('enabled');
    const token = tokenFor('alice');
    startSession(token, null);
    const adapter = installMockBackend([api, authApi], () => ({ status: 200, data: { message: 'Password changed' } }));

    await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).resolves.toBeUndefined();

    expect(adapter).toHaveBeenCalledTimes(1);
    const config = adapter.mock.calls[0][0];
    expect(config.method).toBe('put');
    expect(pathOf(config)).toBe('/api/v1/auth/me/password');
    expect(config.baseURL).toBe('/api');
    expect(JSON.parse(String(config.data))).toEqual({ current_password: CURRENT, new_password: NEW_PASSWORD });
    expect(headerOf(config, 'X-Requested-With')).toBe('XMLHttpRequest');
    expect(headerOf(config, 'Authorization')).toBe(`Bearer ${token}`);
    expect(config.withCredentials).toBe(true);
  });

  it('goes through the auth client, not the api instance (no 401 interceptor)', async () => {
    setAuthMode('enabled');
    startSession(tokenFor('alice'), null);
    const apiOnly = installMockBackend([api], () => ({ status: 500 }));
    const authOnly = installMockBackend([authApi], () => ({ status: 200, data: {} }));

    await authService.changePassword(CURRENT, NEW_PASSWORD);

    expect(authOnly).toHaveBeenCalledTimes(1);
    expect(apiOnly).not.toHaveBeenCalled();
  });

  it('accepts 204 as success', async () => {
    setAuthMode('enabled');
    startSession(tokenFor('alice'), null);
    installMockBackend([api, authApi], () => ({ status: 204 }));
    await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).resolves.toBeUndefined();
  });

  it('a wrong current password (401) is one request: no refresh, no retry', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('enabled');
    startSession(tokenFor('alice'), null);
    const adapter = installMockBackend([api, authApi], () => WRONG);

    await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).rejects.toMatchObject({
      response: { status: 401, data: { error: 'Current password is incorrect' } },
    });

    expect(adapter).toHaveBeenCalledTimes(1);
    expect(adapter.mock.calls.some(([c]) => isRefresh(c))).toBe(false);
  });

  it.each([400, 403, 429, 500])('a %i answer is one request and is rethrown', async (status) => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('enabled');
    startSession(tokenFor('alice'), null);
    const adapter = installMockBackend([api, authApi], () => ({ status, data: { error: 'x' } }));

    await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).rejects.toMatchObject({ response: { status } });

    expect(adapter).toHaveBeenCalledTimes(1);
  });

  describe('an expired access token (401 that is not the wrong password)', () => {
    it('refreshes once and resubmits once with the new token', async () => {
      setAuthMode('enabled');
      const old = tokenFor('alice', 'user', -5);
      const fresh = tokenFor('alice', 'user', 900, { jti: 'fresh' });
      startSession(old, null);
      const sent: (string | undefined)[] = [];
      const adapter = installMockBackend([api, authApi], (config) => {
        if (isRefresh(config)) return { status: 200, data: { token: fresh } };
        sent.push(headerOf(config, 'Authorization'));
        return sent.length === 1 ? EXPIRED : { status: 200, data: { message: 'Password changed' } };
      });

      await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).resolves.toBeUndefined();

      expect(adapter.mock.calls.filter(([c]) => isRefresh(c))).toHaveLength(1);
      expect(adapter.mock.calls.filter(([c]) => isChange(c))).toHaveLength(2);
      expect(sent).toEqual([`Bearer ${old}`, `Bearer ${fresh}`]);
      const second = adapter.mock.calls.filter(([c]) => isChange(c))[1][0];
      expect(JSON.parse(String(second.data))).toEqual({ current_password: CURRENT, new_password: NEW_PASSWORD });
      expect(headerOf(second, 'X-Requested-With')).toBe('XMLHttpRequest');
    });

    it('resubmits at most once: a second refusal is thrown', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {});
      setAuthMode('enabled');
      startSession(tokenFor('alice', 'user', -5), null);
      const adapter = installMockBackend([api, authApi], (config) =>
        isRefresh(config) ? { status: 200, data: { token: tokenFor('alice', 'user', 900, { jti: 'n' }) } } : EXPIRED,
      );

      await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).rejects.toMatchObject({ response: { status: 401 } });

      expect(adapter.mock.calls.filter(([c]) => isChange(c))).toHaveLength(2);
      expect(adapter.mock.calls.filter(([c]) => isRefresh(c))).toHaveLength(1);
    });

    it('a wrong current password after the resubmit is thrown as that error', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {});
      setAuthMode('enabled');
      startSession(tokenFor('alice', 'user', -5), null);
      let changes = 0;
      installMockBackend([api, authApi], (config) => {
        if (isRefresh(config)) return { status: 200, data: { token: tokenFor('alice', 'user', 900, { jti: 'n' }) } };
        changes += 1;
        return changes === 1 ? EXPIRED : WRONG;
      });

      await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).rejects.toMatchObject({
        response: { status: 401, data: { error: 'Current password is incorrect' } },
      });
      expect(changes).toBe(2);
    });

    it('a refused refresh ends the session and throws the original 401 without a resubmit', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {});
      setAuthMode('enabled');
      startSession(tokenFor('alice', 'user', -5), null);
      const adapter = installMockBackend([api, authApi], (config) =>
        isRefresh(config) ? { status: 401, data: { error: 'Invalid session' } } : EXPIRED,
      );

      await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).rejects.toMatchObject({
        response: { status: 401, data: { error: 'Invalid or expired token' } },
      });

      expect(adapter.mock.calls.filter(([c]) => isChange(c))).toHaveLength(1);
      expect(adapter.mock.calls.filter(([c]) => isRefresh(c))).toHaveLength(1);
      expect(getSession().token).toBeNull();
    });

    it('a refresh that fails on the network throws the original 401 without a resubmit', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {});
      setAuthMode('enabled');
      startSession(tokenFor('alice', 'user', -5), null);
      const adapter = installMockBackend([api, authApi], (config) =>
        isRefresh(config) ? { networkError: true } : EXPIRED,
      );

      await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).rejects.toMatchObject({ response: { status: 401 } });

      expect(adapter.mock.calls.filter(([c]) => isChange(c))).toHaveLength(1);
    });

    it('uses a newer token that another tab shared, without a refresh call, and resubmits once', async () => {
      setAuthMode('enabled');
      const old = tokenFor('alice', 'user', 900, { jti: 'old' });
      const newer = tokenFor('alice', 'user', 900, { jti: 'newer' });
      startSession(old, null);
      const sent: (string | undefined)[] = [];
      const adapter = installMockBackend([api, authApi], (config) => {
        sent.push(headerOf(config, 'Authorization'));
        if (sent.length === 1) {
          // Another tab refreshed meanwhile.
          startSession(newer, null);
          return EXPIRED;
        }
        return { status: 200, data: {} };
      });

      await authService.changePassword(CURRENT, NEW_PASSWORD);

      expect(adapter.mock.calls.some(([c]) => isRefresh(c))).toBe(false);
      expect(sent).toEqual([`Bearer ${old}`, `Bearer ${newer}`]);
    });
  });

  it('logs only a summary: no password and no bearer token', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('enabled');
    const token = tokenFor('alice');
    startSession(token, null);
    installMockBackend([api, authApi], () => WRONG);

    await expect(authService.changePassword(CURRENT, NEW_PASSWORD)).rejects.toBeDefined();

    expect(errorSpy).toHaveBeenCalled();
    const logged = reachableText(errorSpy.mock.calls.flat());
    expect(logged).not.toContain(CURRENT);
    expect(logged).not.toContain(NEW_PASSWORD);
    expect(logged).not.toContain(token);
    expect(logged).toContain('401');
    expect(logged).toContain('/api/v1/auth/me/password');
  });
});

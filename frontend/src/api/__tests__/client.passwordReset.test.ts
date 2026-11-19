import { describe, it, expect, afterEach, vi } from 'vitest';
import api, { passwordResetService, unauthenticatedApi, uiConfigService } from '../client';
import { authApi, resetRefreshForTesting } from '../refresh';
import { getSession, setAuthMode, startSession, resetSessionForTesting } from '../session';
import { headerOf, installMockBackend, pathOf, reachableText, tokenFor } from '../../test/mockHttp';
import type { InternalAxiosRequestConfig } from 'axios';

const TOKEN = 'Q'.repeat(43);
const PASSWORD = 'correct horse battery';
const USERNAME = 'alice';

const isRefresh = (config: InternalAxiosRequestConfig) => pathOf(config) === '/api/v1/auth/refresh';

describe('password reset service', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('requestPasswordReset posts the username and resolves on 202', async () => {
    const adapter = installMockBackend([unauthenticatedApi], () => ({
      status: 202,
      data: { message: 'If the account exists and can receive mail, a reset link has been sent.' },
    }));
    await expect(passwordResetService.requestPasswordReset(USERNAME)).resolves.toBeUndefined();
    const [config] = adapter.mock.calls[0];
    expect(pathOf(config)).toBe('/api/v1/auth/password-reset');
    expect(config.method).toBe('post');
    expect(JSON.parse(String(config.data))).toEqual({ username: USERNAME });
  });

  it('requestPasswordReset sends no bearer token and no cookie, even with a session', async () => {
    setAuthMode('enabled');
    startSession(tokenFor('alice'), null);
    const adapter = installMockBackend([unauthenticatedApi], () => ({ status: 202, data: {} }));
    await passwordResetService.requestPasswordReset(USERNAME);
    const [config] = adapter.mock.calls[0];
    expect(headerOf(config, 'Authorization')).toBeUndefined();
    expect(config.withCredentials).toBeFalsy();
  });

  it('requestPasswordReset rejects on a 400 and never tries a refresh', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('enabled');
    startSession(tokenFor('alice'), null);
    const adapter = installMockBackend([unauthenticatedApi], () => ({ status: 400, data: { error: 'Username is required' } }));
    await expect(passwordResetService.requestPasswordReset('   ')).rejects.toMatchObject({ response: { status: 400 } });
    expect(adapter).toHaveBeenCalledTimes(1);
    expect(adapter.mock.calls.some(([c]) => isRefresh(c))).toBe(false);
  });

  it('confirmPasswordReset posts the token and the password and resolves on 204', async () => {
    const adapter = installMockBackend([unauthenticatedApi], () => ({ status: 204 }));
    await expect(passwordResetService.confirmPasswordReset(TOKEN, PASSWORD)).resolves.toBeUndefined();
    const [config] = adapter.mock.calls[0];
    expect(pathOf(config)).toBe('/api/v1/auth/password-reset/confirm');
    expect(JSON.parse(String(config.data))).toEqual({ token: TOKEN, new_password: PASSWORD });
  });

  it('confirmPasswordReset on a 401 does not start a refresh (no 401 interceptor)', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setAuthMode('enabled');
    startSession(tokenFor('alice'), null);
    const adapter = installMockBackend([unauthenticatedApi], () => ({ status: 401, data: { error: 'nope' } }));
    await expect(passwordResetService.confirmPasswordReset(TOKEN, PASSWORD)).rejects.toMatchObject({
      response: { status: 401 },
    });
    expect(adapter).toHaveBeenCalledTimes(1);
    expect(adapter.mock.calls.some(([c]) => isRefresh(c))).toBe(false);
    expect(getSession().token).not.toBeNull();
  });

  it('confirmPasswordReset on a 400 rejects with the status and body for the page to read', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([unauthenticatedApi], () => ({ status: 400, data: { error: 'Invalid or expired token' } }));
    await expect(passwordResetService.confirmPasswordReset(TOKEN, PASSWORD)).rejects.toMatchObject({
      response: { status: 400, data: { error: 'Invalid or expired token' } },
    });
  });

  it('never logs the username, the password or the token', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([unauthenticatedApi], () => ({ status: 500, data: { error: 'Internal server error' } }));
    await expect(passwordResetService.requestPasswordReset(USERNAME)).rejects.toBeDefined();
    await expect(passwordResetService.confirmPasswordReset(TOKEN, PASSWORD)).rejects.toBeDefined();
    const text = reachableText(spy.mock.calls.flat());
    expect(text).not.toContain(USERNAME);
    expect(text).not.toContain(PASSWORD);
    expect(text).not.toContain(TOKEN);
    expect(text).toContain('/api/v1/auth/password-reset/confirm');
  });
});

describe('uiConfigService with password reset', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('passes password_reset_enabled through', async () => {
    installMockBackend([api, authApi], () => ({
      status: 200,
      data: { title: 'App', auth_enabled: true, oidc_enabled: false, local_auth_enabled: true, password_reset_enabled: true },
    }));
    await expect(uiConfigService.get()).resolves.toMatchObject({ password_reset_enabled: true });
  });

  it('treats a missing flag as off', async () => {
    installMockBackend([api, authApi], () => ({
      status: 200,
      data: { title: 'App', auth_enabled: true, oidc_enabled: false, local_auth_enabled: true },
    }));
    await expect(uiConfigService.get()).resolves.toMatchObject({ password_reset_enabled: false });
  });
});

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, waitFor, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useAuth, refreshDelayMs } from '../AuthContext';
import { getSession, resetSessionForTesting, SESSION_CHANNEL_NAME } from '../../api/session';
import { resetRefreshForTesting } from '../../api/refresh';
import { AuthProviders, fakeBackend, uiConfigOf } from '../../test/authHarness';
import { headerOf, makeJwt, pathOf, tokenFor } from '../../test/mockHttp';

function Probe() {
  const { authEnabled, isLoading, isAuthenticated, user, role, login, logout } = useAuth();
  if (isLoading) return <p>auth loading</p>;
  return (
    <div>
      <p>enabled:{String(authEnabled)}</p>
      <p>authenticated:{String(isAuthenticated)}</p>
      <p>user:{user?.display_name ?? 'none'}</p>
      <p>role:{role ?? 'none'}</p>
      <button type="button" onClick={() => login('alice', 'pw').catch(() => {})}>
        do login
      </button>
      <button type="button" onClick={() => void logout()}>
        do logout
      </button>
    </div>
  );
}

const renderProbe = () =>
  render(
    <AuthProviders>
      <Probe />
    </AuthProviders>,
  );

const calls = (adapter: ReturnType<typeof fakeBackend>, path: string) =>
  adapter.mock.calls.filter(([config]) => pathOf(config) === path);

describe('AuthProvider', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    localStorage.clear();
    sessionStorage.clear();
    vi.useRealTimers();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('auth disabled: no session restore, no refresh call, not authenticated', async () => {
    const adapter = fakeBackend({ uiConfig: uiConfigOf({ auth_enabled: false }) });
    renderProbe();
    expect(screen.getByText('auth loading')).toBeInTheDocument();
    expect(await screen.findByText('enabled:false')).toBeInTheDocument();
    expect(screen.getByText('authenticated:false')).toBeInTheDocument();
    expect(calls(adapter, '/api/v1/auth/refresh')).toHaveLength(0);
    expect(getSession().mode).toBe('disabled');
  });

  it('auth enabled without a session cookie: anonymous after one refresh attempt', async () => {
    const adapter = fakeBackend({ uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }) });
    renderProbe();
    expect(await screen.findByText('authenticated:false')).toBeInTheDocument();
    expect(screen.getByText('enabled:true')).toBeInTheDocument();
    expect(calls(adapter, '/api/v1/auth/refresh')).toHaveLength(1);
  });

  it('auth enabled with a session cookie: restores the user from the refreshed token', async () => {
    fakeBackend({
      uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
      refresh: { status: 200, data: { token: tokenFor('alice', 'admin') } },
    });
    renderProbe();
    expect(await screen.findByText('authenticated:true')).toBeInTheDocument();
    expect(screen.getByText('user:Alice Person')).toBeInTheDocument();
    expect(screen.getByText('role:admin')).toBeInTheDocument();
  });

  it('login keeps the token in memory only and shows the user (localStorage and sessionStorage untouched)', async () => {
    const token = tokenFor('alice', 'devops');
    fakeBackend({
      uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
      login: {
        status: 200,
        data: { token, user: { id: 'u1', username: 'alice', display_name: 'Alice From Server', role: 'devops' } },
      },
    });
    renderProbe();
    await screen.findByText('authenticated:false');

    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    await userEvent.setup().click(screen.getByRole('button', { name: 'do login' }));

    expect(await screen.findByText('authenticated:true')).toBeInTheDocument();
    expect(screen.getByText('user:Alice From Server')).toBeInTheDocument();
    expect(screen.getByText('role:devops')).toBeInTheDocument();
    expect(getSession().token).toBe(token);
    expect(setItem).not.toHaveBeenCalled();
    const stored = [...Object.values({ ...localStorage }), ...Object.values({ ...sessionStorage })].join('\n');
    expect(stored).not.toContain(token);
    expect(localStorage.length + sessionStorage.length).toBe(0);
  });

  it('a failed login rejects and leaves the user anonymous', async () => {
    fakeBackend({ uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }) });
    renderProbe();
    await screen.findByText('authenticated:false');
    await userEvent.setup().click(screen.getByRole('button', { name: 'do login' }));
    await waitFor(() => expect(screen.getByText('authenticated:false')).toBeInTheDocument());
    expect(getSession().token).toBeNull();
  });

  it('logout calls the logout endpoint with the bearer token, clears memory and tells other tabs', async () => {
    const token = tokenFor('alice');
    const adapter = fakeBackend({
      uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
      refresh: { status: 200, data: { token } },
    });
    const otherTab = new BroadcastChannel(SESSION_CHANNEL_NAME);
    const messages: unknown[] = [];
    otherTab.onmessage = (event: MessageEvent) => messages.push(event.data);
    renderProbe();
    await screen.findByText('authenticated:true');

    await userEvent.setup().click(screen.getByRole('button', { name: 'do logout' }));

    expect(await screen.findByText('authenticated:false')).toBeInTheDocument();
    expect(getSession().token).toBeNull();
    const logout = calls(adapter, '/api/v1/auth/logout');
    expect(logout).toHaveLength(1);
    expect(headerOf(logout[0][0], 'Authorization')).toBe(`Bearer ${token}`);
    await waitFor(() => expect(messages).toContainEqual({ type: 'logout' }));
    otherTab.close();
  });

  it('logout still clears the local session when the server call fails', async () => {
    fakeBackend({
      uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
      refresh: { status: 200, data: { token: tokenFor('alice') } },
      logout: { status: 500 },
    });
    renderProbe();
    await screen.findByText('authenticated:true');
    await userEvent.setup().click(screen.getByRole('button', { name: 'do logout' }));
    expect(await screen.findByText('authenticated:false')).toBeInTheDocument();
  });

  it('a session ended by a failed refresh elsewhere (for example the 401 interceptor) logs this provider out', async () => {
    fakeBackend({
      uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
      refresh: { status: 200, data: { token: tokenFor('alice') } },
    });
    renderProbe();
    await screen.findByText('authenticated:true');
    const otherTab = new BroadcastChannel(SESSION_CHANNEL_NAME);
    act(() => otherTab.postMessage({ type: 'logout' }));
    expect(await screen.findByText('authenticated:false')).toBeInTheDocument();
    otherTab.close();
  });

  it('refreshes the access token before it expires', async () => {
    let n = 0;
    const adapter = fakeBackend({
      uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
      refresh: () => {
        n += 1;
        // The first token lives one second, the next one 15 minutes.
        return { status: 200, data: { token: tokenFor('alice', 'user', n === 1 ? 1 : 900, { jti: `t${n}` }) } };
      },
    });
    renderProbe();
    await screen.findByText('authenticated:true');
    await waitFor(() => expect(calls(adapter, '/api/v1/auth/refresh')).toHaveLength(2), { timeout: 3000 });
  });
});

describe('refreshDelayMs', () => {
  const now = 1_700_000_000_000;
  const at = (seconds: number) => Math.floor(now / 1000) + seconds;

  it('refreshes one minute before expiry for a long-lived token', () => {
    expect(refreshDelayMs(makeJwt({ iat: at(0), exp: at(900) }), now)).toBe(840_000);
  });

  it('refreshes at half the lifetime for a short-lived token', () => {
    expect(refreshDelayMs(makeJwt({ iat: at(0), exp: at(60) }), now)).toBe(30_000);
  });

  it('uses the lifetime of the token (iat to exp), so a wrong client clock cannot cause a refresh loop', () => {
    // Issued and expired in the past by the client clock: still refreshed at half its 90 s lifetime.
    expect(refreshDelayMs(makeJwt({ iat: at(-100), exp: at(-10) }), now)).toBe(45_000);
  });

  it('waits at least one second', () => {
    expect(refreshDelayMs(makeJwt({ iat: at(0), exp: at(1) }), now)).toBe(1_000);
    expect(refreshDelayMs(makeJwt({ exp: at(-10) }), now)).toBe(1_000);
  });

  it('uses exp and the clock when iat is missing', () => {
    expect(refreshDelayMs(makeJwt({ exp: at(600) }), now)).toBe(540_000);
  });

  it('never schedules for a token without exp or a malformed token', () => {
    expect(refreshDelayMs(makeJwt({ iat: at(0) }), now)).toBeNull();
    expect(refreshDelayMs('garbage', now)).toBeNull();
  });
});

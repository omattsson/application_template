import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import App from '../App';
import { getSession, resetSessionForTesting } from '../api/session';
import { resetRefreshForTesting } from '../api/refresh';
import { fakeBackend, uiConfigOf } from '../test/authHarness';
import { headerOf, pathOf, tokenFor } from '../test/mockHttp';

// The socket is covered by its own tests; here it must not open a real connection.
vi.mock('reconnecting-websocket', () => ({
  default: class MockRWS extends EventTarget {
    close() {}
    send() {}
  },
}));

const ITEMS = [{ id: 1, name: 'Widget', price: 9.5, created_at: '', updated_at: '' }];

function at(path: string) {
  window.history.pushState({}, '', path);
}

const pageCalls = (adapter: ReturnType<typeof fakeBackend>) =>
  adapter.mock.calls.filter(([c]) => pathOf(c).split('?')[0] === '/api/v1/items');
const authCalls = (adapter: ReturnType<typeof fakeBackend>) =>
  adapter.mock.calls.filter(([c]) => pathOf(c).startsWith('/api/v1/auth/'));

describe('App with optional auth', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    localStorage.clear();
    sessionStorage.clear();
    at('/');
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('auth disabled: protected pages render with no login and requests carry no Authorization', async () => {
    const adapter = fakeBackend({ uiConfig: uiConfigOf({ auth_enabled: false }), other: () => ({ status: 200, data: ITEMS }) });
    at('/items');
    render(<App />);

    expect(await screen.findByText('Widget')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Sign in' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /log out/i })).not.toBeInTheDocument();
    expect(window.location.pathname).toBe('/items');
    for (const [config] of adapter.mock.calls) {
      expect(headerOf(config, 'Authorization')).toBeUndefined();
    }
    expect(authCalls(adapter)).toHaveLength(0);
  });

  it('auth enabled: an anonymous visitor is sent to /login with the intended path', async () => {
    const adapter = fakeBackend({ uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }) });
    at('/items');
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(window.location.pathname).toBe('/login');
    expect(new URLSearchParams(window.location.search).get('next')).toBe('/items');
    expect(pageCalls(adapter)).toHaveLength(0);
  });

  it('auth enabled: login shows the user, returns to the page and sends the bearer token', async () => {
    const token = tokenFor('alice', 'user');
    const adapter = fakeBackend({
      uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
      login: {
        status: 200,
        data: { token, user: { id: 'u1', username: 'alice', display_name: 'Alice Person', role: 'user' } },
      },
      other: (config) =>
        headerOf(config, 'Authorization') === `Bearer ${token}` ? { status: 200, data: ITEMS } : { status: 401 },
    });
    at('/items');
    render(<App />);
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/username/i), 'alice');
    await user.type(screen.getByLabelText(/password/i), 'pw');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));

    expect(await screen.findByText('Widget')).toBeInTheDocument();
    expect(window.location.pathname).toBe('/items');
    expect(screen.getByText('Alice Person')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /log out/i })).toBeInTheDocument();
    expect(headerOf(pageCalls(adapter)[0][0], 'Authorization')).toBe(`Bearer ${token}`);
  });

  it('auth enabled: a failed refresh after a 401 logs out and shows the login page', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    let refreshes = 0;
    const adapter = fakeBackend({
      uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
      // The page load restores the session; the second refresh (after the 401) is refused.
      refresh: () => {
        refreshes += 1;
        return refreshes === 1
          ? { status: 200, data: { token: tokenFor('alice') } }
          : { status: 401, data: { error: 'Invalid session' } };
      },
      other: () => ({ status: 401 }),
    });
    at('/items');
    render(<App />);

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    await waitFor(() => expect(window.location.pathname).toBe('/login'));
    expect(getSession().token).toBeNull();
    expect(adapter.mock.calls.filter(([c]) => pathOf(c) === '/api/v1/auth/refresh')).toHaveLength(2);
  });

  it('auth enabled: Logout ends the session and shows the login page', async () => {
    const adapter = fakeBackend({
      uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
      refresh: { status: 200, data: { token: tokenFor('alice') } },
      other: () => ({ status: 200, data: ITEMS }),
    });
    at('/items');
    render(<App />);
    await screen.findByText('Widget');

    await userEvent.setup().click(screen.getByRole('button', { name: /log out/i }));

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(window.location.pathname).toBe('/login');
    expect(getSession().token).toBeNull();
    const logout = adapter.mock.calls.filter(([c]) => pathOf(c) === '/api/v1/auth/logout');
    expect(logout).toHaveLength(1);
    expect(headerOf(logout[0][0], 'X-Requested-With')).toBe('XMLHttpRequest');
  });
});

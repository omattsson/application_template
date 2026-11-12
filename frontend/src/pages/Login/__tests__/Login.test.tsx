import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import Login from '../index';
import { redirectBrowser } from '../../../utils/navigation';
import { getSession, resetSessionForTesting } from '../../../api/session';
import { resetRefreshForTesting } from '../../../api/refresh';
import { AuthProviders, fakeBackend, uiConfigOf } from '../../../test/authHarness';
import { pathOf, tokenFor } from '../../../test/mockHttp';
import type { MockReply } from '../../../test/mockHttp';
import type { UIConfig } from '../../../context/UIConfigContext';

vi.mock('../../../utils/navigation', () => ({ redirectBrowser: vi.fn() }));

function Where() {
  const location = useLocation();
  return <p>at {location.pathname + location.search}</p>;
}

function renderLogin(config: Partial<UIConfig>, opts: { path?: string; login?: MockReply } = {}) {
  const adapter = fakeBackend({ uiConfig: uiConfigOf({ title: 'Test App', ...config }), login: opts.login });
  const view = render(
    <AuthProviders>
      <MemoryRouter initialEntries={[opts.path ?? '/login']}>
        <Routes>
          <Route path="/login" element={<Login />} />
          <Route path="*" element={<Where />} />
        </Routes>
      </MemoryRouter>
    </AuthProviders>,
  );
  return { adapter, ...view };
}

const LOCAL = { auth_enabled: true, local_auth_enabled: true, oidc_enabled: false };
const okLogin = (role = 'user'): MockReply => ({
  status: 200,
  data: { token: tokenFor('alice', role), user: { id: 'u1', username: 'alice', display_name: 'Alice', role } },
});

async function submit(username = 'alice', password = 'pw') {
  const user = userEvent.setup();
  await user.type(await screen.findByLabelText(/username/i), username);
  await user.type(screen.getByLabelText(/password/i), password);
  await user.click(screen.getByRole('button', { name: 'Sign in' }));
}

describe('Login page', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    sessionStorage.clear();
    localStorage.clear();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('shows a loading state while the config loads, then the title and the local form only', async () => {
    renderLogin(LOCAL);
    expect(screen.getByRole('status')).toBeInTheDocument();
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.getByText('Test App')).toBeInTheDocument();
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/password/i)).toHaveAttribute('type', 'password');
    expect(screen.queryByRole('button', { name: 'Sign in with SSO' })).not.toBeInTheDocument();
  });

  it('shows only the SSO button when local auth is off', async () => {
    renderLogin({ auth_enabled: true, oidc_enabled: true, local_auth_enabled: false });
    expect(await screen.findByRole('button', { name: 'Sign in with SSO' })).toBeInTheDocument();
    expect(screen.queryByLabelText(/username/i)).not.toBeInTheDocument();
  });

  it('shows both when OIDC and local auth are on', async () => {
    renderLogin({ auth_enabled: true, oidc_enabled: true, local_auth_enabled: true });
    expect(await screen.findByRole('button', { name: 'Sign in with SSO' })).toBeInTheDocument();
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument();
  });

  it('redirects to home when auth is disabled', async () => {
    renderLogin({ auth_enabled: false });
    expect(await screen.findByText('at /')).toBeInTheDocument();
  });

  it('signs in and goes to the intended page from next', async () => {
    renderLogin(LOCAL, { path: `/login?next=${encodeURIComponent('/items?limit=5')}`, login: okLogin() });
    await submit();
    expect(await screen.findByText('at /items?limit=5')).toBeInTheDocument();
    expect(getSession().user?.username).toBe('alice');
  });

  it.each([
    ['protocol-relative', '//evil.example.com/x'],
    ['absolute', 'https://evil.example.com/'],
    ['backslash', '/\\evil.example.com'],
    ['javascript', 'javascript:alert(1)'],
  ])('ignores an unsafe next (%s) and goes home: no open redirect', async (_name, next) => {
    renderLogin(LOCAL, { path: `/login?next=${encodeURIComponent(next)}`, login: okLogin() });
    await submit();
    expect(await screen.findByText('at /')).toBeInTheDocument();
  });

  it('keeps the token out of localStorage and sessionStorage', async () => {
    renderLogin(LOCAL, { login: okLogin() });
    await screen.findByLabelText(/username/i);
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    await submit();
    await screen.findByText('at /');
    expect(setItem).not.toHaveBeenCalled();
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
  });

  it('shows one generic message for wrong credentials (does not reveal whether the user exists)', async () => {
    renderLogin(LOCAL, { login: { status: 401, data: { error: 'Invalid username or password' } } });
    await submit('nobody', 'bad');
    expect(await screen.findByRole('alert')).toHaveTextContent('Invalid username or password');
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument();
  });

  it('maps 429 to a too-many-attempts message', async () => {
    renderLogin(LOCAL, {
      login: { status: 429, data: { error: 'Too many login attempts' }, headers: { 'retry-after': '900' } },
    });
    await submit();
    expect(await screen.findByRole('alert')).toHaveTextContent(/too many sign-in attempts/i);
    expect(screen.getByRole('alert')).toHaveTextContent(/15 minutes/);
  });

  it('shows a server error message for 5xx and a connection message for a network error', async () => {
    const { unmount } = renderLogin(LOCAL, { login: { status: 500 } });
    await submit();
    expect(await screen.findByRole('alert')).toHaveTextContent(/server error/i);
    unmount();
    resetSessionForTesting();

    renderLogin(LOCAL, { login: { networkError: true } });
    await submit();
    expect(await screen.findByRole('alert')).toHaveTextContent(/cannot reach the server/i);
  });

  it('disables the button while signing in', async () => {
    let release!: (r: MockReply) => void;
    const adapter = fakeBackend({
      uiConfig: uiConfigOf(LOCAL),
      login: () => new Promise<MockReply>((r) => (release = r)),
    });
    render(
      <AuthProviders>
        <MemoryRouter initialEntries={['/login']}>
          <Routes>
            <Route path="/login" element={<Login />} />
            <Route path="*" element={<Where />} />
          </Routes>
        </MemoryRouter>
      </AuthProviders>,
    );
    await submit();
    expect(screen.getByRole('button', { name: /signing in/i })).toBeDisabled();
    release(okLogin());
    expect(await screen.findByText('at /')).toBeInTheDocument();
    expect(adapter.mock.calls.filter(([c]) => pathOf(c) === '/api/v1/auth/login')).toHaveLength(1);
  });

  it('SSO keeps the safe intended path for the callback and goes to the OIDC login endpoint', async () => {
    renderLogin(
      { auth_enabled: true, oidc_enabled: true, local_auth_enabled: false },
      { path: `/login?next=${encodeURIComponent('/items')}` },
    );
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Sign in with SSO' }));
    expect(redirectBrowser).toHaveBeenCalledWith('/api/api/v1/auth/oidc/login');
    expect(sessionStorage.getItem('auth-return-path')).toBe('/items');
  });

  it('SSO with an unsafe next stores home as the return path', async () => {
    renderLogin(
      { auth_enabled: true, oidc_enabled: true, local_auth_enabled: false },
      { path: `/login?next=${encodeURIComponent('//evil.example.com')}` },
    );
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Sign in with SSO' }));
    await waitFor(() => expect(redirectBrowser).toHaveBeenCalled());
    expect(sessionStorage.getItem('auth-return-path')).toBe('/');
  });
});

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import AuthCallback from '../index';
import { getSession, resetSessionForTesting } from '../../../api/session';
import { resetRefreshForTesting } from '../../../api/refresh';
import { AuthProviders, fakeBackend, uiConfigOf } from '../../../test/authHarness';
import { deferred, headerOf, pathOf, tokenFor } from '../../../test/mockHttp';
import type { MockReply } from '../../../test/mockHttp';
import type { FakeBackendRoutes } from '../../../test/authHarness';

function Where() {
  const location = useLocation();
  return <p>at {location.pathname + location.search}</p>;
}

const OIDC = uiConfigOf({ auth_enabled: true, oidc_enabled: true, local_auth_enabled: false });

function renderCallback(routes: Omit<FakeBackendRoutes, 'uiConfig'> & { uiConfig?: FakeBackendRoutes['uiConfig'] }, path = '/auth/callback') {
  const adapter = fakeBackend({ uiConfig: OIDC, ...routes });
  const view = render(
    <AuthProviders>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/auth/callback" element={<AuthCallback />} />
          <Route path="*" element={<Where />} />
        </Routes>
      </MemoryRouter>
    </AuthProviders>,
  );
  return { adapter, ...view };
}

const refreshCalls = (adapter: ReturnType<typeof fakeBackend>) =>
  adapter.mock.calls.filter(([c]) => pathOf(c) === '/api/v1/auth/refresh');

describe('AuthCallback', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    sessionStorage.clear();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('obtains the access token with exactly one POST /auth/refresh and goes to the stored intended page', async () => {
    sessionStorage.setItem('auth-return-path', '/items?limit=5');
    const token = tokenFor('alice');
    const { adapter } = renderCallback({ refresh: { status: 200, data: { token } } });

    expect(screen.getByRole('status')).toBeInTheDocument();
    expect(await screen.findByText('at /items?limit=5')).toBeInTheDocument();
    expect(refreshCalls(adapter)).toHaveLength(1);
    expect(headerOf(refreshCalls(adapter)[0][0], 'X-Requested-With')).toBe('XMLHttpRequest');
    expect(getSession().token).toBe(token);
    expect(sessionStorage.getItem('auth-return-path')).toBeNull();
  });

  it('goes home when no return path was stored', async () => {
    renderCallback({ refresh: { status: 200, data: { token: tokenFor('alice') } } });
    expect(await screen.findByText('at /')).toBeInTheDocument();
  });

  it('rejects a tampered return path (open redirect) and goes home', async () => {
    sessionStorage.setItem('auth-return-path', '//evil.example.com');
    renderCallback({ refresh: { status: 200, data: { token: tokenFor('alice') } } });
    expect(await screen.findByText('at /')).toBeInTheDocument();
  });

  it('shows a clear error with a link back to the login page when the refresh fails', async () => {
    renderCallback({ refresh: { status: 401, data: { error: 'Invalid session' } } });
    expect(await screen.findByRole('alert')).toHaveTextContent(/sign-in could not be completed/i);
    expect(screen.getByRole('link', { name: 'Back to sign in' })).toHaveAttribute('href', '/login');
    expect(getSession().token).toBeNull();
  });

  it('shows the error for an error parameter in the URL without calling the token endpoint twice', async () => {
    const { adapter } = renderCallback({}, '/auth/callback?error=access_denied');
    expect(await screen.findByRole('alert')).toHaveTextContent(/sign-in could not be completed/i);
    expect(screen.getByRole('link', { name: 'Back to sign in' })).toBeInTheDocument();
    expect(refreshCalls(adapter).length).toBeLessThanOrEqual(1);
  });

  it('goes home when auth is disabled', async () => {
    const { adapter } = renderCallback({ uiConfig: uiConfigOf({ auth_enabled: false }) });
    expect(await screen.findByText('at /')).toBeInTheDocument();
    expect(refreshCalls(adapter)).toHaveLength(0);
  });

  it('does not abort the refresh when the page unmounts: the token still lands in memory', async () => {
    const reply = deferred<MockReply>();
    const token = tokenFor('alice');
    const { adapter, unmount } = renderCallback({ refresh: () => reply.promise });
    await vi.waitFor(() => expect(refreshCalls(adapter)).toHaveLength(1));
    const config = refreshCalls(adapter)[0][0];
    expect(config.signal).toBeUndefined();

    unmount();
    reply.resolve({ status: 200, data: { token } });

    await vi.waitFor(() => expect(getSession().token).toBe(token));
  });
});

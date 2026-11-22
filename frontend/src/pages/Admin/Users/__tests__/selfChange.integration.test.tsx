import { StrictMode } from 'react';
import { describe, it, expect, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useLocation } from 'react-router-dom';
import AppRoutes from '../../../../routes';
import { resetRefreshForTesting } from '../../../../api/refresh';
import { resetSessionForTesting } from '../../../../api/session';
import { AuthProviders, fakeBackend, uiConfigOf } from '../../../../test/authHarness';
import { tokenFor } from '../../../../test/mockHttp';
import { NotificationProvider } from '../../../../context/NotificationContext';
import type { MockReply } from '../../../../test/mockHttp';

/**
 * The self-change flow with nothing mocked: the real session module, auth
 * providers, route guard and login page. The unit tests of the page mock the
 * session and cannot see the route guard react to the ended session.
 */

const ALICE_ID = 'id-alice';
const NOTICE = /your own account was changed/i;

const alice = {
  id: ALICE_ID,
  username: 'alice',
  email: '',
  display_name: 'Alice Person',
  role: 'admin',
  auth_provider: 'local',
  disabled: false,
  service_account: false,
  created_at: '2026-01-01T10:00:00Z',
  updated_at: '2026-01-01T10:00:00Z',
};

function Where() {
  const location = useLocation();
  return <p data-testid="where">{location.pathname + location.search}</p>;
}

/** A backend whose sessions end as soon as `ended.value` is set (what the real one does after a self change). */
function setup(ended: { value: boolean }, overrides: { role?: MockReply } = {}) {
  return fakeBackend({
    uiConfig: uiConfigOf({ auth_enabled: true, local_auth_enabled: true }),
    refresh: () =>
      ended.value
        ? { status: 401, data: { error: 'Invalid session' } }
        : { status: 200, data: { token: tokenFor('alice', 'admin') } },
    other: (config) => {
      if (ended.value) return { status: 401, data: { error: 'Unauthorized' } };
      if (config.method === 'put' && config.url?.endsWith('/role')) {
        ended.value = true;
        return (
          overrides.role ?? {
            status: 200,
            data: { id: ALICE_ID, old_role: 'admin', new_role: 'devops', message: 'Role changed', changed: true, idp_managed: false },
          }
        );
      }
      return { status: 200, data: { data: [alice], total: 1, page: 1, pageSize: 25 } };
    },
  });
}

function renderApp(path: string) {
  // StrictMode as in main.tsx: the one-shot notice must survive its double render.
  return render(
    <StrictMode>
      <AuthProviders>
        <NotificationProvider>
          <MemoryRouter initialEntries={[path]}>
            <AppRoutes />
            <Where />
          </MemoryRouter>
        </NotificationProvider>
      </AuthProviders>
    </StrictMode>,
  );
}

async function changeOwnRole() {
  const user = userEvent.setup();
  await user.click(await screen.findByRole('combobox', { name: 'Role of alice' }));
  await user.click(await screen.findByRole('option', { name: 'devops' }));
}

describe('own account change through the real session and route guard', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    sessionStorage.clear();
    localStorage.clear();
  });

  it('ends on the login page with the notice visible', async () => {
    setup({ value: false });
    renderApp('/admin/users');

    await changeOwnRole();

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(await screen.findByText(NOTICE)).toBeInTheDocument();
    expect(screen.getByTestId('where')).toHaveTextContent(/^\/login/);
  });

  it('shows the notice once: a later visit of the login page has none', async () => {
    const ended = { value: false };
    setup(ended);
    const first = renderApp('/admin/users');
    await changeOwnRole();
    expect(await screen.findByText(NOTICE)).toBeInTheDocument();
    first.unmount();

    // A reload or a second visit: the session is gone, the backend refuses the refresh.
    renderApp('/login');
    await screen.findByRole('heading', { name: 'Sign in' });
    await waitFor(() => expect(screen.getByLabelText(/username/i)).toBeInTheDocument());
    expect(screen.queryByText(NOTICE)).not.toBeInTheDocument();
  });

  it('does not show a notice to a visitor who just opens the login page', async () => {
    setup({ value: true });
    renderApp('/login');
    await screen.findByLabelText(/username/i);
    expect(screen.queryByText(NOTICE)).not.toBeInTheDocument();
  });
});

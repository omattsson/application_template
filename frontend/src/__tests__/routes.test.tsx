import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Link, MemoryRouter } from 'react-router-dom';
import AppRoutes from '../routes';
import { useAuth } from '../context/AuthContext';
import type { AuthContextValue } from '../context/AuthContext';
import { useUIConfig } from '../context/UIConfigContext';
import type { UIConfig } from '../context/UIConfigContext';

vi.mock('../context/AuthContext', () => ({ useAuth: vi.fn() }));
vi.mock('../context/UIConfigContext', () => ({ useUIConfig: vi.fn() }));
vi.mock('../pages/ForgotPassword', () => ({ default: () => <h1>Forgot password page</h1> }));
vi.mock('../pages/ResetPassword', () => ({ default: () => <h1>Reset password page</h1> }));
vi.mock('../pages/Login', () => ({ default: () => <h1>Login page</h1> }));
vi.mock('../pages/AuthCallback', () => ({ default: () => <h1>Callback page</h1> }));

function authState(overrides: Partial<AuthContextValue> = {}): AuthContextValue {
  return {
    authEnabled: false,
    isLoading: false,
    isAuthenticated: false,
    user: null,
    role: null,
    login: vi.fn(),
    logout: vi.fn(),
    loginWithOIDC: vi.fn(),
    ...overrides,
  };
}

vi.mock('../pages/Home', () => ({ default: () => <h1>Home page</h1> }));
vi.mock('../pages/Health', () => ({ default: () => <h1>Health page</h1> }));
vi.mock('../pages/Admin/Users', () => ({ default: () => <h1>Admin users page</h1> }));
vi.mock('../pages/Account/ApiKeys', () => ({ default: () => <h1>API keys page</h1> }));
vi.mock('../pages/Admin/AuditLogs', () => ({ default: () => <h1>Audit log page</h1> }));
vi.mock('../pages/Account/Profile', () => ({ default: () => <h1>Account page</h1> }));
const itemsState = { shouldThrow: false };
vi.mock('../pages/Items', () => ({
  default: () => {
    if (itemsState.shouldThrow) throw new Error('Items exploded');
    return <h1>Items page</h1>;
  },
}));

function uiConfig(overrides: Partial<UIConfig> = {}): { config: UIConfig; loading: boolean } {
  return {
    config: {
      title: 'Test App',
      auth_enabled: true,
      oidc_enabled: false,
      local_auth_enabled: true,
      password_reset_enabled: false,
      ...overrides,
    },
    loading: false,
  };
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppRoutes />
    </MemoryRouter>,
  );
}

describe('AppRoutes', () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue(authState());
    vi.mocked(useUIConfig).mockReturnValue(uiConfig());
  });

  afterEach(() => {
    itemsState.shouldThrow = false;
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it.each([
    ['/', 'Home page'],
    ['/health', 'Health page'],
    ['/items', 'Items page'],
  ])('renders %s', (path, heading) => {
    renderAt(path);
    expect(screen.getByRole('heading', { name: heading })).toBeInTheDocument();
  });

  it.each([
    ['/login', 'Login page'],
    ['/auth/callback', 'Callback page'],
  ])('renders the public auth route %s', (path, heading) => {
    vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
    renderAt(path);
    expect(screen.getByRole('heading', { name: heading })).toBeInTheDocument();
  });

  it.each(['/', '/health', '/items'])('protects %s when auth is enabled', (path) => {
    vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
    renderAt(path);
    expect(screen.getByRole('heading', { name: 'Login page' })).toBeInTheDocument();
  });

  describe('/admin/users', () => {
    const ALICE = { id: 'u1', username: 'alice', display_name: 'Alice', role: 'admin' as const };

    it('renders for an admin', () => {
      vi.mocked(useAuth).mockReturnValue(
        authState({ authEnabled: true, isAuthenticated: true, user: ALICE, role: 'admin' }),
      );
      renderAt('/admin/users');
      expect(screen.getByRole('heading', { name: 'Admin users page' })).toBeInTheDocument();
    });

    it.each(['user', 'devops'] as const)('shows Access denied (no redirect) to the role %s', (role) => {
      vi.mocked(useAuth).mockReturnValue(
        authState({ authEnabled: true, isAuthenticated: true, user: { ...ALICE, role }, role }),
      );
      renderAt('/admin/users');
      expect(screen.getByRole('heading', { name: 'Access denied' })).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Admin users page' })).not.toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Login page' })).not.toBeInTheDocument();
    });

    it('sends an anonymous visitor to the login page', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
      renderAt('/admin/users');
      expect(screen.getByRole('heading', { name: 'Login page' })).toBeInTheDocument();
    });
  });

  describe('/account/api-keys', () => {
    const SIGNED_IN = { id: 'u1', username: 'alice', display_name: 'Alice', role: 'user' as const };

    it('renders for a signed-in user when auth is enabled', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: SIGNED_IN, role: 'user' }));
      renderAt('/account/api-keys');
      expect(screen.getByRole('heading', { name: 'API keys page' })).toBeInTheDocument();
    });

    it('sends an anonymous visitor to the login page', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
      renderAt('/account/api-keys');
      expect(screen.getByRole('heading', { name: 'Login page' })).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'API keys page' })).not.toBeInTheDocument();
    });

    it('has no route with auth off (NotFound)', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: false }));
      renderAt('/account/api-keys');
      expect(screen.getByText('Page not found')).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'API keys page' })).not.toBeInTheDocument();
    });

    it('shows the loading state while the session loads, not NotFound', () => {
      vi.mocked(useUIConfig).mockReturnValue({ config: uiConfig().config, loading: true });
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isLoading: true }));
      renderAt('/account/api-keys');
      expect(screen.getByRole('status')).toBeInTheDocument();
      expect(screen.queryByText('Page not found')).not.toBeInTheDocument();
    });
  });

  describe('/account', () => {
    const SIGNED_IN = { id: 'u1', username: 'alice', display_name: 'Alice', role: 'user' as const };

    it('renders for a signed-in user with auth and local sign-in on', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: SIGNED_IN, role: 'user' }));
      renderAt('/account');
      expect(screen.getByRole('heading', { name: 'Account page' })).toBeInTheDocument();
    });

    it('also renders with a trailing slash', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: SIGNED_IN, role: 'user' }));
      renderAt('/account/');
      expect(screen.getByRole('heading', { name: 'Account page' })).toBeInTheDocument();
    });

    it('sends an anonymous visitor to the login page', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
      renderAt('/account');
      expect(screen.getByRole('heading', { name: 'Login page' })).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Account page' })).not.toBeInTheDocument();
    });

    it('has no route with auth off (NotFound)', () => {
      vi.mocked(useUIConfig).mockReturnValue(uiConfig({ auth_enabled: false, local_auth_enabled: false }));
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: false }));
      renderAt('/account');
      expect(screen.getByText('Page not found')).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Account page' })).not.toBeInTheDocument();
    });

    it('has no route in OIDC-only mode, without local sign-in (NotFound)', () => {
      vi.mocked(useUIConfig).mockReturnValue(uiConfig({ oidc_enabled: true, local_auth_enabled: false }));
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: SIGNED_IN, role: 'user' }));
      renderAt('/account');
      expect(screen.getByText('Page not found')).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Account page' })).not.toBeInTheDocument();
    });

    it('shows the loading state while the session loads, not NotFound', () => {
      vi.mocked(useUIConfig).mockReturnValue({ config: uiConfig().config, loading: true });
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isLoading: true }));
      renderAt('/account');
      expect(screen.getByRole('status')).toBeInTheDocument();
      expect(screen.queryByText('Page not found')).not.toBeInTheDocument();
    });

    it('shows the loading state while only the UI config loads', () => {
      vi.mocked(useUIConfig).mockReturnValue({ config: uiConfig({ auth_enabled: false, local_auth_enabled: false }).config, loading: true });
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: false, isLoading: true }));
      renderAt('/account');
      expect(screen.getByRole('status')).toBeInTheDocument();
      expect(screen.queryByText('Page not found')).not.toBeInTheDocument();
    });
  });

  describe('/admin/audit-logs', () => {
    const ALICE_ADMIN = { id: 'u1', username: 'alice', display_name: 'Alice', role: 'admin' as const };

    it('renders for an admin when auth is enabled', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: ALICE_ADMIN, role: 'admin' }));
      renderAt('/admin/audit-logs');
      expect(screen.getByRole('heading', { name: 'Audit log page' })).toBeInTheDocument();
    });

    it.each(['user', 'devops'] as const)('shows Access denied (no redirect) to the role %s', (role) => {
      vi.mocked(useAuth).mockReturnValue(
        authState({ authEnabled: true, isAuthenticated: true, user: { ...ALICE_ADMIN, role }, role }),
      );
      renderAt('/admin/audit-logs');
      expect(screen.getByRole('heading', { name: 'Access denied' })).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Audit log page' })).not.toBeInTheDocument();
    });

    it('sends an anonymous visitor to the login page', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
      renderAt('/admin/audit-logs');
      expect(screen.getByRole('heading', { name: 'Login page' })).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Audit log page' })).not.toBeInTheDocument();
    });

    it('has no route with auth off (NotFound)', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: false }));
      renderAt('/admin/audit-logs');
      expect(screen.getByText('Page not found')).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'Audit log page' })).not.toBeInTheDocument();
    });

    it('shows the loading state while the session loads, not NotFound', () => {
      vi.mocked(useUIConfig).mockReturnValue({ config: uiConfig().config, loading: true });
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isLoading: true }));
      renderAt('/admin/audit-logs');
      expect(screen.getByRole('status')).toBeInTheDocument();
      expect(screen.queryByText('Page not found')).not.toBeInTheDocument();
    });
  });

  describe('password reset pages', () => {
    const PATHS = [
      ['/forgot-password', 'Forgot password page'],
      ['/reset-password', 'Reset password page'],
    ] as const;

    it.each(PATHS)('renders %s for a visitor when the reset is on', (path, heading) => {
      vi.mocked(useUIConfig).mockReturnValue(uiConfig({ password_reset_enabled: true }));
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
      renderAt(path);
      expect(screen.getByRole('heading', { name: heading })).toBeInTheDocument();
    });

    it.each(PATHS)('has no %s route when the reset is off (NotFound)', (path) => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
      renderAt(path);
      expect(screen.getByText('Page not found')).toBeInTheDocument();
    });

    it.each(PATHS)('has no %s route for a signed-in user (NotFound)', (path) => {
      vi.mocked(useUIConfig).mockReturnValue(uiConfig({ password_reset_enabled: true }));
      vi.mocked(useAuth).mockReturnValue(
        authState({ authEnabled: true, isAuthenticated: true, user: { id: 'u1', username: 'alice', display_name: 'Alice', role: 'user' }, role: 'user' }),
      );
      renderAt(path);
      expect(screen.getByText('Page not found')).toBeInTheDocument();
    });

    it.each(PATHS)('shows the loading state on %s while the session loads, not NotFound', (path) => {
      vi.mocked(useUIConfig).mockReturnValue({ config: uiConfig().config, loading: true });
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isLoading: true }));
      renderAt(path);
      expect(screen.getByRole('status')).toBeInTheDocument();
      expect(screen.queryByText('Page not found')).not.toBeInTheDocument();
    });
  });

  it('renders NotFound for an unknown path', () => {
    renderAt('/no/such/page');
    expect(screen.getByText('404')).toBeInTheDocument();
    expect(screen.getByText('Page not found')).toBeInTheDocument();
  });

  it('contains a page crash in the error boundary and recovers on navigation', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    itemsState.shouldThrow = true;
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={['/items']}>
        <Link to="/health">Go to health</Link>
        <AppRoutes />
      </MemoryRouter>,
    );
    expect(screen.getByRole('heading', { name: 'Something went wrong' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Go to health' })).toBeInTheDocument();

    await user.click(screen.getByRole('link', { name: 'Go to health' }));
    expect(screen.getByRole('heading', { name: 'Health page' })).toBeInTheDocument();
  });
});

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Link, MemoryRouter } from 'react-router-dom';
import AppRoutes from '../routes';
import { useAuth } from '../context/AuthContext';
import type { AuthContextValue } from '../context/AuthContext';

vi.mock('../context/AuthContext', () => ({ useAuth: vi.fn() }));
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
const itemsState = { shouldThrow: false };
vi.mock('../pages/Items', () => ({
  default: () => {
    if (itemsState.shouldThrow) throw new Error('Items exploded');
    return <h1>Items page</h1>;
  },
}));

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

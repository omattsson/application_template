import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import ProtectedRoute from '../index';
import { useAuth } from '../../../context/AuthContext';
import type { AuthContextValue } from '../../../context/AuthContext';

vi.mock('../../../context/AuthContext', () => ({ useAuth: vi.fn() }));

function authState(overrides: Partial<AuthContextValue>): AuthContextValue {
  return {
    authEnabled: true,
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

function LoginProbe() {
  const location = useLocation();
  return <p>login page {location.search}</p>;
}

function renderAt(path: string, roles?: Array<'user' | 'devops' | 'admin'>) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/login" element={<LoginProbe />} />
        <Route
          path="*"
          element={
            <ProtectedRoute roles={roles}>
              <h1>Secret page</h1>
            </ProtectedRoute>
          }
        />
      </Routes>
    </MemoryRouter>,
  );
}

describe('ProtectedRoute', () => {
  afterEach(() => {
    vi.clearAllMocks();
  });

  it('renders the children directly when auth is disabled', () => {
    vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: false }));
    renderAt('/items', ['admin']);
    expect(screen.getByRole('heading', { name: 'Secret page' })).toBeInTheDocument();
  });

  it('shows a loading state (not the login page) while the session is restored', () => {
    vi.mocked(useAuth).mockReturnValue(authState({ isLoading: true }));
    renderAt('/items');
    expect(screen.getByRole('status')).toBeInTheDocument();
    expect(screen.queryByText(/login page/)).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Secret page' })).not.toBeInTheDocument();
  });

  it('redirects an anonymous user to /login and keeps the intended path', () => {
    vi.mocked(useAuth).mockReturnValue(authState({}));
    renderAt('/items?limit=5');
    expect(screen.getByText(`login page ?next=${encodeURIComponent('/items?limit=5')}`)).toBeInTheDocument();
  });

  it('renders the children for an authenticated user', () => {
    vi.mocked(useAuth).mockReturnValue(authState({ isAuthenticated: true, role: 'user' }));
    renderAt('/items');
    expect(screen.getByRole('heading', { name: 'Secret page' })).toBeInTheDocument();
  });

  it("roles={['admin']} blocks a user with a Forbidden state, not a redirect", () => {
    vi.mocked(useAuth).mockReturnValue(authState({ isAuthenticated: true, role: 'user' }));
    renderAt('/admin', ['admin']);
    expect(screen.getByRole('heading', { name: 'Access denied' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to Home' })).toHaveAttribute('href', '/');
    expect(screen.queryByRole('heading', { name: 'Secret page' })).not.toBeInTheDocument();
    expect(screen.queryByText(/login page/)).not.toBeInTheDocument();
  });

  it("roles={['admin']} lets an admin in", () => {
    vi.mocked(useAuth).mockReturnValue(authState({ isAuthenticated: true, role: 'admin' }));
    renderAt('/admin', ['admin']);
    expect(screen.getByRole('heading', { name: 'Secret page' })).toBeInTheDocument();
  });

  it('blocks a session without a known role when roles are required', () => {
    vi.mocked(useAuth).mockReturnValue(authState({ isAuthenticated: true, role: null }));
    renderAt('/admin', ['admin', 'devops']);
    expect(screen.getByRole('heading', { name: 'Access denied' })).toBeInTheDocument();
  });
});

import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import Home from '../index';
import { useAuth } from '../../../context/AuthContext';
import type { AuthContextValue } from '../../../context/AuthContext';
import { useUIConfig } from '../../../context/UIConfigContext';

vi.mock('../../../context/AuthContext', () => ({ useAuth: vi.fn() }));
vi.mock('../../../context/UIConfigContext', () => ({ useUIConfig: vi.fn() }));

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

const USER = { id: 'u1', username: 'alice', display_name: 'Alice Person', role: 'user' as const };

function renderHome() {
  return render(
    <MemoryRouter>
      <Home />
    </MemoryRouter>,
  );
}

function startLinks() {
  const section = screen.getByRole('region', { name: 'Where to start' });
  return within(section).getAllByRole('link');
}

describe('Home Page', () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue(authState());
    vi.mocked(useUIConfig).mockReturnValue({
      config: {
        title: 'Test App',
        auth_enabled: false,
        oidc_enabled: false,
        local_auth_enabled: false,
        password_reset_enabled: false,
      },
      loading: false,
    });
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  it('renders the welcome heading with the configured title', () => {
    renderHome();
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Welcome to Test App');
  });

  it('renders a short introduction', () => {
    renderHome();
    expect(screen.getByText(/starting point for your application/i)).toBeInTheDocument();
  });

  it('links to Items and Health under "Where to start" with auth off', () => {
    renderHome();
    expect(screen.getByRole('heading', { level: 2, name: 'Where to start' })).toBeInTheDocument();
    const links = startLinks();
    expect(links.map((a) => a.textContent)).toEqual([expect.stringContaining('Health'), expect.stringContaining('Items')]);
    expect(screen.getByRole('link', { name: /Items/ })).toHaveAttribute('href', '/items');
    expect(screen.getByRole('link', { name: /Health/ })).toHaveAttribute('href', '/health');
    expect(screen.queryByRole('link', { name: /Users/ })).not.toBeInTheDocument();
  });

  it('does not list the Home page itself', () => {
    renderHome();
    expect(within(screen.getByRole('region', { name: 'Where to start' })).queryByRole('link', { name: /^Home/ })).not.toBeInTheDocument();
  });

  it('adds Users and Audit log for an admin with auth on', () => {
    vi.mocked(useAuth).mockReturnValue(
      authState({ authEnabled: true, isAuthenticated: true, user: { ...USER, role: 'admin' }, role: 'admin' }),
    );
    renderHome();
    expect(screen.getByRole('link', { name: /Users/ })).toHaveAttribute('href', '/admin/users');
    expect(screen.getByRole('link', { name: /Audit log/ })).toHaveAttribute('href', '/admin/audit-logs');
  });

  it('omits Users and Audit log for a non-admin with auth on', () => {
    vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: USER, role: 'user' }));
    renderHome();
    expect(screen.getByRole('link', { name: /Items/ })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /Users/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /Audit log/ })).not.toBeInTheDocument();
  });

  it('omits Users and Audit log for an admin role with auth off', () => {
    vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: false, role: 'admin' }));
    renderHome();
    expect(screen.queryByRole('link', { name: /Users/ })).not.toBeInTheDocument();
  });

  it('shows no links while the session loads', () => {
    vi.mocked(useAuth).mockReturnValue(authState({ isLoading: true }));
    renderHome();
    expect(screen.queryByRole('region', { name: 'Where to start' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
  });

  it('has a "Make this your own" section in plain text, without links', () => {
    renderHome();
    const section = screen.getByRole('region', { name: 'Make this your own' });
    expect(within(section).getByRole('heading', { level: 2, name: 'Make this your own' })).toBeInTheDocument();
    expect(within(section).getByText(/EXTENDING\.md/)).toBeInTheDocument();
    expect(within(section).getByText(/APP_TITLE/)).toBeInTheDocument();
    expect(within(section).queryByRole('link')).not.toBeInTheDocument();
  });

  it('shows no title in the heading while the UI config loads', () => {
    vi.mocked(useUIConfig).mockReturnValue({
      config: {
        title: 'Application Template',
        auth_enabled: false,
        oidc_enabled: false,
        local_auth_enabled: false,
        password_reset_enabled: false,
      },
      loading: true,
    });
    renderHome();
    expect(screen.getByRole('heading', { level: 1 })).not.toHaveTextContent('Application Template');
  });
});

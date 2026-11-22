import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import AppRoutes from '../routes';
import { useAuth } from '../context/AuthContext';
import { useUIConfig } from '../context/UIConfigContext';

// Each page module is imported once per file (React.lazy keeps the result), so the
// pages that these tests use are not shared with routes.test.tsx.
const home = vi.hoisted(() => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { gate, release };
});

vi.mock('../context/AuthContext', () => ({ useAuth: vi.fn() }));
vi.mock('../context/UIConfigContext', () => ({ useUIConfig: vi.fn() }));
// A page chunk that is slow to arrive.
vi.mock('../pages/Home', async () => {
  await home.gate;
  return { default: () => <h1>Home page</h1> };
});

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppRoutes />
    </MemoryRouter>,
  );
}

describe('lazy routes', () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue({
      authEnabled: false,
      isLoading: false,
      isAuthenticated: false,
      user: null,
      role: null,
      login: vi.fn(),
      logout: vi.fn(),
      loginWithOIDC: vi.fn(),
    });
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

  it('shows the loading state until the page chunk has arrived, then the page', async () => {
    renderAt('/');
    expect(screen.getByRole('status')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Home page' })).not.toBeInTheDocument();

    home.release();
    expect(await screen.findByRole('heading', { name: 'Home page' })).toBeInTheDocument();
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('serves NotFound without loading a chunk', () => {
    renderAt('/no/such/page');
    expect(screen.getByText('Page not found')).toBeInTheDocument();
  });
});

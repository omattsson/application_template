import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import Layout from '../index';
import { ThemeModeProvider } from '../../../context/ThemeContext';
import { useAuth } from '../../../context/AuthContext';
import type { AuthContextValue } from '../../../context/AuthContext';
import { useUIConfig } from '../../../context/UIConfigContext';
import type { UIConfig } from '../../../context/UIConfigContext';

vi.mock('../../../context/AuthContext', () => ({ useAuth: vi.fn() }));
vi.mock('../../../context/UIConfigContext', () => ({ useUIConfig: vi.fn() }));

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

function authState(overrides: Partial<AuthContextValue> = {}): AuthContextValue {
  return {
    authEnabled: false,
    isLoading: false,
    isAuthenticated: false,
    user: null,
    role: null,
    login: vi.fn(),
    logout: vi.fn().mockResolvedValue(undefined),
    loginWithOIDC: vi.fn(),
    ...overrides,
  };
}

const ALICE = {
  id: 'u1',
  username: 'alice',
  display_name: 'Alice Person',
  role: 'user' as const,
};

/** JSDOM has no matchMedia: answer every `max-width` query (theme.breakpoints.down) with `narrow`. */
function mockViewport(narrow: boolean) {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: narrow && query.includes('max-width'),
    media: query,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }));
}

function renderLayout(children: React.ReactNode = <div>Test Content</div>, path = '/') {
  return render(
    <ThemeModeProvider>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/login" element={<p>login page</p>} />
          <Route path="*" element={<Layout>{children}</Layout>} />
        </Routes>
      </MemoryRouter>
    </ThemeModeProvider>
  );
}

describe('Layout', () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue(authState());
    vi.mocked(useUIConfig).mockReturnValue(uiConfig());
  });

  afterEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    // @ts-expect-error JSDOM has no matchMedia: remove the mock again
    delete window.matchMedia;
    document.title = '';
  });

  describe('title', () => {
    it('shows the configured title in the app bar', () => {
      renderLayout();
      expect(screen.getByText('Test App')).toBeInTheDocument();
    });

    it('shows the configured title in the footer, after the year', () => {
      renderLayout();
      const year = new Date().getFullYear();
      expect(screen.getByRole('contentinfo')).toHaveTextContent(`© ${year} Test App`);
    });

    it('sets the document title once the config has loaded', () => {
      renderLayout();
      expect(document.title).toBe('Test App');
    });

    it('shows no title and keeps the document title while the config loads (no flash of the fallback)', () => {
      document.title = 'static title';
      vi.mocked(useUIConfig).mockReturnValue({ ...uiConfig({ title: 'Application Template' }), loading: true });
      renderLayout();
      expect(screen.queryByText('Application Template')).not.toBeInTheDocument();
      expect(screen.getByRole('contentinfo')).not.toHaveTextContent('Application Template');
      expect(document.title).toBe('static title');
    });

    it('shows the fallback title when the config could not be read (loading ended)', () => {
      vi.mocked(useUIConfig).mockReturnValue(uiConfig({ title: 'Application Template' }));
      renderLayout();
      expect(screen.getByText('Application Template')).toBeInTheDocument();
      expect(document.title).toBe('Application Template');
    });

    it('follows a title that changes after the first render', () => {
      const { rerender } = renderLayout();
      vi.mocked(useUIConfig).mockReturnValue(uiConfig({ title: 'Other Name' }));
      rerender(
        <ThemeModeProvider>
          <MemoryRouter>
            <Layout>
              <div>Test Content</div>
            </Layout>
          </MemoryRouter>
        </ThemeModeProvider>,
      );
      expect(document.title).toBe('Other Name');
    });
  });

  it('renders Home, Health, and Items nav links', () => {
    renderLayout();
    expect(screen.getByRole('link', { name: /home/i })).toHaveAttribute('href', '/');
    expect(screen.getByRole('link', { name: /health/i })).toHaveAttribute('href', '/health');
    expect(screen.getByRole('link', { name: /items/i })).toHaveAttribute('href', '/items');
  });

  it('renders children content', () => {
    renderLayout(<p>Child paragraph</p>);
    expect(screen.getByText('Child paragraph')).toBeInTheDocument();
  });

  it('renders footer with current year', () => {
    renderLayout();
    const year = new Date().getFullYear();
    expect(screen.getByText(new RegExp(`© ${year}`))).toBeInTheDocument();
  });

  it('has a theme toggle that switches between light and dark and is persisted', async () => {
    const user = userEvent.setup();
    renderLayout();

    await user.click(screen.getByRole('button', { name: 'Switch to dark mode' }));
    expect(screen.getByRole('button', { name: 'Switch to light mode' })).toBeInTheDocument();
    expect(localStorage.getItem('theme-mode')).toBe('dark');

    await user.click(screen.getByRole('button', { name: 'Switch to light mode' }));
    expect(screen.getByRole('button', { name: 'Switch to dark mode' })).toBeInTheDocument();
    expect(localStorage.getItem('theme-mode')).toBe('light');
  });

  describe('auth', () => {
    it('shows no user name and no Logout button when auth is disabled', () => {
      renderLayout();
      expect(screen.queryByRole('button', { name: /log out/i })).not.toBeInTheDocument();
      expect(screen.queryByText('Alice Person')).not.toBeInTheDocument();
    });

    it('shows the user name and a Logout button when auth is enabled and the user is signed in', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: ALICE, role: 'user' }));
      renderLayout();
      expect(screen.getByText('Alice Person')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: /log out/i })).toBeInTheDocument();
    });

    it('hides Logout and the navigation for an anonymous user when auth is enabled', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
      renderLayout();
      expect(screen.queryByRole('button', { name: /log out/i })).not.toBeInTheDocument();
      expect(screen.queryByRole('link', { name: /items/i })).not.toBeInTheDocument();
      expect(screen.getByText('Test App')).toBeInTheDocument();
    });

    it.each([
      ['the UI config loads (auth mode unknown)', { isLoading: true, authEnabled: false }],
      ['the session is restored with auth on', { isLoading: true, authEnabled: true }],
    ])('hides the navigation while %s (no flicker)', (_name, state) => {
      vi.mocked(useAuth).mockReturnValue(authState(state));
      renderLayout();
      expect(screen.queryByRole('link', { name: /items/i })).not.toBeInTheDocument();
      expect(screen.queryByRole('link', { name: /home/i })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: /log out/i })).not.toBeInTheDocument();
      expect(screen.getByText('Test App')).toBeInTheDocument();
    });

    describe('Users link', () => {
      const ADMIN = { ...ALICE, role: 'admin' as const };

      it('is shown to an admin when auth is enabled', () => {
        vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: ADMIN, role: 'admin' }));
        renderLayout();
        expect(screen.getByRole('link', { name: 'Users' })).toHaveAttribute('href', '/admin/users');
      });

      it.each(['user', 'devops'] as const)('is hidden from the role %s', (role) => {
        vi.mocked(useAuth).mockReturnValue(
          authState({ authEnabled: true, isAuthenticated: true, user: { ...ALICE, role }, role }),
        );
        renderLayout();
        expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
      });

      it('is hidden when auth is disabled (there are no users to manage)', () => {
        vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: false, role: 'admin' }));
        renderLayout();
        expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
      });

      it.each([
        ['an anonymous visitor', { authEnabled: true, role: 'admin' as const }],
        ['an admin while the session is restored', { authEnabled: true, isLoading: true, isAuthenticated: true, user: ADMIN, role: 'admin' as const }],
      ])('is hidden from %s', (_name, state) => {
        vi.mocked(useAuth).mockReturnValue(authState(state));
        renderLayout();
        expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
      });
    });

    describe('Audit log link', () => {
      const ADMIN = { ...ALICE, role: 'admin' as const };

      it('is shown to an admin when auth is enabled and points to the audit log page', () => {
        vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: ADMIN, role: 'admin' }));
        renderLayout();
        expect(screen.getByRole('link', { name: 'Audit log' })).toHaveAttribute('href', '/admin/audit-logs');
      });

      it.each(['user', 'devops'] as const)('is hidden from the role %s', (role) => {
        vi.mocked(useAuth).mockReturnValue(
          authState({ authEnabled: true, isAuthenticated: true, user: { ...ALICE, role }, role }),
        );
        renderLayout();
        expect(screen.queryByRole('link', { name: 'Audit log' })).not.toBeInTheDocument();
      });

      it('is hidden when auth is disabled', () => {
        vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: false, role: 'admin' }));
        renderLayout();
        expect(screen.queryByRole('link', { name: 'Audit log' })).not.toBeInTheDocument();
      });

      it.each([
        ['an anonymous visitor', { authEnabled: true, role: 'admin' as const }],
        ['an admin while the session is restored', { authEnabled: true, isLoading: true, isAuthenticated: true, user: ADMIN, role: 'admin' as const }],
      ])('is hidden from %s', (_name, state) => {
        vi.mocked(useAuth).mockReturnValue(authState(state));
        renderLayout();
        expect(screen.queryByRole('link', { name: 'Audit log' })).not.toBeInTheDocument();
      });
    });

    describe('API keys link', () => {
      it('is shown to a signed-in user when auth is enabled, whatever the role', () => {
        vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: ALICE, role: 'user' }));
        renderLayout();
        expect(screen.getByRole('link', { name: 'API keys' })).toHaveAttribute('href', '/account/api-keys');
      });

      it('is shown to an admin as well', () => {
        vi.mocked(useAuth).mockReturnValue(
          authState({ authEnabled: true, isAuthenticated: true, user: { ...ALICE, role: 'admin' }, role: 'admin' }),
        );
        renderLayout();
        expect(screen.getByRole('link', { name: 'API keys' })).toBeInTheDocument();
      });

      it.each([
        ['auth is disabled', { authEnabled: false, isAuthenticated: false, user: null, role: null }],
        ['an anonymous visitor', { authEnabled: true, isAuthenticated: false, user: null, role: null }],
        ['the session is still restored', { authEnabled: true, isLoading: true, isAuthenticated: true, user: ALICE, role: 'user' as const }],
      ])('is hidden when %s', (_name, state) => {
        vi.mocked(useAuth).mockReturnValue(authState(state));
        renderLayout();
        expect(screen.queryByRole('link', { name: 'API keys' })).not.toBeInTheDocument();
      });
    });

    describe('account link (the user name)', () => {
      const signedIn = () =>
        vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true, isAuthenticated: true, user: ALICE, role: 'user' }));

      it('is a link to /account where the page exists (auth on, signed in, local sign-in on)', () => {
        signedIn();
        renderLayout();
        const link = screen.getByRole('link', { name: 'Alice Person' });
        expect(link).toHaveAttribute('href', '/account');
        expect(link).toHaveAttribute('data-testid', 'current-user');
      });

      it('is plain text without local sign-in (the route does not exist)', () => {
        signedIn();
        vi.mocked(useUIConfig).mockReturnValue(uiConfig({ oidc_enabled: true, local_auth_enabled: false }));
        renderLayout();
        expect(screen.queryByRole('link', { name: 'Alice Person' })).not.toBeInTheDocument();
        expect(screen.getByTestId('current-user')).toHaveTextContent('Alice Person');
        expect(screen.getByTestId('current-user').tagName).not.toBe('A');
      });

      it('is not shown at all with auth off', () => {
        vi.mocked(useUIConfig).mockReturnValue(uiConfig({ auth_enabled: false, local_auth_enabled: false }));
        renderLayout();
        expect(screen.queryByTestId('current-user')).not.toBeInTheDocument();
        expect(screen.queryByRole('link', { name: /account/i })).not.toBeInTheDocument();
      });

      it('keeps the API keys link and Log out next to it', () => {
        signedIn();
        renderLayout();
        expect(screen.getByRole('link', { name: 'API keys' })).toHaveAttribute('href', '/account/api-keys');
        expect(screen.getByRole('button', { name: /log out/i })).toBeInTheDocument();
      });

      it('is plain text while the session loads (nothing is shown)', () => {
        vi.mocked(useAuth).mockReturnValue(
          authState({ authEnabled: true, isLoading: true, isAuthenticated: true, user: ALICE, role: 'user' }),
        );
        renderLayout();
        expect(screen.queryByTestId('current-user')).not.toBeInTheDocument();
      });
    });

    it('Logout calls logout and goes to the login page', async () => {
      const logout = vi.fn().mockResolvedValue(undefined);
      vi.mocked(useAuth).mockReturnValue(
        authState({ authEnabled: true, isAuthenticated: true, user: ALICE, role: 'user', logout }),
      );
      renderLayout(<div>Test Content</div>, '/items');
      await userEvent.setup().click(screen.getByRole('button', { name: /log out/i }));
      expect(logout).toHaveBeenCalledTimes(1);
      expect(await screen.findByText('login page')).toBeInTheDocument();
    });
  });

  describe('below the sm breakpoint (drawer)', () => {
    const ADMIN = { ...ALICE, role: 'admin' as const };
    const signedInAdmin = () =>
      vi.mocked(useAuth).mockReturnValue(
        authState({ authEnabled: true, isAuthenticated: true, user: ADMIN, role: 'admin' }),
      );

    beforeEach(() => mockViewport(true));

    it('shows the menu button and no nav buttons or account items in the app bar', () => {
      signedInAdmin();
      renderLayout();
      const menu = screen.getByRole('button', { name: 'Open navigation menu' });
      expect(menu).toHaveAttribute('aria-expanded', 'false');
      expect(screen.queryByRole('link', { name: 'Items' })).not.toBeInTheDocument();
      expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
      expect(screen.queryByRole('link', { name: 'API keys' })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: /log out/i })).not.toBeInTheDocument();
      expect(screen.queryByTestId('current-user')).not.toBeInTheDocument();
    });

    it('keeps the title and the theme toggle in the app bar', () => {
      renderLayout();
      expect(screen.getByText('Test App')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Switch to dark mode' })).toBeInTheDocument();
    });

    it('has no menu button while the navigation is hidden', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ isLoading: true }));
      const { unmount } = renderLayout();
      expect(screen.queryByRole('button', { name: 'Open navigation menu' })).not.toBeInTheDocument();
      unmount();

      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: true }));
      renderLayout();
      expect(screen.queryByRole('button', { name: 'Open navigation menu' })).not.toBeInTheDocument();
    });

    it('opens a drawer with the links, the account items and Log out; aria-controls points at it', async () => {
      signedInAdmin();
      renderLayout();
      const menu = screen.getByRole('button', { name: 'Open navigation menu' });
      await userEvent.setup().click(menu);

      expect(menu).toHaveAttribute('aria-expanded', 'true');
      const drawer = document.getElementById(menu.getAttribute('aria-controls') ?? '');
      expect(drawer).not.toBeNull();
      const nav = within(screen.getByRole('navigation', { name: 'Main navigation' }));
      expect(nav.getAllByRole('link').map((a) => a.textContent)).toEqual([
        'Home',
        'Health',
        'Items',
        'Users',
        'Audit log',
        'Alice Person',
        'API keys',
      ]);
      expect(nav.getByRole('link', { name: 'Users' })).toHaveAttribute('href', '/admin/users');
      expect(nav.getByRole('link', { name: 'Alice Person' })).toHaveAttribute('href', '/account');
      expect(nav.getByRole('link', { name: 'API keys' })).toHaveAttribute('href', '/account/api-keys');
      expect(nav.getByRole('button', { name: /log out/i })).toBeInTheDocument();
      expect(drawer).toContainElement(screen.getByRole('navigation', { name: 'Main navigation' }));
    });

    it('applies the same visibility rules as the desktop bar', async () => {
      vi.mocked(useAuth).mockReturnValue(
        authState({ authEnabled: true, isAuthenticated: true, user: ALICE, role: 'user' }),
      );
      vi.mocked(useUIConfig).mockReturnValue(uiConfig({ oidc_enabled: true, local_auth_enabled: false }));
      renderLayout();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Open navigation menu' }));
      const nav = within(screen.getByRole('navigation', { name: 'Main navigation' }));
      expect(nav.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
      expect(nav.queryByRole('link', { name: 'Audit log' })).not.toBeInTheDocument();
      expect(nav.queryByRole('link', { name: 'Alice Person' })).not.toBeInTheDocument();
      expect(nav.getByTestId('current-user')).toHaveTextContent('Alice Person');
      expect(nav.getByRole('link', { name: 'API keys' })).toBeInTheDocument();
    });

    it('shows only the links, no account items, with auth off', async () => {
      renderLayout();
      await userEvent.setup().click(screen.getByRole('button', { name: 'Open navigation menu' }));
      const nav = within(screen.getByRole('navigation', { name: 'Main navigation' }));
      expect(nav.getAllByRole('link').map((a) => a.textContent)).toEqual(['Home', 'Health', 'Items']);
      expect(nav.queryByRole('button', { name: /log out/i })).not.toBeInTheDocument();
    });

    it('moves the focus into the drawer on open and back to the menu button on close', async () => {
      const user = userEvent.setup();
      renderLayout();
      const menu = screen.getByRole('button', { name: 'Open navigation menu' });
      await user.click(menu);
      const nav = screen.getByRole('navigation', { name: 'Main navigation' });
      await waitFor(() => expect(document.getElementById('navigation-drawer')).toContainElement(document.activeElement as HTMLElement));
      expect(nav).toBeInTheDocument();

      await user.keyboard('{Escape}');
      await waitFor(() => expect(screen.queryByRole('navigation', { name: 'Main navigation' })).not.toBeInTheDocument());
      expect(menu).toHaveFocus();
      expect(menu).toHaveAttribute('aria-expanded', 'false');
    });

    it('closes on a click on the backdrop', async () => {
      const user = userEvent.setup();
      renderLayout();
      await user.click(screen.getByRole('button', { name: 'Open navigation menu' }));
      const backdrop = document.querySelector('.MuiBackdrop-root');
      expect(backdrop).not.toBeNull();
      await user.click(backdrop as Element);
      await waitFor(() => expect(screen.queryByRole('navigation', { name: 'Main navigation' })).not.toBeInTheDocument());
    });

    it('closes when a link is selected', async () => {
      const user = userEvent.setup();
      renderLayout();
      await user.click(screen.getByRole('button', { name: 'Open navigation menu' }));
      await user.click(within(screen.getByRole('navigation', { name: 'Main navigation' })).getByRole('link', { name: 'Items' }));
      await waitFor(() => expect(screen.queryByRole('navigation', { name: 'Main navigation' })).not.toBeInTheDocument());
    });

    it('Log out in the drawer logs out and goes to the login page', async () => {
      const logout = vi.fn().mockResolvedValue(undefined);
      vi.mocked(useAuth).mockReturnValue(
        authState({ authEnabled: true, isAuthenticated: true, user: ALICE, role: 'user', logout }),
      );
      const user = userEvent.setup();
      renderLayout(<div>Test Content</div>, '/items');
      await user.click(screen.getByRole('button', { name: 'Open navigation menu' }));
      await user.click(within(screen.getByRole('navigation', { name: 'Main navigation' })).getByRole('button', { name: /log out/i }));
      expect(logout).toHaveBeenCalledTimes(1);
      expect(await screen.findByText('login page')).toBeInTheDocument();
    });
  });

  it('shows the full bar and no menu button at sm and up (default viewport)', () => {
    mockViewport(false);
    renderLayout();
    expect(screen.queryByRole('button', { name: 'Open navigation menu' })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Items' })).toBeInTheDocument();
  });
});

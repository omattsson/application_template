import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import App from '../App';

// Prevent WebSocketProvider from opening a real connection in jsdom
vi.mock('reconnecting-websocket', () => ({
  default: class MockRWS extends EventTarget {
    close() {}
  },
}));

vi.mock('../api/client', () => ({
  healthService: {
    checkLiveness: vi.fn().mockReturnValue(new Promise(() => {})),
    checkReadiness: vi.fn().mockReturnValue(new Promise(() => {})),
  },
  itemService: {
    list: vi.fn().mockResolvedValue([]),
  },
  // Auth off (the backend default).
  uiConfigService: {
    get: vi.fn().mockResolvedValue({
      title: 'Application Template',
      auth_enabled: false,
      oidc_enabled: false,
      local_auth_enabled: false,
      password_reset_enabled: false,
    }),
  },
  authService: { login: vi.fn(), logout: vi.fn() },
}));

// Smoke test for the toolchain and UI library: the whole app (theme provider,
// CssBaseline, router, layout, routes) renders and a themed page is reachable.
describe('theme smoke', () => {
  beforeEach(() => {
    window.history.pushState({}, '', '/items');
  });

  afterEach(() => {
    window.history.pushState({}, '', '/');
    vi.clearAllMocks();
  });

  it('renders the app inside the router and shows the Items heading', async () => {
    render(<App />);
    expect(await screen.findByRole('heading', { name: 'Items' })).toBeInTheDocument();
  });
});

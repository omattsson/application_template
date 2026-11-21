import { describe, it, expect, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import App from '../App';

// Prevent WebSocketProvider from opening a real connection in jsdom
vi.mock('reconnecting-websocket', () => ({
  default: class MockRWS extends EventTarget {
    close() {}
  },
}));

// Mock the health and item services to prevent actual API calls during render
vi.mock('../api/client', () => ({
  healthService: {
    checkLiveness: vi.fn().mockReturnValue(new Promise(() => {})),
    checkReadiness: vi.fn().mockReturnValue(new Promise(() => {})),
  },
  itemService: {
    list: vi.fn().mockReturnValue(new Promise(() => {})),
  },
  // Auth off (the backend default): the app behaves as without auth.
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

describe('App', () => {
  it('renders the layout with navigation', async () => {
    render(<App />);
    // The title shows once the UI config is loaded (empty while loading, no flash of the fallback).
    expect(await screen.findByText('Application Template')).toBeInTheDocument();
    // The links show once the UI config is loaded (hidden while loading, no flicker).
    // Home lists Health and Items as well, so look in the app bar.
    const bar = within(screen.getByRole('banner'));
    expect(await bar.findByRole('link', { name: /home/i })).toBeInTheDocument();
    expect(bar.getByRole('link', { name: /health/i })).toBeInTheDocument();
    expect(bar.getByRole('link', { name: /items/i })).toBeInTheDocument();
  });

  it('renders the Home page by default', async () => {
    render(<App />);
    expect(
      await screen.findByRole('heading', { name: /welcome to application template/i })
    ).toBeInTheDocument();
  });
});

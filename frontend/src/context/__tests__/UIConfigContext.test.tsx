import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { UIConfigProvider, useUIConfig, FALLBACK_UI_CONFIG } from '../UIConfigContext';
import { uiConfigService } from '../../api/client';

vi.mock('../../api/client', () => ({
  uiConfigService: { get: vi.fn() },
}));

function Probe() {
  const { config, loading } = useUIConfig();
  if (loading) return <p>loading</p>;
  return (
    <p>
      {config.title}|{String(config.auth_enabled)}|{String(config.oidc_enabled)}|{String(config.local_auth_enabled)}
    </p>
  );
}

describe('UIConfigProvider', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('shows loading first, then the server config', async () => {
    vi.mocked(uiConfigService.get).mockResolvedValue({
      title: 'My App',
      auth_enabled: true,
      oidc_enabled: false,
      local_auth_enabled: true,
    });
    render(
      <UIConfigProvider>
        <Probe />
      </UIConfigProvider>,
    );
    expect(screen.getByText('loading')).toBeInTheDocument();
    expect(await screen.findByText('My App|true|false|true')).toBeInTheDocument();
  });

  it('falls back to the default title with auth off when the request fails', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    vi.mocked(uiConfigService.get).mockRejectedValue(new Error('Network Error'));
    render(
      <UIConfigProvider>
        <Probe />
      </UIConfigProvider>,
    );
    expect(await screen.findByText('Application Template|false|false|false')).toBeInTheDocument();
    expect(FALLBACK_UI_CONFIG).toEqual({
      title: 'Application Template',
      auth_enabled: false,
      oidc_enabled: false,
      local_auth_enabled: false,
    });
  });

  it('fetches the config once', async () => {
    vi.mocked(uiConfigService.get).mockResolvedValue({ ...FALLBACK_UI_CONFIG });
    const { rerender } = render(
      <UIConfigProvider>
        <Probe />
      </UIConfigProvider>,
    );
    rerender(
      <UIConfigProvider>
        <Probe />
      </UIConfigProvider>,
    );
    await screen.findByText(/Application Template/);
    await waitFor(() => expect(uiConfigService.get).toHaveBeenCalledTimes(1));
  });

  it('throws when used outside the provider', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => render(<Probe />)).toThrow('useUIConfig must be used within <UIConfigProvider>');
  });
});

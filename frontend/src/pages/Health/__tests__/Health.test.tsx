import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, act } from '@testing-library/react';
import Health from '../index';
import { healthService } from '../../../api/client';

vi.mock('../../../api/client', () => ({
  healthService: {
    checkLiveness: vi.fn(),
    checkReadiness: vi.fn(),
  },
}));

describe('Health Page', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('shows a loading spinner initially', () => {
    (healthService.checkLiveness as ReturnType<typeof vi.fn>).mockReturnValue(
      new Promise(() => {})
    );
    (healthService.checkReadiness as ReturnType<typeof vi.fn>).mockReturnValue(
      new Promise(() => {})
    );

    render(<Health />);
    expect(screen.getByRole('progressbar')).toBeInTheDocument();
  });

  it('displays healthy status when both checks pass', async () => {
    (healthService.checkLiveness as ReturnType<typeof vi.fn>).mockResolvedValue({
      status: 'UP',
    });
    (healthService.checkReadiness as ReturnType<typeof vi.fn>).mockResolvedValue({
      status: 'UP',
    });

    render(<Health />);

    await waitFor(() => {
      expect(screen.getByText('System is live')).toBeInTheDocument();
    });
    expect(screen.getByText('System is ready to handle requests')).toBeInTheDocument();
  });

  it('displays unhealthy status when checks return non-UP', async () => {
    (healthService.checkLiveness as ReturnType<typeof vi.fn>).mockResolvedValue({
      status: 'DOWN',
    });
    (healthService.checkReadiness as ReturnType<typeof vi.fn>).mockResolvedValue({
      status: 'DOWN',
    });

    render(<Health />);

    await waitFor(() => {
      expect(screen.getByText('System is not responding')).toBeInTheDocument();
    });
    expect(screen.getByText('System is not ready')).toBeInTheDocument();
  });

  it('displays error alert when health checks fail', async () => {
    (healthService.checkLiveness as ReturnType<typeof vi.fn>).mockRejectedValue(
      new Error('Network Error')
    );
    (healthService.checkReadiness as ReturnType<typeof vi.fn>).mockRejectedValue(
      new Error('Network Error')
    );

    render(<Health />);

    await waitFor(() => {
      expect(screen.getByText('Failed to fetch health status')).toBeInTheDocument();
    });
  });

  it('renders the page heading', async () => {
    (healthService.checkLiveness as ReturnType<typeof vi.fn>).mockResolvedValue({
      status: 'UP',
    });
    (healthService.checkReadiness as ReturnType<typeof vi.fn>).mockResolvedValue({
      status: 'UP',
    });

    render(<Health />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('System Health');
    });
  });

  it('sets up a 30-second polling interval', async () => {
    vi.useFakeTimers();

    (healthService.checkLiveness as ReturnType<typeof vi.fn>).mockResolvedValue({
      status: 'UP',
    });
    (healthService.checkReadiness as ReturnType<typeof vi.fn>).mockResolvedValue({
      status: 'UP',
    });

    await act(async () => {
      render(<Health />);
    });

    // Capture initial call count
    const initialCount = (healthService.checkLiveness as ReturnType<typeof vi.fn>).mock.calls.length;

    // Advance past the 30s interval
    await act(async () => {
      vi.advanceTimersByTime(30000);
    });

    expect(healthService.checkLiveness).toHaveBeenCalledTimes(initialCount + 1);

    vi.useRealTimers();
  });
});

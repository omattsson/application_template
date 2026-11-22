import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import ConnectionStatus from '../index';
import { useWebSocketContext } from '../../../context/WebSocketContext';
import type { ConnectionStatus as Status } from '../../../hooks/useWebSocket';

vi.mock('../../../context/WebSocketContext', () => ({ useWebSocketContext: vi.fn() }));

function mockSocket(connectionStatus: Status, active = true) {
  vi.mocked(useWebSocketContext).mockReturnValue({
    lastMessage: null,
    connectionStatus,
    active,
    reconnectCount: 0,
    subscribe: vi.fn(),
  });
}

describe('ConnectionStatus', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it.each([
    ['open', 'Live'],
    ['connecting', 'Connecting'],
    ['closed', 'Offline'],
    ['closing', 'Offline'],
  ] as const)('shows the text "%s" -> %s as a status', (state, text) => {
    mockSocket(state);
    render(<ConnectionStatus />);
    expect(screen.getByRole('status')).toHaveTextContent(text);
  });

  it('describes the state in words for a screen reader', () => {
    mockSocket('closed');
    render(<ConnectionStatus />);
    expect(screen.getByRole('status')).toHaveAccessibleName('Offline: the data may be out of date');
  });

  it('uses a different icon for each state, not only a colour', () => {
    const icons = new Set<string | null>();
    for (const state of ['open', 'connecting', 'closed'] as const) {
      mockSocket(state);
      const { container, unmount } = render(<ConnectionStatus />);
      icons.add(container.querySelector('svg')?.getAttribute('data-testid') ?? null);
      unmount();
    }
    expect(icons.size).toBe(3);
    expect(icons.has(null)).toBe(false);
  });

  it('shows nothing when no socket is wanted', () => {
    mockSocket('closed', false);
    render(<ConnectionStatus />);
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });
});

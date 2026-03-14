import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, act } from '@testing-library/react';
import Items from '../index';
import { itemService } from '../../../api/client';
import { useWebSocketContext } from '../../../context/WebSocketContext';
import type { WebSocketMessage } from '../../../hooks/useWebSocket';

vi.mock('../../../context/WebSocketContext', () => ({
  useWebSocketContext: vi.fn(),
}));

vi.mock('../../../api/client', () => ({
  itemService: {
    list: vi.fn(),
  },
}));

const mockItems = [
  {
    id: 1,
    name: 'Widget',
    price: 9.99,
    description: 'A test widget',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  },
  {
    id: 2,
    name: 'Gadget',
    price: 19.99,
    created_at: '2026-01-02T00:00:00Z',
    updated_at: '2026-01-02T00:00:00Z',
  },
];

function setupSubscribeMock() {
  const handlers: Record<string, (msg: WebSocketMessage) => void> = {};
  const mockUnsubscribe = vi.fn();
  const mockSubscribe = vi
    .fn()
    .mockImplementation((type: string, handler: (msg: WebSocketMessage) => void) => {
      handlers[type] = handler;
      return mockUnsubscribe;
    });
  (useWebSocketContext as ReturnType<typeof vi.fn>).mockReturnValue({
    subscribe: mockSubscribe,
  });
  return { handlers, mockUnsubscribe, mockSubscribe };
}

describe('Items Page', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('shows a loading spinner initially', () => {
    setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockReturnValue(new Promise(() => {}));

    render(<Items />);

    expect(screen.getByRole('progressbar')).toBeInTheDocument();
  });

  it('renders items table when data loads', async () => {
    setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue(mockItems);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Items');
    });
    expect(screen.getByText('Widget')).toBeInTheDocument();
    expect(screen.getByText('$9.99')).toBeInTheDocument();
    expect(screen.getByText('Gadget')).toBeInTheDocument();
    expect(screen.getByText('$19.99')).toBeInTheDocument();
    expect(screen.getByText('A test widget')).toBeInTheDocument();
  });

  it('shows error alert when fetch fails', async () => {
    setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('Network error'));

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument();
    });
    expect(screen.getByText('Failed to load items')).toBeInTheDocument();
  });

  it('shows item.created toast with correct item name', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Items');
    });

    act(() => {
      handlers['item.created']({
        type: 'item.created',
        payload: { id: 3, name: 'NewWidget', price: 5.0, created_at: '', updated_at: '' },
      });
    });

    await waitFor(() => {
      expect(screen.getByText("Item 'NewWidget' created")).toBeInTheDocument();
    });
  });

  it('shows item.updated toast with correct item name', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Items');
    });

    act(() => {
      handlers['item.updated']({
        type: 'item.updated',
        payload: { id: 1, name: 'Widget Pro', price: 29.99, created_at: '', updated_at: '' },
      });
    });

    await waitFor(() => {
      expect(screen.getByText("Item 'Widget Pro' updated")).toBeInTheDocument();
    });
  });

  it('shows item.deleted toast', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Items');
    });

    act(() => {
      handlers['item.deleted']({ type: 'item.deleted', payload: { id: 1 } });
    });

    await waitFor(() => {
      expect(screen.getByText('Item deleted')).toBeInTheDocument();
    });
  });

  it('shows no-items empty state alert when list is empty', async () => {
    setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('No items found.')).toBeInTheDocument();
    });
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('renders em-dash for items without a description', async () => {
    setupSubscribeMock();
    const itemWithoutDescription = [
      {
        id: 1,
        name: 'Widget',
        price: 9.99,
        created_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-01T00:00:00Z',
      },
    ];
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue(itemWithoutDescription);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });
    expect(screen.getByText('—')).toBeInTheDocument();
  });

  it('re-fetches items when item.created event arrives', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue(mockItems);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    expect(itemService.list).toHaveBeenCalledTimes(1);

    act(() => {
      handlers['item.created']({
        type: 'item.created',
        payload: { id: 3, name: 'New', price: 1.0, created_at: '', updated_at: '' },
      });
    });

    await waitFor(() => {
      expect(itemService.list).toHaveBeenCalledTimes(2);
    });
  });

  it('re-fetches items when item.updated event arrives', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue(mockItems);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    expect(itemService.list).toHaveBeenCalledTimes(1);

    act(() => {
      handlers['item.updated']({
        type: 'item.updated',
        payload: { id: 1, name: 'Widget Pro', price: 29.99, created_at: '', updated_at: '' },
      });
    });

    await waitFor(() => {
      expect(itemService.list).toHaveBeenCalledTimes(2);
    });
  });

  it('re-fetches items when item.deleted event arrives', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue(mockItems);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    expect(itemService.list).toHaveBeenCalledTimes(1);

    act(() => {
      handlers['item.deleted']({ type: 'item.deleted', payload: { id: 1 } });
    });

    await waitFor(() => {
      expect(itemService.list).toHaveBeenCalledTimes(2);
    });
  });

  it('unsubscribes on unmount', async () => {
    const { mockUnsubscribe } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { unmount } = render(<Items />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Items');
    });

    unmount();

    expect(mockUnsubscribe).toHaveBeenCalledTimes(3);
  });

  // ---------------------------------------------------------------------------
  // Silent-fail: background refresh errors must NOT surface an error alert
  // ---------------------------------------------------------------------------

  it('keeps existing items and shows no error when background refresh fails on item.created', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce(mockItems)
      .mockRejectedValueOnce(new Error('Network error'));

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    act(() => {
      handlers['item.created']({
        type: 'item.created',
        payload: { id: 3, name: 'NewWidget', price: 5.0, created_at: '', updated_at: '' },
      });
    });

    await waitFor(() => {
      expect(itemService.list).toHaveBeenCalledTimes(2);
    });

    expect(screen.getByText('Widget')).toBeInTheDocument();
    expect(screen.getByText('Gadget')).toBeInTheDocument();
    expect(screen.queryByText('Failed to load items')).not.toBeInTheDocument();
  });

  it('keeps existing items and shows no error when background refresh fails on item.updated', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce(mockItems)
      .mockRejectedValueOnce(new Error('Network error'));

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    act(() => {
      handlers['item.updated']({
        type: 'item.updated',
        payload: { id: 1, name: 'Widget Pro', price: 29.99, created_at: '', updated_at: '' },
      });
    });

    await waitFor(() => {
      expect(itemService.list).toHaveBeenCalledTimes(2);
    });

    expect(screen.getByText('Widget')).toBeInTheDocument();
    expect(screen.getByText('Gadget')).toBeInTheDocument();
    expect(screen.queryByText('Failed to load items')).not.toBeInTheDocument();
  });

  it('keeps existing items and shows no error when background refresh fails on item.deleted', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce(mockItems)
      .mockRejectedValueOnce(new Error('Network error'));

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    act(() => {
      handlers['item.deleted']({ type: 'item.deleted', payload: { id: 1 } });
    });

    await waitFor(() => {
      expect(itemService.list).toHaveBeenCalledTimes(2);
    });

    expect(screen.getByText('Widget')).toBeInTheDocument();
    expect(screen.getByText('Gadget')).toBeInTheDocument();
    expect(screen.queryByText('Failed to load items')).not.toBeInTheDocument();
  });

  // ---------------------------------------------------------------------------
  // UI update: DOM must reflect the fresh data returned by the background fetch
  // ---------------------------------------------------------------------------

  it('renders new item in table after item.created triggers re-fetch', async () => {
    const { handlers } = setupSubscribeMock();
    const updatedItems = [
      ...mockItems,
      { id: 3, name: 'NewWidget', price: 5.0, created_at: '2026-01-03T00:00:00Z', updated_at: '2026-01-03T00:00:00Z' },
    ];
    (itemService.list as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce(mockItems)
      .mockResolvedValueOnce(updatedItems);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });
    expect(screen.queryByText('NewWidget')).not.toBeInTheDocument();

    act(() => {
      handlers['item.created']({
        type: 'item.created',
        payload: { id: 3, name: 'NewWidget', price: 5.0, created_at: '', updated_at: '' },
      });
    });

    await waitFor(() => {
      expect(screen.getByText('NewWidget')).toBeInTheDocument();
    });
    expect(screen.getByText('$5.00')).toBeInTheDocument();
  });

  it('renders updated item data in table after item.updated triggers re-fetch', async () => {
    const { handlers } = setupSubscribeMock();
    const updatedItems = [
      { ...mockItems[0], name: 'Widget Pro', price: 29.99 },
      mockItems[1],
    ];
    (itemService.list as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce(mockItems)
      .mockResolvedValueOnce(updatedItems);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    act(() => {
      handlers['item.updated']({
        type: 'item.updated',
        payload: { id: 1, name: 'Widget Pro', price: 29.99, created_at: '', updated_at: '' },
      });
    });

    await waitFor(() => {
      expect(screen.getByText('Widget Pro')).toBeInTheDocument();
    });
    expect(screen.queryByText('Widget')).not.toBeInTheDocument();
    expect(screen.getByText('$29.99')).toBeInTheDocument();
  });

  it('removes deleted item from table after item.deleted triggers re-fetch', async () => {
    const { handlers } = setupSubscribeMock();
    const updatedItems = [mockItems[1]]; // Widget (id:1) removed by server
    (itemService.list as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce(mockItems)
      .mockResolvedValueOnce(updatedItems);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    act(() => {
      handlers['item.deleted']({ type: 'item.deleted', payload: { id: 1 } });
    });

    await waitFor(() => {
      expect(screen.queryByText('Widget')).not.toBeInTheDocument();
    });
    expect(screen.getByText('Gadget')).toBeInTheDocument();
  });

  // ---------------------------------------------------------------------------
  // Rapid successive events: no race condition / state corruption
  // ---------------------------------------------------------------------------

  it('clears error state and shows items when background refresh succeeds after initial load failure', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>)
      .mockRejectedValueOnce(new Error('Network error'))
      .mockResolvedValueOnce(mockItems);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Failed to load items')).toBeInTheDocument();
    });

    act(() => {
      handlers['item.created']({
        type: 'item.created',
        payload: { id: 1, name: 'Widget', price: 9.99, created_at: '', updated_at: '' },
      });
    });

    await waitFor(() => {
      expect(screen.queryByText('Failed to load items')).not.toBeInTheDocument();
    });
    expect(screen.getByText('Widget')).toBeInTheDocument();
    expect(screen.getByText('Gadget')).toBeInTheDocument();
  });

  it('handles rapid successive WebSocket events without state corruption', async () => {
    const { handlers } = setupSubscribeMock();
    (itemService.list as ReturnType<typeof vi.fn>).mockResolvedValue(mockItems);

    render(<Items />);

    await waitFor(() => {
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    act(() => {
      handlers['item.created']({
        type: 'item.created',
        payload: { id: 3, name: 'ItemA', price: 1.0, created_at: '', updated_at: '' },
      });
      handlers['item.updated']({
        type: 'item.updated',
        payload: { id: 1, name: 'Widget', price: 9.99, created_at: '', updated_at: '' },
      });
      handlers['item.deleted']({ type: 'item.deleted', payload: { id: 2 } });
    });

    await waitFor(() => {
      expect(itemService.list).toHaveBeenCalledTimes(4); // 1 initial + 3 events
    });

    // Component must not crash; latest mocked data (mockItems) should be shown
    expect(screen.getByText('Widget')).toBeInTheDocument();
    expect(screen.getByText('Gadget')).toBeInTheDocument();
  });
});

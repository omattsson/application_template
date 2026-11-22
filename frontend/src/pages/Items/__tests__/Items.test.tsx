import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, act, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation, useNavigate, useNavigationType } from 'react-router-dom';
import { AxiosError, AxiosHeaders } from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import Items from '../index';
import { itemService } from '../../../api/client';
import type { Item, ItemListParams, ItemListResult } from '../../../api/client';
import { useWebSocketContext } from '../../../context/WebSocketContext';
import { NotificationProvider } from '../../../context/NotificationContext';
import type { WebSocketMessage } from '../../../hooks/useWebSocket';

vi.mock('../../../context/WebSocketContext', () => ({
  useWebSocketContext: vi.fn(),
}));

vi.mock('../../../api/client', () => ({
  itemService: {
    list: vi.fn(),
    delete: vi.fn(),
  },
}));

const mockItems: Item[] = [
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

const listMock = itemService.list as ReturnType<typeof vi.fn>;
const deleteMock = itemService.delete as ReturnType<typeof vi.fn>;

/** Items named "Item 01" ... "Item NN", ids 1 ... NN. */
function makeItems(count: number): Item[] {
  return Array.from({ length: count }, (_, index) => {
    const id = index + 1;
    return {
      id,
      name: `Item ${String(id).padStart(2, '0')}`,
      price: id,
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    };
  });
}

/**
 * A list backend that honours limit, offset and the name filter, like the API.
 * `all` is read on every call, so a test can change the data between calls.
 */
function fakeListBackend(all: () => Item[]) {
  return vi.fn(async (params: ItemListParams = {}): Promise<ItemListResult> => {
    const matching = all().filter((item) => !params.name || item.name.includes(params.name));
    const offset = params.offset ?? 0;
    const limit = params.limit ?? matching.length;
    return { items: matching.slice(offset, offset + limit), total: matching.length };
  });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

function setupSubscribeMock() {
  const handlers: Record<string, (msg: WebSocketMessage) => void> = {};
  const mockUnsubscribe = vi.fn();
  const mockSubscribe = vi
    .fn()
    .mockImplementation((type: string, handler: (msg: WebSocketMessage) => void) => {
      handlers[type] = handler;
      return mockUnsubscribe;
    });
  const setReconnectCount = (reconnectCount: number) =>
    (useWebSocketContext as ReturnType<typeof vi.fn>).mockReturnValue({
      subscribe: mockSubscribe,
      reconnectCount,
    });
  setReconnectCount(0);
  return { handlers, mockUnsubscribe, mockSubscribe, setReconnectCount };
}

/** Shows the URL and the last navigation type, and has a Back button. */
function Probe() {
  const location = useLocation();
  const navigationType = useNavigationType();
  const navigate = useNavigate();
  return (
    <>
      <div data-testid="location">{`${location.pathname}${location.search}`}</div>
      <div data-testid="navigation-type">{navigationType}</div>
      <button type="button" onClick={() => navigate(-1)}>
        Go back in history
      </button>
    </>
  );
}

function pageTree(entry = '/items') {
  return (
    <NotificationProvider>
      <MemoryRouter initialEntries={[entry]}>
        <Routes>
          <Route
            path="/items"
            element={
              <>
                <Items />
                <Probe />
              </>
            }
          />
        </Routes>
      </MemoryRouter>
    </NotificationProvider>
  );
}

function renderPage(entry = '/items') {
  return render(pageTree(entry));
}

/** The query of the URL shown by the probe. */
function urlParams(): URLSearchParams {
  return new URLSearchParams(screen.getByTestId('location').textContent?.split('?')[1] ?? '');
}

async function renderLoaded(entry = '/items') {
  const utils = renderPage(entry);
  await waitFor(() => {
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Items');
  });
  await waitFor(() => {
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
  });
  return utils;
}

describe('Items Page', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  describe('states and live updates', () => {
    it('shows a loading spinner initially', () => {
      setupSubscribeMock();
      listMock.mockReturnValue(new Promise(() => {}));

      renderPage();

      expect(screen.getByRole('progressbar')).toBeInTheDocument();
    });

    it('renders the rows and a pager with the total when data loads', async () => {
      setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });

      renderPage();

      await waitFor(() => {
        expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Items');
      });
      expect(screen.getByText('Widget')).toBeInTheDocument();
      expect(screen.getByText('$9.99')).toBeInTheDocument();
      expect(screen.getByText('Gadget')).toBeInTheDocument();
      expect(screen.getByText('$19.99')).toBeInTheDocument();
      expect(screen.getByText('A test widget')).toBeInTheDocument();
      expect(screen.getByText(/1.2 of 2/)).toBeInTheDocument();
    });

    it('shows error alert when fetch fails', async () => {
      setupSubscribeMock();
      listMock.mockRejectedValue(new Error('Network error'));

      renderPage();

      await waitFor(() => {
        expect(screen.getByRole('alert')).toBeInTheDocument();
      });
      expect(screen.getByText('Failed to load items')).toBeInTheDocument();
    });

    it('loads the page again when the socket has reconnected, but not on the first open', async () => {
      const { setReconnectCount } = setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 1 });

      const { rerender } = renderPage();
      await screen.findByRole('table');
      expect(listMock).toHaveBeenCalledTimes(1);

      // A render with an unchanged count (the socket opened for the first time) loads nothing.
      rerender(pageTree());
      expect(listMock).toHaveBeenCalledTimes(1);

      setReconnectCount(1);
      rerender(pageTree());
      await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));

      // The reload is silent: the table stays and no error shows.
      expect(screen.getByRole('table')).toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();

      setReconnectCount(2);
      rerender(pageTree());
      await waitFor(() => expect(listMock).toHaveBeenCalledTimes(3));
    });

    it('shows item.created toast with correct item name', async () => {
      const { handlers } = setupSubscribeMock();
      listMock.mockResolvedValue({ items: [], total: 0 });

      renderPage();

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
      listMock.mockResolvedValue({ items: [], total: 0 });

      renderPage();

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
      listMock.mockResolvedValue({ items: [], total: 0 });

      renderPage();

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

    it('shows the no-items empty state when the list is empty and there is no filter', async () => {
      setupSubscribeMock();
      listMock.mockResolvedValue({ items: [], total: 0 });

      renderPage();

      await waitFor(() => {
        expect(screen.getByRole('heading', { name: 'No items found' })).toBeInTheDocument();
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
      listMock.mockResolvedValue({ items: itemWithoutDescription, total: 1 });

      renderPage();

      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });
      expect(screen.getByText('—')).toBeInTheDocument();
    });

    it('re-fetches items when item.created event arrives', async () => {
      const { handlers } = setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });

      renderPage();

      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });

      expect(listMock).toHaveBeenCalledTimes(1);

      vi.useFakeTimers();

      act(() => {
        handlers['item.created']({
          type: 'item.created',
          payload: { id: 3, name: 'New', price: 1.0, created_at: '', updated_at: '' },
        });
      });

      await act(async () => {
        vi.runAllTimers();
      });

      expect(listMock).toHaveBeenCalledTimes(2);
    });

    it('re-fetches items when item.updated event arrives', async () => {
      const { handlers } = setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });

      renderPage();

      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });

      expect(listMock).toHaveBeenCalledTimes(1);

      vi.useFakeTimers();

      act(() => {
        handlers['item.updated']({
          type: 'item.updated',
          payload: { id: 1, name: 'Widget Pro', price: 29.99, created_at: '', updated_at: '' },
        });
      });

      await act(async () => {
        vi.runAllTimers();
      });

      expect(listMock).toHaveBeenCalledTimes(2);
    });

    it('re-fetches items when item.deleted event arrives', async () => {
      const { handlers } = setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });

      renderPage();

      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });

      expect(listMock).toHaveBeenCalledTimes(1);

      vi.useFakeTimers();

      act(() => {
        handlers['item.deleted']({ type: 'item.deleted', payload: { id: 1 } });
      });

      await act(async () => {
        vi.runAllTimers();
      });

      expect(listMock).toHaveBeenCalledTimes(2);
    });

    it('unsubscribes on unmount', async () => {
      const { mockUnsubscribe } = setupSubscribeMock();
      listMock.mockResolvedValue({ items: [], total: 0 });

      const { unmount } = renderPage();

      await waitFor(() => {
        expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Items');
      });

      unmount();

      expect(mockUnsubscribe).toHaveBeenCalledTimes(3);
    });

    it('shows the error of a page change that follows a background refresh', async () => {
      const { handlers } = setupSubscribeMock();
      listMock
        .mockResolvedValueOnce({ items: mockItems, total: 2 })
        .mockResolvedValueOnce({ items: [mockItems[1]], total: 1 })
        .mockRejectedValueOnce(new Error('Network error'));
      const user = userEvent.setup();

      renderPage();
      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });

      // A delete refreshes the list in the background (silent).
      act(() => {
        handlers['item.deleted']({ type: 'item.deleted', payload: { id: 1 } });
      });
      await waitFor(() => {
        expect(listMock).toHaveBeenCalledTimes(2);
      });
      expect(screen.queryByText('Failed to load items')).not.toBeInTheDocument();

      // A sort change is the user's request: its failure must be shown.
      await user.click(screen.getByRole('button', { name: 'Name' }));

      expect(await screen.findByText('Failed to load items')).toBeInTheDocument();
    });

    it('keeps existing items and shows no error when background refresh fails on item.created', async () => {
      const { handlers } = setupSubscribeMock();
      listMock
        .mockResolvedValueOnce({ items: mockItems, total: 2 })
        .mockRejectedValueOnce(new Error('Network error'));

      renderPage();

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
        expect(listMock).toHaveBeenCalledTimes(2);
      });

      expect(screen.getByText('Widget')).toBeInTheDocument();
      expect(screen.getByText('Gadget')).toBeInTheDocument();
      expect(screen.queryByText('Failed to load items')).not.toBeInTheDocument();
    });

    it('renders new item in table after item.created triggers re-fetch', async () => {
      const { handlers } = setupSubscribeMock();
      const updatedItems = [
        ...mockItems,
        { id: 3, name: 'NewWidget', price: 5.0, created_at: '2026-01-03T00:00:00Z', updated_at: '2026-01-03T00:00:00Z' },
      ];
      listMock
        .mockResolvedValueOnce({ items: mockItems, total: 2 })
        .mockResolvedValueOnce({ items: updatedItems, total: 3 });

      renderPage();

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

    it('removes deleted item from table after item.deleted triggers re-fetch', async () => {
      const { handlers } = setupSubscribeMock();
      listMock
        .mockResolvedValueOnce({ items: mockItems, total: 2 })
        .mockResolvedValueOnce({ items: [mockItems[1]], total: 1 });

      renderPage();

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

    it('re-fetches with the current page, sort and filter when a WebSocket event arrives', async () => {
      const { handlers } = setupSubscribeMock();
      const all = makeItems(30);
      listMock.mockImplementation(fakeListBackend(() => all));

      renderPage('/items?page=2&size=10&sort=name&order=desc&q=Item');
      await waitFor(() => {
        expect(screen.getByText('Item 20')).toBeInTheDocument();
      });

      act(() => {
        handlers['item.updated']({
          type: 'item.updated',
          payload: { id: 1, name: 'Item 01', price: 1, created_at: '', updated_at: '' },
        });
      });

      await waitFor(() => {
        expect(listMock).toHaveBeenLastCalledWith({ limit: 10, offset: 10, sort: 'name', order: 'desc', name: 'Item' });
      });
    });

    it('moves to the last page when a refresh finds the current page past the end', async () => {
      const { handlers } = setupSubscribeMock();
      const all = makeItems(21);
      listMock.mockImplementation(fakeListBackend(() => all));

      renderPage('/items?page=3&size=10');
      await waitFor(() => {
        expect(screen.getByText('Item 21')).toBeInTheDocument();
      });

      // The last row of the last page is deleted on the server.
      all.splice(20, 1);
      act(() => {
        handlers['item.deleted']({ type: 'item.deleted', payload: { id: 21 } });
      });

      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('/items?page=2&size=10');
      });
      await waitFor(() => {
        expect(listMock).toHaveBeenLastCalledWith({ limit: 10, offset: 10, sort: 'id', order: 'asc', name: undefined });
      });
      expect(screen.getByText('Item 20')).toBeInTheDocument();
    });

    it('drops a stale response and shows the newest one', async () => {
      setupSubscribeMock();
      const older = deferred<ItemListResult>();
      const newer = deferred<ItemListResult>();
      listMock
        .mockResolvedValueOnce({ items: mockItems, total: 2 })
        .mockReturnValueOnce(older.promise)
        .mockReturnValueOnce(newer.promise);
      const user = userEvent.setup();

      renderPage();
      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });

      const nameHeader = screen.getByRole('button', { name: 'Name' });
      await user.click(nameHeader); // request 2, still pending
      await user.click(nameHeader); // request 3, still pending

      await act(async () => {
        newer.resolve({ items: [{ ...mockItems[0], name: 'Newest' }], total: 1 });
      });
      await act(async () => {
        older.resolve({ items: [{ ...mockItems[0], name: 'Stale' }], total: 1 });
      });

      expect(screen.getByText('Newest')).toBeInTheDocument();
      expect(screen.queryByText('Stale')).not.toBeInTheDocument();
    });

    it('does not re-subscribe to WebSocket messages when the filter changes', async () => {
      const { mockSubscribe } = setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });
      const user = userEvent.setup();

      renderPage();
      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });
      const subscriptions = mockSubscribe.mock.calls.length;

      await user.type(screen.getByRole('searchbox', { name: 'Filter by name' }), 'wid');

      expect(mockSubscribe.mock.calls.length).toBe(subscriptions);
    });
  });

  describe('request and URL state', () => {
    it('requests the first page with the defaults and writes no defaults to the URL', async () => {
      setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });

      renderPage();

      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });
      expect(listMock).toHaveBeenCalledWith({ limit: 25, offset: 0, sort: 'id', order: 'asc', name: undefined });
      expect(screen.getByTestId('location')).toHaveTextContent(/^\/items$/);
    });

    it('restores the page, size, sort, order and filter from the URL', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(30)));

      renderPage('/items?page=2&size=10&sort=name&order=desc&q=Item');

      await waitFor(() => {
        expect(screen.getByText('Item 20')).toBeInTheDocument();
      });
      expect(listMock).toHaveBeenCalledWith({ limit: 10, offset: 10, sort: 'name', order: 'desc', name: 'Item' });
      expect(screen.getByRole('searchbox', { name: 'Filter by name' })).toHaveValue('Item');
      expect(screen.getByRole('columnheader', { name: /Name/ })).toHaveAttribute('aria-sort', 'descending');
    });

    it('falls back to the defaults for invalid values', async () => {
      setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });

      renderPage('/items?page=abc&size=7&sort=description&order=sideways');

      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });
      expect(listMock).toHaveBeenCalledWith({ limit: 25, offset: 0, sort: 'id', order: 'asc', name: undefined });
    });

    it('treats a page below 1 as page 1', async () => {
      setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });

      renderPage('/items?page=0');

      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });
      expect(listMock).toHaveBeenCalledWith(expect.objectContaining({ offset: 0 }));
    });

    it('moves to the last page when the URL page is past the end', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(12)));

      renderPage('/items?page=9&size=10');

      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('/items?page=2&size=10');
      });
      await waitFor(() => {
        expect(screen.getByText('Item 12')).toBeInTheDocument();
      });
    });
  });

  describe('sorting', () => {
    it('makes ID, Name, Price, Created At and Updated At sort buttons, and not Description or Actions', async () => {
      setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });

      await renderLoaded();

      for (const name of ['ID', 'Name', 'Price', 'Created At', 'Updated At']) {
        expect(screen.getByRole('button', { name })).toBeInTheDocument();
      }
      expect(screen.queryByRole('button', { name: 'Description' })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Actions' })).not.toBeInTheDocument();
    });

    it('marks ID as the ascending default sort', async () => {
      setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });

      await renderLoaded();

      expect(screen.getByRole('columnheader', { name: /ID/ })).toHaveAttribute('aria-sort', 'ascending');
      expect(screen.getByRole('columnheader', { name: /Name/ })).not.toHaveAttribute('aria-sort');
    });

    it('starts a new column ascending, resets the page and pushes a history entry', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(30)));
      const user = userEvent.setup();

      await renderLoaded('/items?page=2&size=10');
      await user.click(screen.getByRole('button', { name: 'Name' }));

      await waitFor(() => {
        expect(listMock).toHaveBeenLastCalledWith({ limit: 10, offset: 0, sort: 'name', order: 'asc', name: undefined });
      });
      expect(urlParams().get('page')).toBeNull();
      expect(urlParams().get('size')).toBe('10');
      expect(urlParams().get('sort')).toBe('name');
      expect(screen.getByTestId('navigation-type')).toHaveTextContent('PUSH');
    });

    it('toggles the direction when the sorted column is clicked again', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(3)));
      const user = userEvent.setup();

      await renderLoaded('/items?sort=name');
      expect(screen.getByRole('columnheader', { name: /Name/ })).toHaveAttribute('aria-sort', 'ascending');

      await user.click(screen.getByRole('button', { name: 'Name' }));
      await waitFor(() => {
        expect(screen.getByRole('columnheader', { name: /Name/ })).toHaveAttribute('aria-sort', 'descending');
      });
      expect(urlParams().get('order')).toBe('desc');

      await user.click(screen.getByRole('button', { name: 'Name' }));
      await waitFor(() => {
        expect(screen.getByRole('columnheader', { name: /Name/ })).toHaveAttribute('aria-sort', 'ascending');
      });
      expect(urlParams().get('order')).toBeNull();
    });

    it('sorts the rows by the price column', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(3)));
      const user = userEvent.setup();

      await renderLoaded();
      await user.click(screen.getByRole('button', { name: 'Price' }));

      await waitFor(() => {
        expect(listMock).toHaveBeenLastCalledWith({ limit: 25, offset: 0, sort: 'price', order: 'asc', name: undefined });
      });
    });
  });

  describe('name filter', () => {
    it('sends the name after the 300 ms debounce, not for each keystroke', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(3)));
      const user = userEvent.setup();

      await renderLoaded();
      await user.type(screen.getByRole('searchbox', { name: 'Filter by name' }), 'Item 02');

      await waitFor(() => {
        expect(listMock).toHaveBeenLastCalledWith(expect.objectContaining({ name: 'Item 02' }));
      });
      const names = listMock.mock.calls.map((call) => (call[0] as ItemListParams | undefined)?.name);
      expect(names).not.toContain('I');
      expect(names).not.toContain('Item 0');
      expect(screen.getByTestId('location')).toHaveTextContent('?q=Item+02');
    });

    it('replaces the history entry when the filter is committed', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(3)));
      const user = userEvent.setup();

      await renderLoaded();
      await user.type(screen.getByRole('searchbox', { name: 'Filter by name' }), 'Item');

      await waitFor(() => {
        expect(screen.getByTestId('navigation-type')).toHaveTextContent('REPLACE');
      });
    });

    it('resets the page to 1 when the filter changes', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(30)));
      const user = userEvent.setup();

      await renderLoaded('/items?page=3&size=10');
      await user.type(screen.getByRole('searchbox', { name: 'Filter by name' }), 'Item 1');

      await waitFor(() => {
        expect(listMock).toHaveBeenLastCalledWith({ limit: 10, offset: 0, sort: 'id', order: 'asc', name: 'Item 1' });
      });
      expect(urlParams().get('page')).toBeNull();
    });

    it('shows the filter value from the URL again after going back in history', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(3)));
      const user = userEvent.setup();

      await renderLoaded();
      await user.click(screen.getByRole('button', { name: 'Name' })); // push: ?sort=name
      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('?sort=name');
      });
      await user.type(screen.getByRole('searchbox', { name: 'Filter by name' }), 'Item');
      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('?sort=name&q=Item');
      });

      await user.click(screen.getByRole('button', { name: 'Go back in history' }));

      await waitFor(() => {
        expect(screen.getByRole('searchbox', { name: 'Filter by name' })).toHaveValue('');
      });
      expect(screen.getByTestId('location')).toHaveTextContent(/^\/items$/);
    });

    it('shows a filter empty state that is different from "no items yet" and keeps the filter field', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(3)));
      const user = userEvent.setup();

      await renderLoaded();
      await user.type(screen.getByRole('searchbox', { name: 'Filter by name' }), 'zzz');

      await waitFor(() => {
        expect(screen.getByRole('heading', { name: 'No matching items' })).toBeInTheDocument();
      });
      expect(screen.queryByRole('heading', { name: 'No items found' })).not.toBeInTheDocument();
      expect(screen.getByRole('searchbox', { name: 'Filter by name' })).toHaveValue('zzz');
      expect(screen.queryByRole('table')).not.toBeInTheDocument();
    });
  });

  describe('paging', () => {
    it('moves to the next page with a history entry', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(30)));
      const user = userEvent.setup();

      await renderLoaded();
      await user.click(screen.getByRole('button', { name: 'Go to next page' }));

      await waitFor(() => {
        expect(listMock).toHaveBeenLastCalledWith({ limit: 25, offset: 25, sort: 'id', order: 'asc', name: undefined });
      });
      expect(urlParams().get('page')).toBe('2');
      expect(screen.getByTestId('navigation-type')).toHaveTextContent('PUSH');
    });

    it('changes the page size, resets the page to 1 and writes the size to the URL', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(60)));
      const user = userEvent.setup();

      await renderLoaded('/items?page=2');
      await user.click(screen.getByRole('combobox', { name: /rows per page/i }));
      await user.click(screen.getByRole('option', { name: '50' }));

      await waitFor(() => {
        expect(listMock).toHaveBeenLastCalledWith({ limit: 50, offset: 0, sort: 'id', order: 'asc', name: undefined });
      });
      expect(urlParams().get('page')).toBeNull();
      expect(urlParams().get('size')).toBe('50');
    });
  });

  describe('loading and errors after the first load', () => {
    it('does not show the loading state or remount the filter field while the list refetches', async () => {
      setupSubscribeMock();
      listMock.mockImplementation(fakeListBackend(() => makeItems(30)));
      const user = userEvent.setup();

      await renderLoaded();
      const filter = screen.getByRole('searchbox', { name: 'Filter by name' });
      await user.click(screen.getByRole('button', { name: 'Name' }));

      await waitFor(() => {
        expect(listMock).toHaveBeenCalledTimes(2);
      });
      expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
      expect(screen.getByRole('searchbox', { name: 'Filter by name' })).toBe(filter);
    });

    it('shows an error for a failed page change and keeps the controls', async () => {
      setupSubscribeMock();
      listMock
        .mockResolvedValueOnce({ items: mockItems, total: 2 })
        .mockRejectedValueOnce(new Error('Network error'));
      const user = userEvent.setup();

      await renderLoaded();
      await user.click(screen.getByRole('button', { name: 'Name' }));

      expect(await screen.findByText('Failed to load items')).toBeInTheDocument();
      expect(screen.getByRole('searchbox', { name: 'Filter by name' })).toBeInTheDocument();
    });
  });

  // ---------------------------------------------------------------------------
  // Delete asks for confirmation first
  // ---------------------------------------------------------------------------

  describe('delete', () => {
    async function renderWithRows() {
      const utils = setupSubscribeMock();
      listMock.mockResolvedValue({ items: mockItems, total: 2 });
      const user = userEvent.setup();
      renderPage();
      await waitFor(() => {
        expect(screen.getByText('Widget')).toBeInTheDocument();
      });
      return { user, ...utils };
    }

    it('renders an accessible delete button per item', async () => {
      await renderWithRows();
      expect(screen.getByRole('button', { name: 'Delete Widget' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Delete Gadget' })).toBeInTheDocument();
    });

    it('asks for confirmation and does not delete yet', async () => {
      const { user } = await renderWithRows();

      await user.click(screen.getByRole('button', { name: 'Delete Widget' }));

      const dialog = await screen.findByRole('dialog', { name: 'Delete item' });
      expect(within(dialog).getByText(/Widget/)).toBeInTheDocument();
      expect(deleteMock).not.toHaveBeenCalled();
    });

    it('does not delete when the dialog is cancelled', async () => {
      const { user } = await renderWithRows();

      await user.click(screen.getByRole('button', { name: 'Delete Widget' }));
      const dialog = await screen.findByRole('dialog');
      await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));

      await waitFor(() => {
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      });
      expect(deleteMock).not.toHaveBeenCalled();
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });

    it('deletes the item, refreshes the list and confirms with a toast when confirmed', async () => {
      const { user } = await renderWithRows();
      deleteMock.mockResolvedValue(undefined);
      listMock.mockResolvedValue({ items: [mockItems[1]], total: 1 });

      await user.click(screen.getByRole('button', { name: 'Delete Widget' }));
      const dialog = await screen.findByRole('dialog');
      await user.click(within(dialog).getByRole('button', { name: 'Delete' }));

      await waitFor(() => {
        expect(deleteMock).toHaveBeenCalledWith(1);
      });
      await waitFor(() => {
        expect(screen.queryByText('Widget')).not.toBeInTheDocument();
      });
      expect(screen.getByText('Gadget')).toBeInTheDocument();
      expect(screen.getByText('Item deleted')).toBeInTheDocument();
      await waitFor(() => {
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      });
    });

    it('shows the server error and keeps the item when the delete fails', async () => {
      const { user } = await renderWithRows();
      const config = { headers: new AxiosHeaders() } as InternalAxiosRequestConfig;
      const error = new AxiosError('Request failed', 'ERR_BAD_REQUEST', config, undefined, {
        status: 404,
        statusText: '',
        data: { error: 'item not found' },
        headers: {},
        config,
      });
      deleteMock.mockRejectedValue(error);

      await user.click(screen.getByRole('button', { name: 'Delete Widget' }));
      const dialog = await screen.findByRole('dialog');
      await user.click(within(dialog).getByRole('button', { name: 'Delete' }));

      expect(await screen.findByText('item not found')).toBeInTheDocument();
      expect(screen.getByText('Widget')).toBeInTheDocument();
    });
  });
});

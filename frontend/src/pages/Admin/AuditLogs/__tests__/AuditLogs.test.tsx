import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, within, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AxiosError, AxiosHeaders } from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import AuditLogsPage from '../index';
import { auditLogService } from '../../../../api/client';
import type { AuditLog, AuditLogListResult } from '../../../../api/client';

vi.mock('../../../../api/client', () => ({
  auditLogService: {
    list: vi.fn(),
  },
}));

const CREATE_ITEM: AuditLog = {
  id: '55555555-5555-4555-8555-555555555555',
  timestamp: '2026-11-21T10:00:00Z',
  user_id: '99999999-9999-4999-8999-999999999999',
  username: 'admin',
  action: 'create',
  entity_type: 'item',
  entity_id: '42',
  details: '{"method":"POST","route":"/api/v1/items","status":201,"client_ip":"10.0.0.7"}',
};

const ROLE_CHANGE: AuditLog = {
  id: '66666666-6666-4666-8666-666666666666',
  timestamp: '2026-11-20T09:30:00Z',
  user_id: '77777777-7777-4777-8777-777777777777',
  username: 'alice',
  action: 'change_role',
  entity_type: 'user',
  entity_id: '77777777-7777-4777-8777-777777777777',
  details: 'not json <img src=x onerror="alert(1)">',
};

function result(data: AuditLog[], total = data.length, limit = 25, offset = 0): AuditLogListResult {
  return { data, total, limit, offset };
}

/** An AxiosError as the client throws it for a non-2xx answer. */
function httpError(status: number, body: unknown): AxiosError {
  const config = { headers: new AxiosHeaders(), method: 'get', url: '/api/v1/audit-logs' } as InternalAxiosRequestConfig;
  return new AxiosError(`Request failed with status code ${status}`, AxiosError.ERR_BAD_REQUEST, config, {}, {
    data: body,
    status,
    statusText: String(status),
    headers: {},
    config,
  });
}

/** A promise that the test settles by hand. */
function controlled<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const mocked = {
  list: vi.mocked(auditLogService.list),
};

function renderPage() {
  return render(<AuditLogsPage />);
}

async function renderLoaded(entries: AuditLog[] = [CREATE_ITEM, ROLE_CHANGE], total?: number) {
  mocked.list.mockResolvedValue(result(entries, total));
  const view = renderPage();
  await screen.findByRole('table', { name: 'Audit log entries' });
  return view;
}

function rowOf(entityId: string): HTMLElement {
  return screen.getByRole('row', { name: new RegExp(`${entityId}\\b`) });
}

/** The form of the filters. */
function filterForm(): HTMLElement {
  return screen.getByRole('search', { name: 'Filter audit entries' });
}

/** Pick an option of a select by its label (MUI renders the select as a combobox). */
async function pick(user: ReturnType<typeof userEvent.setup>, label: string, option: string) {
  await user.click(within(filterForm()).getByRole('combobox', { name: label }));
  await user.click(screen.getByRole('option', { name: option }));
}

/** Apply the filters with the Apply button. */
async function apply(user: ReturnType<typeof userEvent.setup>) {
  await user.click(within(filterForm()).getByRole('button', { name: 'Apply' }));
}

describe('Audit log page', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  describe('list', () => {
    it('shows a loading state with the title while the first page loads', async () => {
      const pending = controlled<AuditLogListResult>();
      mocked.list.mockReturnValue(pending.promise);
      renderPage();

      expect(screen.getByRole('heading', { name: 'Audit log', level: 1 })).toBeInTheDocument();
      expect(screen.getByRole('progressbar', { name: 'Loading audit log' })).toBeInTheDocument();
      expect(screen.queryByRole('table', { name: 'Audit log entries' })).not.toBeInTheDocument();

      pending.resolve(result([CREATE_ITEM]));
      await screen.findByRole('table', { name: 'Audit log entries' });
      expect(screen.queryByRole('progressbar', { name: 'Loading audit log' })).not.toBeInTheDocument();
    });

    it('asks for the first page with 25 rows and no filter', async () => {
      await renderLoaded();

      expect(mocked.list).toHaveBeenCalledTimes(1);
      expect(mocked.list).toHaveBeenCalledWith({ limit: 25, offset: 0 });
    });

    it('shows the entries in the order of the answer, with the time, user, action, entity and details columns', async () => {
      await renderLoaded();

      const headers = screen.getAllByRole('columnheader').map((th) => th.textContent);
      expect(headers).toEqual(['Time', 'User', 'Action', 'Entity type', 'Entity ID', 'Details']);

      const rows = screen.getAllByRole('row').slice(1);
      expect(rows[0]).toHaveTextContent('create');
      expect(rows[1]).toHaveTextContent('change_role');

      const create = rowOf('42');
      expect(within(create).getByText(new Date(CREATE_ITEM.timestamp).toLocaleString())).toBeInTheDocument();
      expect(within(create).getByText('admin')).toBeInTheDocument();
      expect(within(create).getByText(CREATE_ITEM.user_id)).toBeInTheDocument();
      expect(within(create).getByText('item')).toBeInTheDocument();
    });

    it('shows the total and the page range from the server', async () => {
      await renderLoaded([CREATE_ITEM, ROLE_CHANGE], 30);
      expect(screen.getByText('1–25 of 30')).toBeInTheDocument();
    });

    it('shows the server message when the list fails, keeps the filters, and Retry loads it again', async () => {
      const user = userEvent.setup();
      mocked.list.mockRejectedValueOnce(httpError(400, { error: 'Invalid user_id parameter' }));
      renderPage();

      expect(await screen.findByRole('alert')).toHaveTextContent('Invalid user_id parameter');
      expect(within(filterForm()).getByRole('button', { name: 'Apply' })).toBeEnabled();

      mocked.list.mockResolvedValueOnce(result([CREATE_ITEM]));
      await user.click(screen.getByRole('button', { name: 'Retry' }));

      await screen.findByRole('table', { name: 'Audit log entries' });
      expect(mocked.list).toHaveBeenCalledTimes(2);
    });

    it('shows a 403 from the server as an error', async () => {
      mocked.list.mockRejectedValueOnce(httpError(403, { error: 'The audit log is for admins' }));
      renderPage();

      expect(await screen.findByRole('alert')).toHaveTextContent('The audit log is for admins');
      expect(screen.queryByRole('table', { name: 'Audit log entries' })).not.toBeInTheDocument();
    });

    it('shows a generic message for a 500, never the server body', async () => {
      mocked.list.mockRejectedValueOnce(httpError(500, { error: 'SQL: secret table' }));
      renderPage();

      const alert = await screen.findByRole('alert');
      expect(alert).toHaveTextContent('Something went wrong on the server. Please try again.');
      expect(alert).not.toHaveTextContent('SQL');
    });

    it('shows the empty state without filters', async () => {
      mocked.list.mockResolvedValue(result([], 0));
      renderPage();

      expect(await screen.findByText('No audit entries')).toBeInTheDocument();
      expect(screen.queryByRole('table', { name: 'Audit log entries' })).not.toBeInTheDocument();
      expect(screen.queryByText('No entries match the filters')).not.toBeInTheDocument();
    });

    it('shows the empty state for the filters when filters are set and nothing matches', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      mocked.list.mockResolvedValueOnce(result([], 0));
      await pick(user, 'Action', 'delete');
      await apply(user);

      expect(await screen.findByText('No entries match the filters')).toBeInTheDocument();
      expect(screen.queryByText('No audit entries')).not.toBeInTheDocument();
    });
  });

  describe('details', () => {
    it('hides the details behind a Details button with aria-expanded, and shows the JSON as text', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      const toggle = within(rowOf('42')).getByRole('button', { name: 'Details' });
      expect(toggle).toHaveAttribute('aria-expanded', 'false');
      expect(screen.queryByText(/"client_ip": "10.0.0.7"/)).not.toBeInTheDocument();

      await user.click(toggle);
      expect(toggle).toHaveAttribute('aria-expanded', 'true');
      expect(screen.getByText(/"client_ip": "10.0.0.7"/)).toBeInTheDocument();

      await user.click(toggle);
      expect(toggle).toHaveAttribute('aria-expanded', 'false');
      expect(screen.queryByText(/"client_ip": "10.0.0.7"/)).not.toBeInTheDocument();
    });

    it('shows details that are not JSON as the raw text, never as HTML', async () => {
      const user = userEvent.setup();
      const { container } = await renderLoaded();

      await user.click(within(rowOf('77777777-7777-4777-8777-777777777777')).getByRole('button', { name: 'Details' }));

      expect(screen.getByText(/not json <img src=x onerror="alert\(1\)">/)).toBeInTheDocument();
      expect(container.querySelector('img')).toBeNull();
    });

    it('renders a username that holds markup as text', async () => {
      const hostile = { ...CREATE_ITEM, id: 'x1', username: '<b>bold</b>' };
      const { container } = await renderLoaded([hostile]);

      expect(screen.getByText('<b>bold</b>')).toBeInTheDocument();
      expect(container.querySelector('b')).toBeNull();
    });
  });

  describe('pagination', () => {
    it('asks for the next page with the offset of the page size', async () => {
      const user = userEvent.setup();
      mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 30));
      renderPage();
      await screen.findByRole('table', { name: 'Audit log entries' });

      mocked.list.mockResolvedValueOnce(result([ROLE_CHANGE], 30, 25, 25));
      await user.click(screen.getByRole('button', { name: 'Go to next page' }));

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 25 }));
      await screen.findByRole('row', { name: /change_role/ });
    });

    it('sends the chosen page size and goes back to the first page', async () => {
      const user = userEvent.setup();
      mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 30));
      renderPage();
      await screen.findByRole('table', { name: 'Audit log entries' });

      mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 30, 50, 0));
      await user.click(screen.getByRole('combobox', { name: /rows per page/i }));
      await user.click(screen.getByRole('option', { name: '50' }));

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 50, offset: 0 }));
    });

    it('keeps the filters and the pagination mounted while a page loads', async () => {
      const user = userEvent.setup();
      mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 30));
      renderPage();
      await screen.findByRole('table', { name: 'Audit log entries' });

      const pending = controlled<AuditLogListResult>();
      mocked.list.mockReturnValueOnce(pending.promise);
      await user.click(screen.getByRole('button', { name: 'Go to next page' }));

      expect(within(filterForm()).getByRole('button', { name: 'Apply' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Go to next page' })).toBeInTheDocument();
      expect(screen.getByRole('table', { name: 'Audit log entries' }).parentElement).toHaveAttribute('aria-busy', 'true');

      pending.resolve(result([ROLE_CHANGE], 30, 25, 25));
      await waitFor(() => expect(screen.getByRole('table', { name: 'Audit log entries' }).parentElement).toHaveAttribute('aria-busy', 'false'));
    });
  });

  describe('filters', () => {
    it('filters by action', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      mocked.list.mockResolvedValueOnce(result([CREATE_ITEM]));
      await pick(user, 'Action', 'create');
      await apply(user);

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 0, action: 'create' }));
    });

    it('offers "Any" and every known action and entity type', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      await user.click(within(filterForm()).getByRole('combobox', { name: 'Action' }));
      const actionOptions = screen.getAllByRole('option').map((o) => o.textContent);
      expect(actionOptions).toEqual([
        'Any',
        'create',
        'update',
        'delete',
        'disable',
        'enable',
        'change_role',
        'change_password',
        'password_reset',
      ]);
      await user.keyboard('{Escape}');

      await user.click(within(filterForm()).getByRole('combobox', { name: 'Entity type' }));
      expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual(['Any', 'item', 'user', 'api_key']);
      await user.keyboard('{Escape}');
    });

    it('filters by entity type', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      await pick(user, 'Entity type', 'user');
      await apply(user);

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 0, entity_type: 'user' }));
    });

    it('filters by user ID, trimmed, and submits with Enter', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      await user.type(within(filterForm()).getByRole('textbox', { name: 'User ID' }), '  abc-123{Enter}');

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 0, user_id: 'abc-123' }));
    });

    it('filters by a date range, sent as RFC3339 in UTC', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      fireEvent.change(within(filterForm()).getByLabelText('Start'), { target: { value: '2026-11-01T08:00' } });
      fireEvent.change(within(filterForm()).getByLabelText('End'), { target: { value: '2026-11-30T23:59' } });
      await apply(user);

      await waitFor(() =>
        expect(mocked.list).toHaveBeenLastCalledWith({
          limit: 25,
          offset: 0,
          start_date: new Date('2026-11-01T08:00').toISOString(),
          end_date: new Date('2026-11-30T23:59').toISOString(),
        }),
      );
    });

    it('labels the date inputs as datetime-local fields', () => {
      mocked.list.mockReturnValue(new Promise(() => {}));
      renderPage();

      const start = within(filterForm()).getByLabelText('Start');
      expect(start).toHaveAttribute('type', 'datetime-local');
      expect(within(filterForm()).getByLabelText('End')).toHaveAttribute('type', 'datetime-local');
    });

    it('refuses a start after the end in the form, without a request', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      const calls = mocked.list.mock.calls.length;

      fireEvent.change(within(filterForm()).getByLabelText('Start'), { target: { value: '2026-11-30T00:00' } });
      fireEvent.change(within(filterForm()).getByLabelText('End'), { target: { value: '2026-11-01T00:00' } });
      await apply(user);

      expect(screen.getByText('The start must not be after the end.')).toBeInTheDocument();
      expect(within(filterForm()).getByLabelText('Start')).toHaveAttribute('aria-invalid', 'true');
      expect(within(filterForm()).getByLabelText('End')).toHaveAttribute('aria-invalid', 'true');
      expect(mocked.list).toHaveBeenCalledTimes(calls);
    });

    it('moves the focus to the Start field and announces the refused range once, for both fields', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      fireEvent.change(within(filterForm()).getByLabelText('Start'), { target: { value: '2026-11-30T00:00' } });
      fireEvent.change(within(filterForm()).getByLabelText('End'), { target: { value: '2026-11-01T00:00' } });
      await apply(user);

      const start = within(filterForm()).getByLabelText('Start');
      const end = within(filterForm()).getByLabelText('End');
      expect(start).toHaveFocus();

      const alert = screen.getByRole('alert');
      expect(alert).toHaveTextContent('The start must not be after the end.');
      expect(alert).toHaveAttribute('id');
      expect(start).toHaveAccessibleDescription('The start must not be after the end.');
      expect(end).toHaveAccessibleDescription('The start must not be after the end.');
      expect(start.getAttribute('aria-describedby')?.split(' ')).toContain(alert.id);
      expect(end.getAttribute('aria-describedby')?.split(' ')).toContain(alert.id);
    });

    it('clears the refused-range message on the next change of the range', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      fireEvent.change(within(filterForm()).getByLabelText('Start'), { target: { value: '2026-11-30T00:00' } });
      fireEvent.change(within(filterForm()).getByLabelText('End'), { target: { value: '2026-11-01T00:00' } });
      await apply(user);
      expect(screen.getByRole('alert')).toBeInTheDocument();

      fireEvent.change(within(filterForm()).getByLabelText('End'), { target: { value: '2026-12-01T00:00' } });

      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(within(filterForm()).getByLabelText('End')).toHaveAttribute('aria-invalid', 'false');
    });

    it('Apply goes back to the first page', async () => {
      const user = userEvent.setup();
      mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 60));
      renderPage();
      await screen.findByRole('table', { name: 'Audit log entries' });

      mocked.list.mockResolvedValueOnce(result([ROLE_CHANGE], 60, 25, 25));
      await user.click(screen.getByRole('button', { name: 'Go to next page' }));
      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 25 }));
      await screen.findByRole('row', { name: /change_role/ });

      mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 3));
      await pick(user, 'Action', 'create');
      await apply(user);

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 0, action: 'create' }));
    });

    it('Clear removes every filter, goes back to the first page and fetches again', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      await pick(user, 'Action', 'delete');
      await user.type(within(filterForm()).getByRole('textbox', { name: 'User ID' }), 'u-9');
      await apply(user);
      await waitFor(() =>
        expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 0, action: 'delete', user_id: 'u-9' }),
      );

      await user.click(within(filterForm()).getByRole('button', { name: 'Clear' }));

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 0 }));
      expect(within(filterForm()).getByRole('textbox', { name: 'User ID' })).toHaveValue('');
      expect(within(filterForm()).getByRole('combobox', { name: 'Action' })).toHaveTextContent('Any');
    });

    it('drops the response of an older request when a newer one has answered', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      const older = controlled<AuditLogListResult>();
      mocked.list.mockReturnValueOnce(older.promise);
      await pick(user, 'Action', 'update');
      await apply(user);

      mocked.list.mockResolvedValueOnce(result([ROLE_CHANGE]));
      await pick(user, 'Action', 'change_role');
      await apply(user);

      // The newer answer (no create row) is shown first; then the older one arrives.
      await waitFor(() => expect(screen.queryByRole('row', { name: /create/ })).not.toBeInTheDocument());
      older.resolve(result([CREATE_ITEM]));
      await new Promise((resolve) => setTimeout(resolve, 20));
      expect(screen.queryByRole('row', { name: /create/ })).not.toBeInTheDocument();
      expect(screen.getByRole('row', { name: /change_role/ })).toBeInTheDocument();
    });
  });
});

describe('Audit log page beyond the reachable offset', () => {
  // The server clamps an offset above 100000 to 100000 and answers 200 with that offset.
  const NOTE = 'Only the first 100,000 entries can be paged here. Narrow the filters to see the others.';

  afterEach(() => {
    vi.clearAllMocks();
  });

  it('stops the pager at the last reachable page (offset 100000) and keeps the real total in the label', async () => {
    const user = userEvent.setup();
    mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 200000));
    renderPage();
    await screen.findByRole('table', { name: 'Audit log entries' });
    expect(screen.getByText('1–25 of 200000')).toBeInTheDocument();

    mocked.list.mockResolvedValueOnce(result([ROLE_CHANGE], 200000, 25, 100000));
    await user.click(screen.getByRole('button', { name: 'Go to last page' }));

    await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 100000 }));
    await screen.findByRole('row', { name: /change_role/ });
    expect(screen.getByText('100001–100025 of 200000')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Go to next page' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Go to last page' })).toBeDisabled();
  });

  it('never requests an offset above 100000 with the next button', async () => {
    const user = userEvent.setup();
    mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 200000));
    renderPage();
    await screen.findByRole('table', { name: 'Audit log entries' });

    mocked.list.mockResolvedValueOnce(result([ROLE_CHANGE], 200000, 25, 100000));
    await user.click(screen.getByRole('button', { name: 'Go to last page' }));
    await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith({ limit: 25, offset: 100000 }));
    await screen.findByRole('row', { name: /change_role/ });

    expect(mocked.list.mock.calls.every(([query]) => (query?.offset ?? 0) <= 100000)).toBe(true);
  });

  it('says that only the first 100,000 entries can be paged when the filtered total is larger', async () => {
    mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 200000));
    renderPage();
    await screen.findByRole('table', { name: 'Audit log entries' });

    expect(screen.getByText(NOTE)).toBeInTheDocument();
  });

  it('shows no note when every entry of the filtered list can be paged', async () => {
    await renderLoaded([CREATE_ITEM], 30);

    expect(screen.queryByText(NOTE)).not.toBeInTheDocument();
  });

  it('treats an answer whose offset differs from the requested one as the clamped note, and shows no rows of it', async () => {
    const user = userEvent.setup();
    mocked.list.mockResolvedValueOnce(result([CREATE_ITEM], 200000));
    renderPage();
    await screen.findByRole('table', { name: 'Audit log entries' });

    // The request asks for offset 25; the server answers its clamped offset.
    mocked.list.mockResolvedValueOnce(result([ROLE_CHANGE], 200000, 25, 100000));
    await user.click(screen.getByRole('button', { name: 'Go to next page' }));

    await waitFor(() => expect(screen.getByText(NOTE)).toBeInTheDocument());
    expect(screen.queryByRole('row', { name: /change_role/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('table', { name: 'Audit log entries' })).not.toBeInTheDocument();
    // The controls stay, so the user can go back.
    expect(screen.getByRole('button', { name: 'Go to previous page' })).toBeEnabled();
  });
});

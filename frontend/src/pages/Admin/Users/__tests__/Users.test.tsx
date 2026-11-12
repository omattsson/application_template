import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AxiosError, AxiosHeaders } from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import AdminUsers from '../index';
import { userService } from '../../../../api/client';
import type { User, UserListResponse } from '../../../../api/client';
import { useAuth } from '../../../../context/AuthContext';
import type { AuthContextValue } from '../../../../context/AuthContext';

vi.mock('../../../../context/AuthContext', () => ({ useAuth: vi.fn() }));

vi.mock('../../../../api/client', () => ({
  userService: {
    list: vi.fn(),
    create: vi.fn(),
    changeRole: vi.fn(),
    setDisabled: vi.fn(),
    delete: vi.fn(),
  },
}));

const LAST_ADMIN_MESSAGE = 'The last enabled admin cannot be demoted, disabled or deleted';

function makeUser(overrides: Partial<User> = {}): User {
  return {
    id: '11111111-1111-4111-8111-111111111111',
    username: 'alice',
    email: 'alice@example.com',
    display_name: 'Alice Admin',
    role: 'admin',
    auth_provider: 'local',
    disabled: false,
    service_account: false,
    created_at: '2026-01-01T10:00:00Z',
    updated_at: '2026-01-01T10:00:00Z',
    ...overrides,
  };
}

const ALICE = makeUser();
const BOB = makeUser({
  id: '22222222-2222-4222-8222-222222222222',
  username: 'bob',
  email: 'bob@example.com',
  display_name: 'Bob Builder',
  role: 'user',
  created_at: '2026-02-02T10:00:00Z',
});
const CAROL = makeUser({
  id: '33333333-3333-4333-8333-333333333333',
  username: 'carol',
  email: '',
  display_name: 'Carol',
  role: 'devops',
  disabled: true,
  created_at: '2026-03-03T10:00:00Z',
});

function page(data: User[], total = data.length, pageNumber = 1, pageSize = 25): UserListResponse {
  return { data, total, page: pageNumber, pageSize };
}

/** An AxiosError as the client throws it for a non-2xx answer. */
function httpError(status: number, body: unknown): AxiosError {
  const config = { headers: new AxiosHeaders(), method: 'put', url: '/api/v1/users/x/role' } as InternalAxiosRequestConfig;
  return new AxiosError(`Request failed with status code ${status}`, AxiosError.ERR_BAD_REQUEST, config, {}, {
    data: body,
    status,
    statusText: String(status),
    headers: {},
    config,
  });
}

/** The signed-in user is alice (the admin of the fixtures). */
function authState(overrides: Partial<AuthContextValue> = {}): AuthContextValue {
  return {
    authEnabled: true,
    isLoading: false,
    isAuthenticated: true,
    user: { id: ALICE.id, username: 'alice', display_name: 'Alice Admin', role: 'admin' },
    role: 'admin',
    login: vi.fn(),
    logout: vi.fn(),
    loginWithOIDC: vi.fn(),
    ...overrides,
  };
}

const mocked = {
  list: vi.mocked(userService.list),
  create: vi.mocked(userService.create),
  changeRole: vi.mocked(userService.changeRole),
  setDisabled: vi.mocked(userService.setDisabled),
  delete: vi.mocked(userService.delete),
};

async function renderLoaded(users: User[] = [ALICE, BOB, CAROL]) {
  mocked.list.mockResolvedValue(page(users));
  render(<AdminUsers />);
  await screen.findByRole('table', { name: 'Users' });
}

function rowOf(username: string): HTMLElement {
  return screen.getByRole('row', { name: new RegExp(`^${username}\\b`) });
}

describe('Admin Users page', () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue(authState());
    mocked.delete.mockResolvedValue(undefined);
  });

  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  describe('list', () => {
    it('shows a loading state while the users load', () => {
      mocked.list.mockReturnValue(new Promise(() => {}));
      render(<AdminUsers />);
      expect(screen.getByRole('heading', { level: 1, name: 'Users' })).toBeInTheDocument();
      expect(screen.getByRole('progressbar')).toBeInTheDocument();
    });

    it('renders every user with username, email, display name, role, status and created date', async () => {
      await renderLoaded();

      const table = screen.getByRole('table', { name: 'Users' });
      for (const name of ['Username', 'Email', 'Display name', 'Role', 'Status', 'Created']) {
        expect(within(table).getByRole('columnheader', { name })).toBeInTheDocument();
      }
      const bob = within(rowOf('bob'));
      expect(bob.getByText('bob@example.com')).toBeInTheDocument();
      expect(bob.getByText('Bob Builder')).toBeInTheDocument();
      expect(bob.getByRole('combobox', { name: 'Role of bob' })).toHaveTextContent('user');
      expect(bob.getByText('Active')).toBeInTheDocument();
      expect(bob.getByText(new Date(BOB.created_at).toLocaleString())).toBeInTheDocument();
      expect(within(rowOf('carol')).getByText('Disabled')).toBeInTheDocument();
      expect(within(rowOf('carol')).getByRole('combobox', { name: 'Role of carol' })).toHaveTextContent('devops');
    });

    it('asks the backend for the first page of 25', async () => {
      await renderLoaded();
      expect(mocked.list).toHaveBeenCalledWith(1, 25);
    });

    it('shows an empty state when there are no users', async () => {
      mocked.list.mockResolvedValue(page([]));
      render(<AdminUsers />);
      expect(await screen.findByText('No users found')).toBeInTheDocument();
      expect(screen.queryByRole('table')).not.toBeInTheDocument();
    });

    it('shows the server message in an alert when the list fails, and retries', async () => {
      const user = userEvent.setup();
      mocked.list.mockRejectedValueOnce(httpError(403, { error: 'Admin role required' }));
      render(<AdminUsers />);
      expect(await screen.findByRole('alert')).toHaveTextContent('Admin role required');

      mocked.list.mockResolvedValue(page([ALICE]));
      await user.click(screen.getByRole('button', { name: 'Retry' }));
      expect(await screen.findByRole('table', { name: 'Users' })).toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('never shows the body of a 5xx answer', async () => {
      mocked.list.mockRejectedValueOnce(httpError(500, { error: 'pq: relation "users" does not exist' }));
      render(<AdminUsers />);
      const alert = await screen.findByRole('alert');
      expect(alert).not.toHaveTextContent('relation');
    });
  });

  describe('pagination', () => {
    it('shows the total and asks for the next page with the same page size', async () => {
      const user = userEvent.setup();
      mocked.list.mockImplementation(async (pageNumber) =>
        pageNumber === 1 ? page([ALICE], 40) : page([BOB], 40, 2),
      );
      render(<AdminUsers />);
      await screen.findByRole('table', { name: 'Users' });
      expect(screen.getByText('1–25 of 40')).toBeInTheDocument();

      await user.click(screen.getByRole('button', { name: /next page/i }));

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith(2, 25));
      expect(await screen.findByText('bob')).toBeInTheDocument();
    });

    it('goes back to page 1 when the page size changes', async () => {
      const user = userEvent.setup();
      mocked.list.mockResolvedValue(page([ALICE], 40));
      render(<AdminUsers />);
      await screen.findByRole('table', { name: 'Users' });

      await user.click(screen.getByRole('combobox', { name: /rows per page/i }));
      await user.click(await screen.findByRole('option', { name: '50' }));

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith(1, 50));
    });

    it('moves to the last page when a delete empties the current page', async () => {
      const user = userEvent.setup();
      mocked.list.mockImplementation(async (pageNumber) => {
        if (pageNumber === 2) return page([BOB], 26, 2);
        return page([ALICE], 26, 1);
      });
      render(<AdminUsers />);
      await screen.findByRole('table', { name: 'Users' });
      await user.click(screen.getByRole('button', { name: /next page/i }));
      await screen.findByText('bob');

      // After the delete, page 2 is empty and the total is 25.
      mocked.list.mockImplementation(async (pageNumber) =>
        pageNumber === 1 ? page([ALICE], 25, 1) : page([], 25, 2),
      );
      await user.click(screen.getByRole('button', { name: 'Delete bob' }));
      await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete' }));

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith(1, 25));
      expect(await screen.findByText('alice')).toBeInTheDocument();
    });
  });

  describe('role change', () => {
    it('calls PUT /users/:id/role with the new role and reloads the list', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.changeRole.mockResolvedValue({
        id: BOB.id, old_role: 'user', new_role: 'devops', message: 'Role changed', changed: true, idp_managed: false,
      });
      mocked.list.mockClear();

      await user.click(screen.getByRole('combobox', { name: 'Role of bob' }));
      await user.click(await screen.findByRole('option', { name: 'devops' }));

      await waitFor(() => expect(mocked.changeRole).toHaveBeenCalledWith(BOB.id, 'devops'));
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(1));
      expect(await screen.findByText('Role of bob changed to devops')).toBeInTheDocument();
    });

    it('shows the server message on a last-admin 409 and reloads the list', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.changeRole.mockRejectedValue(httpError(409, { error: LAST_ADMIN_MESSAGE }));
      mocked.list.mockClear();

      await user.click(screen.getByRole('combobox', { name: 'Role of alice' }));
      await user.click(await screen.findByRole('option', { name: 'user' }));

      expect(await screen.findByRole('alert')).toHaveTextContent(LAST_ADMIN_MESSAGE);
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(1));
      // The select shows the stored role again.
      expect(screen.getByRole('combobox', { name: 'Role of alice' })).toHaveTextContent('admin');
    });

    it('reloads the list after any failure, a 500 included', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.changeRole.mockRejectedValue(httpError(500, { error: 'internal detail' }));
      mocked.list.mockClear();

      await user.click(screen.getByRole('combobox', { name: 'Role of bob' }));
      await user.click(await screen.findByRole('option', { name: 'admin' }));

      const alert = await screen.findByRole('alert');
      expect(alert).not.toHaveTextContent('internal detail');
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(1));
    });
  });

  describe('disable and enable', () => {
    it('disables an active user and reloads the list', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.setDisabled.mockResolvedValue({ id: BOB.id, disabled: true, changed: true, message: 'User disabled' });
      mocked.list.mockClear();

      await user.click(screen.getByRole('button', { name: 'Disable bob' }));

      await waitFor(() => expect(mocked.setDisabled).toHaveBeenCalledWith(BOB.id, true));
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(1));
      expect(await screen.findByText('bob disabled')).toBeInTheDocument();
    });

    it('enables a disabled user', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.setDisabled.mockResolvedValue({ id: CAROL.id, disabled: false, changed: true, message: 'User enabled' });

      await user.click(screen.getByRole('button', { name: 'Enable carol' }));

      await waitFor(() => expect(mocked.setDisabled).toHaveBeenCalledWith(CAROL.id, false));
      expect(await screen.findByText('carol enabled')).toBeInTheDocument();
    });

    it('shows the server message on a sole-admin 409 and reloads the list', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.setDisabled.mockRejectedValue(httpError(409, { error: LAST_ADMIN_MESSAGE }));
      mocked.list.mockClear();

      await user.click(screen.getByRole('button', { name: 'Disable alice' }));

      expect(await screen.findByRole('alert')).toHaveTextContent(LAST_ADMIN_MESSAGE);
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(1));
    });
  });

  describe('delete', () => {
    it('asks for confirmation naming the user, and cancel deletes nothing', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      await user.click(screen.getByRole('button', { name: 'Delete bob' }));
      const dialog = await screen.findByRole('dialog', { name: 'Delete user' });
      expect(dialog).toHaveAccessibleDescription(/bob/);
      expect(mocked.delete).not.toHaveBeenCalled();

      await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));

      expect(mocked.delete).not.toHaveBeenCalled();
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    });

    it('deletes after confirmation and reloads the list', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.list.mockClear();
      mocked.list.mockResolvedValue(page([ALICE, CAROL]));

      await user.click(screen.getByRole('button', { name: 'Delete bob' }));
      await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete' }));

      await waitFor(() => expect(mocked.delete).toHaveBeenCalledWith(BOB.id));
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(1));
      await waitFor(() => expect(screen.queryByRole('row', { name: /^bob\b/ })).not.toBeInTheDocument());
      expect(await screen.findByText('User bob deleted')).toBeInTheDocument();
    });

    it('does not submit twice on a double click', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      let finish: () => void = () => {};
      mocked.delete.mockReturnValue(new Promise<void>((resolve) => { finish = resolve; }));

      await user.click(screen.getByRole('button', { name: 'Delete bob' }));
      const confirm = within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete' });
      await user.dblClick(confirm);

      expect(mocked.delete).toHaveBeenCalledTimes(1);
      finish();
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(2));
    });

    it('keeps the name in the dialog message while it closes (no flicker to an empty name)', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      let finish: () => void = () => {};
      mocked.delete.mockReturnValue(new Promise<void>((resolve) => { finish = resolve; }));

      await user.click(screen.getByRole('button', { name: 'Delete bob' }));
      await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete' }));

      // The dialog is still on screen (exit transition) and still names the user.
      const closing = screen.getByRole('dialog', { name: 'Delete user', hidden: true });
      expect(closing).toHaveTextContent('Delete user "bob"?');
      finish();
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(2));
    });

    it('shows the server message on a last-admin 409 and reloads the list', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.delete.mockRejectedValue(httpError(409, { error: LAST_ADMIN_MESSAGE }));
      mocked.list.mockClear();

      await user.click(screen.getByRole('button', { name: 'Delete alice' }));
      await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete' }));

      expect(await screen.findByRole('alert')).toHaveTextContent(LAST_ADMIN_MESSAGE);
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(1));
    });

    it('reloads the list after a 404 (the user is already gone) so no stale row stays', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.delete.mockRejectedValue(httpError(404, { error: 'User not found' }));
      mocked.list.mockClear();
      mocked.list.mockResolvedValue(page([ALICE, CAROL]));

      await user.click(screen.getByRole('button', { name: 'Delete bob' }));
      await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete' }));

      expect(await screen.findByRole('alert')).toHaveTextContent('User not found');
      await waitFor(() => expect(screen.queryByRole('row', { name: /^bob\b/ })).not.toBeInTheDocument());
    });
  });

  describe('change in flight', () => {
    it('disables the role selects and every row button until the change and the reload finish', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      let finish: (r: Awaited<ReturnType<typeof userService.changeRole>>) => void = () => {};
      mocked.changeRole.mockReturnValue(new Promise((resolve) => { finish = resolve; }));

      await user.click(screen.getByRole('combobox', { name: 'Role of bob' }));
      await user.click(await screen.findByRole('option', { name: 'devops' }));

      await waitFor(() => expect(screen.getByRole('combobox', { name: 'Role of carol' })).toHaveAttribute('aria-disabled', 'true'));
      expect(screen.getByRole('combobox', { name: 'Role of bob' })).toHaveAttribute('aria-disabled', 'true');
      expect(screen.getByRole('button', { name: 'Disable bob' })).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Enable carol' })).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Delete alice' })).toBeDisabled();

      finish({ id: BOB.id, old_role: 'user', new_role: 'devops', message: 'Role changed', changed: true, idp_managed: false });
      await waitFor(() => expect(screen.getByRole('button', { name: 'Delete alice' })).toBeEnabled());
      expect(screen.getByRole('combobox', { name: 'Role of bob' })).not.toHaveAttribute('aria-disabled', 'true');
    });
  });

  describe('own account', () => {
    it('marks the signed-in user with (you) and nobody else', async () => {
      await renderLoaded();
      expect(within(rowOf('alice')).getByText('(you)')).toBeInTheDocument();
      expect(within(rowOf('bob')).queryByText('(you)')).not.toBeInTheDocument();
    });

    it('warns in the delete dialog when the target is yourself, and not for another user', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      await user.click(screen.getByRole('button', { name: 'Delete alice' }));
      expect(await screen.findByRole('dialog', { name: 'Delete user' })).toHaveAccessibleDescription(/your own account/i);
      await user.click(screen.getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

      await user.click(screen.getByRole('button', { name: 'Delete bob' }));
      expect(await screen.findByRole('dialog', { name: 'Delete user' })).not.toHaveAccessibleDescription(/your own account/i);
    });

    it('warns after changing your own role, and still lets the backend decide', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.changeRole.mockResolvedValue({
        id: ALICE.id, old_role: 'admin', new_role: 'devops', message: 'Role changed', changed: true, idp_managed: false,
      });

      await user.click(screen.getByRole('combobox', { name: 'Role of alice' }));
      await user.click(await screen.findByRole('option', { name: 'devops' }));

      await waitFor(() => expect(mocked.changeRole).toHaveBeenCalledWith(ALICE.id, 'devops'));
      expect(await screen.findByText(/Role of alice changed to devops.*your own account/)).toBeInTheDocument();
    });

    it('warns after disabling yourself', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.setDisabled.mockResolvedValue({ id: ALICE.id, disabled: true, changed: true, message: 'User disabled' });

      await user.click(screen.getByRole('button', { name: 'Disable alice' }));

      expect(await screen.findByText(/alice disabled.*your own account/)).toBeInTheDocument();
    });
  });

  describe('auth disabled', () => {
    it('explains that user management needs authentication and calls no API', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ authEnabled: false, isAuthenticated: false, user: null, role: null }));
      render(<AdminUsers />);
      expect(screen.getByText(/requires authentication to be enabled/i)).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Create user' })).not.toBeInTheDocument();
      expect(mocked.list).not.toHaveBeenCalled();
    });
  });

  describe('inconsistent page', () => {
    it('stops fetching and keeps the pagination when an empty page has a total that says it should exist', async () => {
      const user = userEvent.setup();
      mocked.list.mockImplementation(async (pageNumber) =>
        pageNumber === 1 ? page([ALICE], 50) : page([], 50, 2),
      );
      render(<AdminUsers />);
      await screen.findByRole('table', { name: 'Users' });

      await user.click(screen.getByRole('button', { name: /next page/i }));

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith(2, 25));
      await waitFor(() => expect(screen.getByRole('table', { name: 'Users' }).closest('[aria-busy]')).toHaveAttribute('aria-busy', 'false'));
      // The user can still page back.
      expect(screen.getByRole('button', { name: /previous page/i })).toBeEnabled();
    });
  });

  describe('create user', () => {
    async function openDialog() {
      const user = userEvent.setup();
      await renderLoaded();
      await user.click(screen.getByRole('button', { name: 'Create user' }));
      const dialog = await screen.findByRole('dialog', { name: 'Create user' });
      return { user, dialog };
    }

    it('has labelled fields and a new-password autocomplete on the password', async () => {
      const { dialog } = await openDialog();
      for (const label of ['Username', 'Email', 'Display name', 'Password']) {
        expect(within(dialog).getByLabelText(label, { exact: false })).toBeInTheDocument();
      }
      expect(within(dialog).getByLabelText('Password', { exact: false })).toHaveAttribute('autocomplete', 'new-password');
      expect(within(dialog).getByLabelText('Password', { exact: false })).toHaveAttribute('type', 'password');
      expect(within(dialog).getByRole('combobox', { name: /role/i })).toHaveTextContent('user');
    });

    it('creates the user, closes the dialog and reloads the list', async () => {
      const { user, dialog } = await openDialog();
      mocked.create.mockResolvedValue(makeUser({ id: 'new', username: 'dave' }));
      mocked.list.mockClear();

      await user.type(within(dialog).getByLabelText('Username', { exact: false }), 'dave');
      await user.type(within(dialog).getByLabelText('Email', { exact: false }), 'dave@example.com');
      await user.type(within(dialog).getByLabelText('Display name', { exact: false }), 'Dave D');
      await user.type(within(dialog).getByLabelText('Password', { exact: false }), 'correct horse');
      await user.click(within(dialog).getByRole('combobox', { name: /role/i }));
      await user.click(await screen.findByRole('option', { name: 'devops' }));
      await user.click(within(dialog).getByRole('button', { name: 'Create' }));

      await waitFor(() =>
        expect(mocked.create).toHaveBeenCalledWith({
          username: 'dave',
          email: 'dave@example.com',
          display_name: 'Dave D',
          password: 'correct horse',
          role: 'devops',
        }),
      );
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
      expect(mocked.list).toHaveBeenCalledTimes(1);
      expect(await screen.findByText('User dave created')).toBeInTheDocument();
    });

    it('shows validation errors and does not call the API for an empty form', async () => {
      const { user, dialog } = await openDialog();

      await user.click(within(dialog).getByRole('button', { name: 'Create' }));

      expect(mocked.create).not.toHaveBeenCalled();
      expect(within(dialog).getByLabelText('Username', { exact: false })).toBeInvalid();
      expect(within(dialog).getByText(/Username must be 3 to 100 characters/)).toBeInTheDocument();
      expect(within(dialog).getByText('Password must be at least 8 characters')).toBeInTheDocument();
    });

    it.each([
      ['a username with a space', 'bad name', 'Username must be 3 to 100 characters'],
      ['a username starting with a dot', '.bob', 'Username must be 3 to 100 characters'],
      ['a username that is too short', 'ab', 'Username must be 3 to 100 characters'],
      ['a username that is too long', 'a'.repeat(101), 'Username must be 3 to 100 characters'],
    ])('rejects %s', async (_name, username, message) => {
      const { user, dialog } = await openDialog();
      await user.type(within(dialog).getByLabelText('Username', { exact: false }), username);
      await user.type(within(dialog).getByLabelText('Password', { exact: false }), 'long enough password');
      await user.click(within(dialog).getByRole('button', { name: 'Create' }));
      expect(within(dialog).getByText(new RegExp(message))).toBeInTheDocument();
      expect(mocked.create).not.toHaveBeenCalled();
    });

    it('rejects an invalid email and a password over 72 bytes', async () => {
      const { user, dialog } = await openDialog();
      await user.type(within(dialog).getByLabelText('Username', { exact: false }), 'dave');
      await user.type(within(dialog).getByLabelText('Email', { exact: false }), 'not-an-email');
      await user.type(within(dialog).getByLabelText('Password', { exact: false }), 'x'.repeat(73));
      await user.click(within(dialog).getByRole('button', { name: 'Create' }));
      expect(within(dialog).getByText('Invalid email address')).toBeInTheDocument();
      expect(within(dialog).getByText('Password must be at most 72 bytes')).toBeInTheDocument();
      expect(mocked.create).not.toHaveBeenCalled();
    });

    it('allows an empty email and display name', async () => {
      const { user, dialog } = await openDialog();
      mocked.create.mockResolvedValue(makeUser({ username: 'dave' }));
      await user.type(within(dialog).getByLabelText('Username', { exact: false }), 'dave');
      await user.type(within(dialog).getByLabelText('Password', { exact: false }), 'long enough password');
      await user.click(within(dialog).getByRole('button', { name: 'Create' }));
      await waitFor(() =>
        expect(mocked.create).toHaveBeenCalledWith({
          username: 'dave', email: '', display_name: '', password: 'long enough password', role: 'user',
        }),
      );
    });

    it('keeps the dialog open and shows the server message when the username exists (409)', async () => {
      const { user, dialog } = await openDialog();
      mocked.create.mockRejectedValue(httpError(409, { error: 'User already exists' }));
      await user.type(within(dialog).getByLabelText('Username', { exact: false }), 'alice');
      await user.type(within(dialog).getByLabelText('Password', { exact: false }), 'long enough password');
      await user.click(within(dialog).getByRole('button', { name: 'Create' }));

      expect(await within(dialog).findByRole('alert')).toHaveTextContent('User already exists');
      expect(screen.getByRole('dialog', { name: 'Create user' })).toBeInTheDocument();
    });

    it('does not submit twice on a double click', async () => {
      const { user, dialog } = await openDialog();
      let finish: (u: User) => void = () => {};
      mocked.create.mockReturnValue(new Promise<User>((resolve) => { finish = resolve; }));
      await user.type(within(dialog).getByLabelText('Username', { exact: false }), 'dave');
      await user.type(within(dialog).getByLabelText('Password', { exact: false }), 'long enough password');

      await user.dblClick(within(dialog).getByRole('button', { name: 'Create' }));

      expect(mocked.create).toHaveBeenCalledTimes(1);
      finish(makeUser({ username: 'dave' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    });

    it('ignores Escape while the request is in flight, and closes after it', async () => {
      const { user, dialog } = await openDialog();
      let finish: (u: User) => void = () => {};
      mocked.create.mockReturnValue(new Promise<User>((resolve) => { finish = resolve; }));
      await user.type(within(dialog).getByLabelText('Username', { exact: false }), 'dave');
      await user.type(within(dialog).getByLabelText('Password', { exact: false }), 'long enough password');
      await user.click(within(dialog).getByRole('button', { name: 'Create' }));
      await waitFor(() => expect(mocked.create).toHaveBeenCalledTimes(1));

      // The key goes to the dialog only while the focus is inside it.
      await user.click(within(dialog).getByLabelText('Username', { exact: false }));
      await user.keyboard('{Escape}');

      // Longer than the exit transition of the dialog: a close would have removed it by now.
      await new Promise((resolve) => setTimeout(resolve, 500));
      expect(screen.getByRole('dialog', { name: 'Create user' })).toBeInTheDocument();
      finish(makeUser({ username: 'dave' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    });

    it('closes on Escape when nothing is in flight', async () => {
      const { user } = await openDialog();
      await user.keyboard('{Escape}');
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    });

    it('moves the focus to the first invalid field and ties its message to it', async () => {
      const { user, dialog } = await openDialog();

      await user.click(within(dialog).getByRole('button', { name: 'Create' }));
      const username = within(dialog).getByLabelText('Username', { exact: false });
      expect(username).toHaveFocus();
      expect(username).toHaveAccessibleDescription(/Username must be 3 to 100 characters/);

      await user.type(username, 'dave');
      await user.type(within(dialog).getByLabelText('Email', { exact: false }), 'nope');
      await user.click(within(dialog).getByRole('button', { name: 'Create' }));
      expect(within(dialog).getByLabelText('Email', { exact: false })).toHaveFocus();

      await user.clear(within(dialog).getByLabelText('Email', { exact: false }));
      await user.click(within(dialog).getByRole('button', { name: 'Create' }));
      expect(within(dialog).getByLabelText('Password', { exact: false })).toHaveFocus();
      expect(mocked.create).not.toHaveBeenCalled();
    });

    it('clears the form (and the password) when the dialog is reopened', async () => {
      const { user, dialog } = await openDialog();
      await user.type(within(dialog).getByLabelText('Username', { exact: false }), 'dave');
      await user.type(within(dialog).getByLabelText('Password', { exact: false }), 'long enough password');
      await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

      await user.click(screen.getByRole('button', { name: 'Create user' }));
      const again = await screen.findByRole('dialog', { name: 'Create user' });

      expect(within(again).getByLabelText('Username', { exact: false })).toHaveValue('');
      expect(within(again).getByLabelText('Password', { exact: false })).toHaveValue('');
    });
  });
});

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AxiosError, AxiosHeaders } from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import ApiKeysPage from '../index';
import { apiKeyService } from '../../../../api/client';
import type { ApiKey, ApiKeyListResponse, CreatedApiKey } from '../../../../api/client';

vi.mock('../../../../api/client', () => ({
  apiKeyService: {
    list: vi.fn(),
    create: vi.fn(),
    delete: vi.fn(),
  },
}));

const RAW_KEY = `sk_${'0123456789abcdef'.repeat(4)}`;

const DEPLOY: ApiKey = {
  id: '11111111-1111-4111-8111-111111111111',
  user_id: '99999999-9999-4999-8999-999999999999',
  name: 'deploy',
  prefix: 'sk_a1b2',
  created_at: '2026-03-01T10:00:00Z',
  last_used_at: '2026-03-02T10:00:00Z',
  expires_at: '2099-01-01T00:00:00Z',
};

const OLD_CI: ApiKey = {
  id: '22222222-2222-4222-8222-222222222222',
  user_id: '99999999-9999-4999-8999-999999999999',
  name: 'old-ci',
  prefix: 'sk_c3d4',
  created_at: '2025-01-01T10:00:00Z',
  last_used_at: null,
  expires_at: '2025-06-01T10:00:00Z',
};

function page(data: ApiKey[], total = data.length, pageNumber = 1, pageSize = 25): ApiKeyListResponse {
  return { data, total, page: pageNumber, pageSize };
}

/** An AxiosError as the client throws it for a non-2xx answer. */
function httpError(status: number, body: unknown): AxiosError {
  const config = { headers: new AxiosHeaders(), method: 'post', url: '/api/v1/api-keys' } as InternalAxiosRequestConfig;
  return new AxiosError(`Request failed with status code ${status}`, AxiosError.ERR_BAD_REQUEST, config, {}, {
    data: body,
    status,
    statusText: String(status),
    headers: {},
    config,
  });
}

function createdKey(name: string, overrides: Partial<CreatedApiKey> = {}): CreatedApiKey {
  return {
    id: '33333333-3333-4333-8333-333333333333',
    name,
    prefix: 'sk_0123',
    raw_key: RAW_KEY,
    created_at: '2026-11-21T10:00:00Z',
    expires_at: '2027-02-19T10:00:00Z',
    ...overrides,
  };
}

const mocked = {
  list: vi.mocked(apiKeyService.list),
  create: vi.mocked(apiKeyService.create),
  delete: vi.mocked(apiKeyService.delete),
};

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

function renderPage() {
  return render(<ApiKeysPage />);
}

async function renderLoaded(keys: ApiKey[] = [DEPLOY, OLD_CI]) {
  mocked.list.mockResolvedValue(page(keys));
  renderPage();
  await screen.findByRole('table', { name: 'API keys' });
}

function rowOf(name: string): HTMLElement {
  return screen.getByRole('row', { name: new RegExp(`^${name}\\b`) });
}

/** Open the create dialog and return it. */
async function openCreate(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Create API key' }));
  return screen.getByRole('dialog', { name: 'Create API key' });
}

/** Fill the create form and submit it. */
async function submitCreate(user: ReturnType<typeof userEvent.setup>, name: string) {
  const dialog = screen.getByRole('dialog', { name: 'Create API key' });
  await user.type(within(dialog).getByRole('textbox', { name: /^name/i }), name);
  await user.click(within(dialog).getByRole('button', { name: 'Create' }));
}

describe('API keys page', () => {
  beforeEach(() => {
    mocked.delete.mockResolvedValue(undefined);
  });

  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  describe('list', () => {
    it('shows a loading state while the first page loads', async () => {
      const pending = controlled<ApiKeyListResponse>();
      mocked.list.mockReturnValue(pending.promise);
      renderPage();

      expect(screen.getByRole('heading', { name: 'API keys', level: 1 })).toBeInTheDocument();
      expect(screen.getByRole('progressbar', { name: 'Loading API keys' })).toBeInTheDocument();

      pending.resolve(page([DEPLOY]));
      await screen.findByRole('table', { name: 'API keys' });
      expect(screen.queryByRole('progressbar', { name: 'Loading API keys' })).not.toBeInTheDocument();
    });

    it('lists the keys of the first page, newest first as the server sends them, with their details', async () => {
      await renderLoaded();

      expect(mocked.list).toHaveBeenCalledWith(1, 25);
      const headers = screen.getAllByRole('columnheader').map((th) => th.textContent);
      expect(headers).toEqual(['Name', 'Prefix', 'Created', 'Last used', 'Expires', 'Actions']);

      const deploy = rowOf('deploy');
      expect(within(deploy).getByText('sk_a1b2')).toBeInTheDocument();
      expect(within(deploy).getByText(new Date(DEPLOY.created_at).toLocaleString())).toBeInTheDocument();
      expect(within(deploy).getByText(new Date(DEPLOY.last_used_at as string).toLocaleString())).toBeInTheDocument();
      expect(within(deploy).queryByText('Expired')).not.toBeInTheDocument();
    });

    it('marks an expired key with the text Expired, and a key never used with Never', async () => {
      await renderLoaded([OLD_CI]);

      const oldCi = rowOf('old-ci');
      expect(within(oldCi).getByText('Expired')).toBeInTheDocument();
      expect(within(oldCi).getByText('Never')).toBeInTheDocument();
    });

    it('shows the empty state with the create button when there are no keys', async () => {
      mocked.list.mockResolvedValue(page([]));
      renderPage();

      expect(await screen.findByText('No API keys yet')).toBeInTheDocument();
      expect(screen.queryByRole('table', { name: 'API keys' })).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Create API key' })).toBeEnabled();
    });

    it('shows the server message when the list fails, and Retry loads it again', async () => {
      const user = userEvent.setup();
      mocked.list.mockRejectedValueOnce(httpError(403, { error: 'API keys can only be managed with a login session' }));
      renderPage();

      expect(await screen.findByRole('alert')).toHaveTextContent('API keys can only be managed with a login session');

      mocked.list.mockResolvedValueOnce(page([DEPLOY]));
      await user.click(screen.getByRole('button', { name: 'Retry' }));

      await screen.findByRole('table', { name: 'API keys' });
      expect(mocked.list).toHaveBeenCalledTimes(2);
    });

    it('pages through the keys with the server total and page numbers', async () => {
      const user = userEvent.setup();
      mocked.list.mockResolvedValueOnce(page([DEPLOY], 30, 1, 25));
      renderPage();
      await screen.findByRole('table', { name: 'API keys' });

      mocked.list.mockResolvedValueOnce(page([OLD_CI], 30, 2, 25));
      await user.click(screen.getByRole('button', { name: 'Go to next page' }));

      await waitFor(() => expect(mocked.list).toHaveBeenLastCalledWith(2, 25));
      await screen.findByRole('row', { name: /^old-ci\b/ });
      expect(screen.getByText('26–30 of 30')).toBeInTheDocument();
    });
  });

  describe('create', () => {
    it('asks for a name and an expiry, with 90 days as the default', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      const dialog = await openCreate(user);

      expect(within(dialog).getByRole('textbox', { name: /^name/i })).toHaveValue('');
      expect(within(dialog).getByRole('combobox', { name: /expires in/i })).toHaveTextContent('90 days');
    });

    it('offers 30, 90, 180 and 365 days', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      const dialog = await openCreate(user);

      await user.click(within(dialog).getByRole('combobox', { name: /expires in/i }));
      const options = screen.getAllByRole('option').map((o) => o.textContent);
      expect(options).toEqual(['30 days', '90 days', '180 days', '365 days']);
    });

    it('refuses an empty name without a request, and moves the focus to the name', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      const dialog = await openCreate(user);

      await user.click(within(dialog).getByRole('button', { name: 'Create' }));

      expect(within(dialog).getByText('Enter a name for the key.')).toBeInTheDocument();
      expect(within(dialog).getByRole('textbox', { name: /^name/i })).toHaveFocus();
      expect(mocked.create).not.toHaveBeenCalled();
    });

    it('refuses a name with a character outside the server set, without a request', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      await openCreate(user);

      await submitCreate(user, 'bad!name');

      expect(screen.getByText('Use only letters, digits, spaces and . _ - : @ / ( ) +.')).toBeInTheDocument();
      expect(mocked.create).not.toHaveBeenCalled();
    });

    it('creates the key with the name and the expiry, and shows the raw key once', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.create.mockResolvedValue(createdKey('ci'));
      const dialog = await openCreate(user);

      await user.type(within(dialog).getByRole('textbox', { name: /^name/i }), '  ci  ');
      await user.click(within(dialog).getByRole('combobox', { name: /expires in/i }));
      await user.click(screen.getByRole('option', { name: '365 days' }));
      await user.click(within(dialog).getByRole('button', { name: 'Create' }));

      const secret = await within(dialog).findByRole('textbox', { name: 'Your API key' });
      expect(secret).toHaveValue(RAW_KEY);
      expect(secret).toHaveAttribute('readonly');
      expect(within(dialog).getByText(/shown only once/i)).toBeInTheDocument();
      expect(mocked.create).toHaveBeenCalledWith({ name: 'ci', expires_in_days: 365 });
    });

    it('reloads the list after a create, while the dialog keeps the key on screen', async () => {
      const user = userEvent.setup();
      await renderLoaded([OLD_CI]);
      mocked.create.mockResolvedValue(createdKey('ci'));
      mocked.list.mockResolvedValue(page([DEPLOY, OLD_CI]));
      const dialog = await openCreate(user);

      await submitCreate(user, 'ci');

      await within(dialog).findByRole('textbox', { name: 'Your API key' });
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(2));
      expect(screen.getByRole('dialog', { name: 'API key created' })).toBeInTheDocument();
    });

    it('shows the server message and keeps the form when the create is refused', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.create.mockRejectedValue(httpError(409, { error: 'You have reached the limit of 25 active API keys' }));
      const dialog = await openCreate(user);

      await submitCreate(user, 'ci');

      expect(await within(dialog).findByText('You have reached the limit of 25 active API keys')).toBeInTheDocument();
      expect(within(dialog).getByRole('textbox', { name: /^name/i })).toHaveValue('ci');
      expect(within(dialog).queryByRole('textbox', { name: 'Your API key' })).not.toBeInTheDocument();
    });

    it('shows the 400 text of the server for a name it refuses', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.create.mockRejectedValue(httpError(400, { error: 'Expiry exceeds the maximum lifetime of 365 days' }));
      const dialog = await openCreate(user);

      await submitCreate(user, 'ci');

      expect(await within(dialog).findByText('Expiry exceeds the maximum lifetime of 365 days')).toBeInTheDocument();
    });

    it('sends one request for a double click on Create', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      const pending = controlled<CreatedApiKey>();
      mocked.create.mockReturnValue(pending.promise);
      const dialog = await openCreate(user);

      await user.type(within(dialog).getByRole('textbox', { name: /^name/i }), 'ci');
      const create = within(dialog).getByRole('button', { name: 'Create' });
      await user.dblClick(create);
      pending.resolve(createdKey('ci'));

      await within(dialog).findByRole('textbox', { name: 'Your API key' });
      expect(mocked.create).toHaveBeenCalledTimes(1);
    });

    it('closes the form on Escape and on Cancel', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      await openCreate(user);
      await user.keyboard('{Escape}');
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();

      await openCreate(user);
      await user.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(mocked.create).not.toHaveBeenCalled();
    });
  });

  describe('the raw key', () => {
    /** Create a key with the dialog and return the dialog with the key shown. */
    async function createShown(user: ReturnType<typeof userEvent.setup>) {
      mocked.create.mockResolvedValue(createdKey('ci'));
      const dialog = await openCreate(user);
      await submitCreate(user, 'ci');
      await within(dialog).findByRole('textbox', { name: 'Your API key' });
      return screen.getByRole('dialog', { name: 'API key created' });
    }

    it('does not close on Escape or on a click on the backdrop, only on Done', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      const dialog = await createShown(user);

      await user.keyboard('{Escape}');
      expect(screen.getByRole('dialog', { name: 'API key created' })).toBeInTheDocument();

      const container = document.querySelector('.MuiDialog-container');
      expect(container).not.toBeNull();
      await user.click(container as Element);
      expect(screen.getByRole('dialog', { name: 'API key created' })).toBeInTheDocument();
      expect(within(dialog).getByRole('textbox', { name: 'Your API key' })).toHaveValue(RAW_KEY);
    });

    it('is dropped from the page when Done closes the dialog, and a new dialog starts empty', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      await createShown(user);

      await user.click(screen.getByRole('button', { name: 'Done' }));

      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(screen.queryByDisplayValue(RAW_KEY)).not.toBeInTheDocument();
      expect(document.body.textContent).not.toContain(RAW_KEY);

      await openCreate(user);
      expect(screen.getByRole('textbox', { name: /^name/i })).toHaveValue('');
    });

    it('copies the key with the copy button and confirms it in text', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      await createShown(user);
      const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined);

      await user.click(screen.getByRole('button', { name: 'Copy key' }));

      expect(writeText).toHaveBeenCalledWith(RAW_KEY);
      expect(await screen.findByText('Copied to the clipboard.')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Copied' })).toBeInTheDocument();
    });

    it('says how to copy by hand when the clipboard refuses', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      await createShown(user);
      vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(new Error('Permission denied'));

      await user.click(screen.getByRole('button', { name: 'Copy key' }));

      expect(await screen.findByText('Copy failed: select the key and copy it by hand.')).toBeInTheDocument();
    });

    it('says how to copy by hand when the clipboard is not available, and the key stays selectable', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      await createShown(user);
      const original = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
      Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
      try {
        await user.click(screen.getByRole('button', { name: 'Copy key' }));

        expect(await screen.findByText('Copy failed: select the key and copy it by hand.')).toBeInTheDocument();
        const secret = screen.getByRole('textbox', { name: 'Your API key' });
        expect(secret).toHaveValue(RAW_KEY);
        expect(secret).not.toHaveAttribute('disabled');
      } finally {
        if (original) Object.defineProperty(navigator, 'clipboard', original);
      }
    });

    it('never writes the key to the URL, the storage or the console', async () => {
      const user = userEvent.setup();
      const consoleSpies = [
        vi.spyOn(console, 'log').mockImplementation(() => {}),
        vi.spyOn(console, 'info').mockImplementation(() => {}),
        vi.spyOn(console, 'warn').mockImplementation(() => {}),
        vi.spyOn(console, 'error').mockImplementation(() => {}),
      ];
      await renderLoaded();
      await createShown(user);

      expect(window.location.href).not.toContain(RAW_KEY);
      expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain(RAW_KEY);
      for (const spy of consoleSpies) {
        expect(JSON.stringify(spy.mock.calls)).not.toContain(RAW_KEY);
      }
    });
  });

  describe('revoke', () => {
    it('asks for confirmation first and does not call the API on Cancel', async () => {
      const user = userEvent.setup();
      await renderLoaded();

      await user.click(screen.getByRole('button', { name: 'Revoke deploy' }));
      const confirm = screen.getByRole('dialog', { name: 'Revoke API key' });
      expect(within(confirm).getByText(/"deploy"/)).toBeInTheDocument();

      await user.click(within(confirm).getByRole('button', { name: 'Cancel' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
      expect(mocked.delete).not.toHaveBeenCalled();
    });

    it('deletes the key on confirm, then reloads the list and says so', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.list.mockResolvedValueOnce(page([OLD_CI]));

      await user.click(screen.getByRole('button', { name: 'Revoke deploy' }));
      await user.click(within(screen.getByRole('dialog', { name: 'Revoke API key' })).getByRole('button', { name: 'Revoke' }));

      await waitFor(() => expect(mocked.delete).toHaveBeenCalledWith(DEPLOY.id));
      expect(await screen.findByText('Key deploy revoked')).toBeInTheDocument();
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(2));
      await waitFor(() => expect(screen.queryByRole('row', { name: /^deploy\b/ })).not.toBeInTheDocument());
    });

    it('treats a 404 as already revoked: it reloads the list and shows the note', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.delete.mockRejectedValue(httpError(404, { error: 'API key not found' }));
      mocked.list.mockResolvedValueOnce(page([OLD_CI]));

      await user.click(screen.getByRole('button', { name: 'Revoke deploy' }));
      await user.click(within(screen.getByRole('dialog', { name: 'Revoke API key' })).getByRole('button', { name: 'Revoke' }));

      expect(await screen.findByText('This key was already revoked. The list has been reloaded.')).toBeInTheDocument();
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(2));
    });

    it('shows the server message for another failure, and reloads the list', async () => {
      const user = userEvent.setup();
      await renderLoaded();
      mocked.delete.mockRejectedValue(httpError(403, { error: 'API keys can only be managed with a login session' }));

      await user.click(screen.getByRole('button', { name: 'Revoke deploy' }));
      await user.click(within(screen.getByRole('dialog', { name: 'Revoke API key' })).getByRole('button', { name: 'Revoke' }));

      expect(await screen.findByText('API keys can only be managed with a login session')).toBeInTheDocument();
      await waitFor(() => expect(mocked.list).toHaveBeenCalledTimes(2));
      // The confirm dialog fades out first: the table is hidden from the accessibility tree until then.
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
      expect(screen.getByRole('row', { name: /^deploy\b/ })).toBeInTheDocument();
    });
  });
});

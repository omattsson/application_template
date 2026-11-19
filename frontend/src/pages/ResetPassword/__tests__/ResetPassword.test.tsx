import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { AxiosError } from 'axios';
import type { AxiosResponse } from 'axios';
import ResetPassword from '../index';
import { passwordResetService } from '../../../api/client';
import { LOGIN_NOTICE_KEY, RESET_CHECK_LOGIN, RESET_COMPLETED } from '../../../utils/loginNotice';
import { reachableText } from '../../../test/mockHttp';

vi.mock('../../../api/client', () => ({
  passwordResetService: {
    requestPasswordReset: vi.fn(),
    confirmPasswordReset: vi.fn(),
  },
}));

const confirm = vi.mocked(passwordResetService.confirmPasswordReset);

const TOKEN = 'Zq3_'.repeat(10) + 'Zq3';
const PASSWORD = 'a-new-pass-phrase';
const INVALID_TOKEN = 'Invalid or expired token';

/** An error as the API client rejects it for an HTTP status and body. */
function httpError(status: number, data: unknown = {}): AxiosError {
  const response = { status, statusText: '', headers: {}, config: {}, data } as unknown as AxiosResponse;
  return new AxiosError(`Request failed with status code ${status}`, 'ERR_BAD_REQUEST', undefined, undefined, response);
}

function Where() {
  const location = useLocation();
  return <p>at {location.pathname}</p>;
}

function renderAt(hash: string) {
  window.history.replaceState(null, '', `/reset-password${hash}`);
  return render(
    <MemoryRouter initialEntries={['/reset-password']}>
      <Routes>
        <Route path="/reset-password" element={<ResetPassword />} />
        <Route path="*" element={<Where />} />
      </Routes>
    </MemoryRouter>,
  );
}

async function fill(password: string, confirmation = password) {
  const user = userEvent.setup();
  await user.type(screen.getByLabelText(/^new password/i), password);
  await user.type(screen.getByLabelText(/^confirm new password/i), confirmation);
  await user.click(screen.getByRole('button', { name: 'Reset password' }));
}

describe('ResetPassword page', () => {
  beforeEach(() => {
    window.history.replaceState(null, '', '/');
  });

  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
    sessionStorage.clear();
    localStorage.clear();
    window.history.replaceState(null, '', '/');
  });

  it('reads the token from the fragment and removes it from the address at once', async () => {
    renderAt(`#token=${TOKEN}`);
    expect(await screen.findByRole('heading', { name: 'Set a new password', level: 1 })).toBeInTheDocument();
    expect(window.location.hash).toBe('');
    expect(window.location.href).not.toContain(TOKEN);
    expect(window.location.pathname).toBe('/reset-password');
  });

  it('keeps the token out of the page text and out of the storage', async () => {
    renderAt(`#token=${TOKEN}`);
    await screen.findByRole('heading', { name: 'Set a new password' });
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
  });

  it('asks for a new password with the same rules, twice', async () => {
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    expect(screen.getByLabelText(/^new password/i)).toHaveAttribute('type', 'password');
    expect(screen.getByLabelText(/^new password/i)).toHaveAttribute('autocomplete', 'new-password');
    expect(screen.getByLabelText(/^confirm new password/i)).toHaveAttribute('type', 'password');
    expect(screen.getByText(/at least 8 characters/i)).toBeInTheDocument();
  });

  it('shows the invalid-link message with a way to a new link when the fragment has no token', async () => {
    renderAt('#token=too-short');
    expect(await screen.findByText(/not valid/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/^new password/i)).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Request a new reset link' })).toHaveAttribute('href', '/forgot-password');
    expect(window.location.hash).toBe('');
  });

  it('shows the invalid-link message when the link has no fragment at all', async () => {
    renderAt('');
    expect(await screen.findByText(/not valid/i)).toBeInTheDocument();
    expect(confirm).not.toHaveBeenCalled();
  });

  it('refuses a mismatch without calling the server', async () => {
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD, 'another-pass-phrase');
    expect(await screen.findByText('The passwords do not match.')).toBeInTheDocument();
    expect(screen.getByLabelText(/^confirm new password/i)).toBeInvalid();
    expect(confirm).not.toHaveBeenCalled();
  });

  it('refuses fewer than 8 characters without calling the server', async () => {
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill('1234567');
    expect(await screen.findByText('Password must be at least 8 characters')).toBeInTheDocument();
    expect(confirm).not.toHaveBeenCalled();
  });

  it('refuses more than 72 bytes without calling the server', async () => {
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill('é'.repeat(37));
    expect(await screen.findByText('Password must be at most 72 bytes')).toBeInTheDocument();
    expect(confirm).not.toHaveBeenCalled();
  });

  it('sends the token and the password, then goes to Login with a notice', async () => {
    confirm.mockResolvedValue(undefined);
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD);
    expect(await screen.findByText('at /login')).toBeInTheDocument();
    expect(confirm).toHaveBeenCalledWith(TOKEN, PASSWORD);
    expect(sessionStorage.getItem(LOGIN_NOTICE_KEY)).toMatch(new RegExp(`^${RESET_COMPLETED}@`));
  });

  it('disables the button while the request runs', async () => {
    let finish: () => void = () => {};
    confirm.mockReturnValue(new Promise<void>((resolve) => (finish = resolve)));
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD);
    expect(screen.getByRole('button', { name: 'Saving…' })).toBeDisabled();
    finish();
    await waitFor(() => expect(screen.getByText('at /login')).toBeInTheDocument());
  });

  it('shows the generic invalid message on 400 with a link to a new one, and no form', async () => {
    confirm.mockRejectedValue(httpError(400, { error: INVALID_TOKEN }));
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD);
    expect(await screen.findByText(/invalid or has expired/i)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Request a new reset link' })).toHaveAttribute('href', '/forgot-password');
    expect(screen.queryByLabelText(/^new password/i)).not.toBeInTheDocument();
  });

  it('shows a server password rule on 400 and keeps the form (the token is not used up)', async () => {
    confirm.mockRejectedValue(httpError(400, { error: 'Password must be at most 72 bytes' }));
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD);
    expect(await screen.findByText('Password must be at most 72 bytes')).toBeInTheDocument();
    expect(screen.getByLabelText(/^new password/i)).toBeInTheDocument();
    expect(screen.queryByText(/invalid or has expired/i)).not.toBeInTheDocument();
  });

  it('treats an unexpected 400 text as an invalid link, never as text from the server', async () => {
    confirm.mockRejectedValue(httpError(400, { error: 'SELECT * FROM users' }));
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD);
    expect(await screen.findByText(/invalid or has expired/i)).toBeInTheDocument();
    expect(screen.queryByText(/SELECT/)).not.toBeInTheDocument();
  });

  it('shows a rate limit message on 429 and keeps the form', async () => {
    confirm.mockRejectedValue(httpError(429));
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD);
    expect(await screen.findByRole('alert')).toHaveTextContent(/too many attempts/i);
    expect(screen.getByLabelText(/^new password/i)).toBeInTheDocument();
  });

  it('shows a retry message on a network error and keeps the form', async () => {
    confirm.mockRejectedValue(new AxiosError('Network Error', AxiosError.ERR_NETWORK));
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD);
    expect(await screen.findByRole('alert')).toHaveTextContent(/could not be reset/i);
    expect(screen.getByLabelText(/^new password/i)).toBeInTheDocument();
  });

  it('sends a 500 (password stands, sessions not ended) to Login with the check-login notice', async () => {
    confirm.mockRejectedValue(httpError(500, { error: 'Internal server error' }));
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD);
    expect(await screen.findByText('at /login')).toBeInTheDocument();
    expect(sessionStorage.getItem(LOGIN_NOTICE_KEY)).toMatch(new RegExp(`^${RESET_CHECK_LOGIN}@`));
  });

  it('never logs the token or the password', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const log = vi.spyOn(console, 'log').mockImplementation(() => {});
    confirm.mockRejectedValue(httpError(400, { error: INVALID_TOKEN }));
    renderAt(`#token=${TOKEN}`);
    await screen.findByLabelText(/^new password/i);
    await fill(PASSWORD);
    await screen.findByText(/invalid or has expired/i);
    const text = reachableText([...spy.mock.calls.flat(), ...log.mock.calls.flat()]);
    expect(text).not.toContain(TOKEN);
    expect(text).not.toContain(PASSWORD);
  });
});

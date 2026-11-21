import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AxiosError, AxiosHeaders } from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import { MemoryRouter } from 'react-router-dom';
import ProfilePage from '../index';
import { authService } from '../../../../api/client';
import { useAuth } from '../../../../context/AuthContext';
import type { AuthContextValue } from '../../../../context/AuthContext';
import type { AuthUser } from '../../../../utils/jwt';
import { reachableText } from '../../../../test/mockHttp';

vi.mock('../../../../api/client', () => ({
  authService: { changePassword: vi.fn() },
}));
vi.mock('../../../../context/AuthContext', () => ({ useAuth: vi.fn() }));

const changePassword = vi.mocked(authService.changePassword);

const CURRENT = 'old-pass-phrase';
const NEW_PASSWORD = 'a-new-pass-phrase';

const ALICE: AuthUser = {
  id: 'u1',
  username: 'alice',
  display_name: 'Alice Person',
  role: 'user',
  email: 'alice@example.test',
  auth_provider: 'local',
};

function authState(user: AuthUser | null = ALICE): AuthContextValue {
  return {
    authEnabled: true,
    isLoading: false,
    isAuthenticated: user !== null,
    user,
    role: user?.role ?? null,
    login: vi.fn(),
    logout: vi.fn(),
    loginWithOIDC: vi.fn(),
  };
}

/** An AxiosError as the auth client throws it for a non-2xx answer. */
function httpError(status: number, body: unknown, headers: Record<string, string> = {}): AxiosError {
  const config = { headers: new AxiosHeaders(), method: 'put', url: '/api/v1/auth/me/password' } as InternalAxiosRequestConfig;
  return new AxiosError(`Request failed with status code ${status}`, AxiosError.ERR_BAD_REQUEST, config, {}, {
    data: body,
    status,
    statusText: String(status),
    headers,
    config,
  });
}

function networkError(): AxiosError {
  return new AxiosError('Network Error', AxiosError.ERR_NETWORK);
}

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
  return render(
    <MemoryRouter>
      <ProfilePage />
    </MemoryRouter>,
  );
}

const field = {
  current: () => screen.getByLabelText(/^current password/i),
  next: () => screen.getByLabelText(/^new password/i),
  confirm: () => screen.getByLabelText(/^confirm new password/i),
};

async function fill(user: ReturnType<typeof userEvent.setup>, current: string, next: string, confirm = next) {
  await user.type(field.current(), current);
  await user.type(field.next(), next);
  await user.type(field.confirm(), confirm);
}

function submitButton() {
  return screen.getByRole('button', { name: /change password|saving/i });
}

describe('Account page', () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue(authState());
    changePassword.mockResolvedValue(undefined);
  });

  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
    localStorage.clear();
    sessionStorage.clear();
  });

  describe('profile', () => {
    it('shows the name, username, email, role and sign-in method from the session', () => {
      renderPage();
      expect(screen.getByRole('heading', { name: 'Account', level: 1 })).toBeInTheDocument();
      expect(screen.getByText('Alice Person')).toBeInTheDocument();
      expect(screen.getByText('alice')).toBeInTheDocument();
      expect(screen.getByText('alice@example.test')).toBeInTheDocument();
      expect(screen.getByText('user')).toBeInTheDocument();
      expect(screen.getByText('Password')).toBeInTheDocument();
    });

    it('shows no email row when the user has no email', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ ...ALICE, email: undefined }));
      renderPage();
      expect(screen.queryByText('Email')).not.toBeInTheDocument();
    });

    it('treats a missing auth_provider as a local user', () => {
      vi.mocked(useAuth).mockReturnValue(authState({ ...ALICE, auth_provider: undefined }));
      renderPage();
      expect(screen.getByText('Password')).toBeInTheDocument();
      expect(screen.getByLabelText(/^current password/i)).toBeInTheDocument();
    });

    it('links to the API keys page', () => {
      renderPage();
      expect(screen.getByRole('link', { name: /api keys/i })).toHaveAttribute('href', '/account/api-keys');
    });

    it('makes no request to read the profile', () => {
      renderPage();
      expect(changePassword).not.toHaveBeenCalled();
    });

    it('shows an error instead of the form when the session has no user', () => {
      vi.mocked(useAuth).mockReturnValue(authState(null));
      renderPage();
      expect(screen.getByRole('alert')).toHaveTextContent(/account details are not available/i);
      expect(screen.queryByLabelText(/^current password/i)).not.toBeInTheDocument();
    });
  });

  describe('single sign-on user', () => {
    const SSO: AuthUser = { ...ALICE, auth_provider: 'Okta' };

    it('shows the provider, a notice and no password form', () => {
      vi.mocked(useAuth).mockReturnValue(authState(SSO));
      renderPage();
      expect(screen.getByText('Okta')).toBeInTheDocument();
      expect(screen.getByText(/managed by your identity provider/i)).toBeInTheDocument();
      expect(screen.queryByLabelText(/password/i)).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: /change password/i })).not.toBeInTheDocument();
    });

    it('still links to the API keys page', () => {
      vi.mocked(useAuth).mockReturnValue(authState(SSO));
      renderPage();
      expect(screen.getByRole('link', { name: /api keys/i })).toBeInTheDocument();
    });
  });

  describe('form fields', () => {
    it('has labelled password fields with the right autocomplete values', () => {
      renderPage();
      expect(field.current()).toHaveAttribute('type', 'password');
      expect(field.current()).toHaveAttribute('autocomplete', 'current-password');
      expect(field.next()).toHaveAttribute('autocomplete', 'new-password');
      expect(field.confirm()).toHaveAttribute('autocomplete', 'new-password');
    });
  });

  describe('client checks (no request while one fails)', () => {
    it('asks for the current password', async () => {
      const user = userEvent.setup();
      renderPage();
      await user.type(field.next(), NEW_PASSWORD);
      await user.type(field.confirm(), NEW_PASSWORD);
      await user.click(submitButton());
      expect(await screen.findByText('Enter your current password.')).toBeInTheDocument();
      expect(field.current()).toHaveFocus();
      expect(changePassword).not.toHaveBeenCalled();
    });

    it('refuses a new password with fewer than 8 characters', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, 'short');
      await user.click(submitButton());
      expect(await screen.findByText('Password must be at least 8 characters')).toBeInTheDocument();
      expect(field.next()).toHaveFocus();
      expect(changePassword).not.toHaveBeenCalled();
    });

    it('counts characters, not bytes, for the minimum', async () => {
      const user = userEvent.setup();
      renderPage();
      // 8 characters, 16 bytes: valid.
      await fill(user, CURRENT, 'ååååååöö');
      await user.click(submitButton());
      await waitFor(() => expect(changePassword).toHaveBeenCalledWith(CURRENT, 'ååååååöö'));
    });

    it('refuses a new password over 72 bytes', async () => {
      const user = userEvent.setup();
      renderPage();
      // 40 characters of 2 bytes: 80 bytes.
      const tooLong = 'å'.repeat(40);
      await fill(user, CURRENT, tooLong);
      await user.click(submitButton());
      expect(await screen.findByText('Password must be at most 72 bytes')).toBeInTheDocument();
      expect(changePassword).not.toHaveBeenCalled();
    });

    it('refuses a confirmation that differs', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, NEW_PASSWORD, `${NEW_PASSWORD}x`);
      await user.click(submitButton());
      expect(await screen.findByText('The passwords do not match.')).toBeInTheDocument();
      expect(field.confirm()).toHaveFocus();
      expect(changePassword).not.toHaveBeenCalled();
    });

    it('refuses a new password that equals the current one', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, NEW_PASSWORD, NEW_PASSWORD);
      await user.click(submitButton());
      expect(await screen.findByText('The new password must differ from the current password')).toBeInTheDocument();
      expect(field.next()).toHaveFocus();
      expect(changePassword).not.toHaveBeenCalled();
    });

    it('announces the errors: invalid fields, described by their message, and an alert summary', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, 'short');
      await user.click(submitButton());
      const message = await screen.findByText('Password must be at least 8 characters');
      expect(field.next()).toBeInvalid();
      expect(field.next().getAttribute('aria-describedby')).toContain(message.id);
      expect(screen.getByRole('alert')).toHaveTextContent(/was not changed/i);
    });

    it('removes the errors on the next valid submit', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, 'short');
      await user.click(submitButton());
      await screen.findByText('Password must be at least 8 characters');
      await user.clear(field.next());
      await user.clear(field.confirm());
      await user.type(field.next(), NEW_PASSWORD);
      await user.type(field.confirm(), NEW_PASSWORD);
      await user.click(submitButton());
      await waitFor(() => expect(changePassword).toHaveBeenCalledTimes(1));
      expect(screen.queryByText('Password must be at least 8 characters')).not.toBeInTheDocument();
    });
  });

  describe('success', () => {
    it('sends the current and the new password once', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, NEW_PASSWORD);
      await user.click(submitButton());
      await waitFor(() => expect(changePassword).toHaveBeenCalledTimes(1));
      expect(changePassword).toHaveBeenCalledWith(CURRENT, NEW_PASSWORD);
    });

    it('clears all three fields, shows the notice and moves focus to it', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, NEW_PASSWORD);
      await user.click(submitButton());
      const notice = await screen.findByRole('status');
      expect(notice).toHaveTextContent(/password was changed/i);
      expect(notice).toHaveTextContent(/other sessions/i);
      expect(notice).toHaveTextContent(/this session stays signed in/i);
      await waitFor(() => expect(notice).toHaveFocus());
      expect(field.current()).toHaveValue('');
      expect(field.next()).toHaveValue('');
      expect(field.confirm()).toHaveValue('');
    });

    it('does not keep the password in the page, the storage or the address', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, NEW_PASSWORD);
      await user.click(submitButton());
      await screen.findByRole('status');
      expect(document.body.innerHTML).not.toContain(NEW_PASSWORD);
      expect(document.body.innerHTML).not.toContain(CURRENT);
      expect(localStorage.length).toBe(0);
      expect(sessionStorage.length).toBe(0);
      expect(window.location.href).not.toContain(NEW_PASSWORD);
      expect(JSON.stringify(window.history.state)).not.toContain(NEW_PASSWORD);
    });

    it('hides the success notice when the next attempt starts', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, NEW_PASSWORD);
      await user.click(submitButton());
      await screen.findByRole('status');
      await user.type(field.current(), CURRENT);
      await user.click(submitButton());
      await waitFor(() => expect(screen.queryByRole('status')).not.toBeInTheDocument());
    });
  });

  describe('failures', () => {
    async function submitWith(error: unknown) {
      changePassword.mockRejectedValue(error);
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, NEW_PASSWORD);
      await user.click(submitButton());
      return user;
    }

    it('401 "Current password is incorrect": a field error on the current password, which is cleared and focused', async () => {
      await submitWith(httpError(401, { error: 'Current password is incorrect' }));
      const message = await screen.findByText('Current password is incorrect');
      expect(field.current()).toBeInvalid();
      expect(field.current().getAttribute('aria-describedby')).toContain(message.id);
      await waitFor(() => expect(field.current()).toHaveFocus());
      expect(field.current()).toHaveValue('');
      // The new password and its confirmation stay, so that the user retypes one field only.
      expect(field.next()).toHaveValue(NEW_PASSWORD);
      expect(field.confirm()).toHaveValue(NEW_PASSWORD);
      expect(screen.queryByRole('status')).not.toBeInTheDocument();
    });

    it('401 for the session: tells the user to sign in again, no field error', async () => {
      await submitWith(httpError(401, { error: 'Invalid or expired token' }));
      expect(await screen.findByRole('alert')).toHaveTextContent(/session has expired.*sign in again/i);
      expect(field.current()).not.toBeInvalid();
    });

    it('400: shows the message of the server', async () => {
      await submitWith(httpError(400, { error: 'A password can only be set for a local user' }));
      expect(await screen.findByRole('alert')).toHaveTextContent('A password can only be set for a local user');
      expect(field.next()).toHaveValue(NEW_PASSWORD);
    });

    it('403: says that the password is managed elsewhere', async () => {
      await submitWith(httpError(403, { error: 'Password sign-in is disabled' }));
      expect(await screen.findByRole('alert')).toHaveTextContent(/managed (elsewhere|by your identity provider)/i);
    });

    it('429 with Retry-After: names the wait in minutes', async () => {
      await submitWith(httpError(429, { error: 'Too many login attempts' }, { 'retry-after': '900' }));
      expect(await screen.findByRole('alert')).toHaveTextContent(/too many attempts.*15 minutes/i);
    });

    it('429 with a short Retry-After: names the wait in seconds', async () => {
      await submitWith(httpError(429, { error: 'Too many login attempts' }, { 'retry-after': '1' }));
      expect(await screen.findByRole('alert')).toHaveTextContent(/1 second\b/i);
    });

    it('429 without Retry-After: asks to wait', async () => {
      await submitWith(httpError(429, { error: 'Too many login attempts' }));
      expect(await screen.findByRole('alert')).toHaveTextContent(/too many attempts.*try again later/i);
    });

    it('500: a generic message that says the password may have changed, and keeps the fields', async () => {
      await submitWith(httpError(500, { error: 'Internal server error' }));
      const alert = await screen.findByRole('alert');
      expect(alert).toHaveTextContent(/something went wrong/i);
      expect(alert).toHaveTextContent(/may have been changed/i);
      expect(field.next()).toHaveValue(NEW_PASSWORD);
    });

    it('network error: a connection message', async () => {
      await submitWith(networkError());
      expect(await screen.findByRole('alert')).toHaveTextContent(/network error/i);
    });

    it('an unknown 4xx shows a generic message, not the body', async () => {
      await submitWith(httpError(413, { error: 'internal detail xyz' }));
      const alert = await screen.findByRole('alert');
      expect(alert).not.toHaveTextContent('internal detail xyz');
      expect(alert).toHaveTextContent(/could not be changed/i);
    });

    it('logs nothing with the password', async () => {
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const logSpy = vi.spyOn(console, 'log').mockImplementation(() => {});
      await submitWith(httpError(400, { error: 'x' }));
      await screen.findByRole('alert');
      const logged = reachableText([...errorSpy.mock.calls.flat(), ...logSpy.mock.calls.flat()]);
      expect(logged).not.toContain(NEW_PASSWORD);
      expect(logged).not.toContain(CURRENT);
    });

    it('the form works again after a failure', async () => {
      const user = await submitWith(httpError(500, { error: 'Internal server error' }));
      await screen.findByRole('alert');
      changePassword.mockResolvedValue(undefined);
      await user.click(submitButton());
      expect(await screen.findByRole('status')).toBeInTheDocument();
      expect(changePassword).toHaveBeenCalledTimes(2);
    });
  });

  describe('double submit', () => {
    it('disables the button while the request runs and sends one request', async () => {
      const pending = controlled<void>();
      changePassword.mockReturnValue(pending.promise);
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, NEW_PASSWORD);
      await user.click(submitButton());
      await waitFor(() => expect(submitButton()).toBeDisabled());
      expect(submitButton()).toHaveTextContent(/saving/i);
      // A second submit event (the form is submitted by script, as the disabled button blocks the click) sends nothing.
      fireEvent.submit(field.confirm().closest('form') as HTMLFormElement);
      expect(changePassword).toHaveBeenCalledTimes(1);
      pending.resolve();
      await screen.findByRole('status');
      expect(submitButton()).toBeEnabled();
    });
  });

  describe('unsaved changes', () => {
    function beforeUnload(): Event {
      const event = new Event('beforeunload', { cancelable: true });
      window.dispatchEvent(event);
      return event;
    }

    it('asks before the tab closes while a field holds text', async () => {
      const user = userEvent.setup();
      renderPage();
      expect(beforeUnload().defaultPrevented).toBe(false);
      await user.type(field.current(), 'x');
      expect(beforeUnload().defaultPrevented).toBe(true);
    });

    it('stops asking after a success cleared the fields', async () => {
      const user = userEvent.setup();
      renderPage();
      await fill(user, CURRENT, NEW_PASSWORD);
      expect(beforeUnload().defaultPrevented).toBe(true);
      await user.click(submitButton());
      await screen.findByRole('status');
      expect(beforeUnload().defaultPrevented).toBe(false);
    });
  });
});

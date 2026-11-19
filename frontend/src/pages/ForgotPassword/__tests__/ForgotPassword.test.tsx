import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { AxiosError } from 'axios';
import type { AxiosResponse } from 'axios';
import ForgotPassword from '../index';
import { passwordResetService } from '../../../api/client';
import { NEUTRAL_RESET_MESSAGE } from '../../../utils/passwordReset';

vi.mock('../../../api/client', () => ({
  passwordResetService: {
    requestPasswordReset: vi.fn(),
    confirmPasswordReset: vi.fn(),
  },
}));

const request = vi.mocked(passwordResetService.requestPasswordReset);

/** An error as the API client rejects it for an HTTP status. */
function httpError(status: number): AxiosError {
  const response = { status, statusText: '', headers: {}, config: {}, data: { error: 'server text' } } as unknown as AxiosResponse;
  return new AxiosError(`Request failed with status code ${status}`, 'ERR_BAD_REQUEST', undefined, undefined, response);
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={['/forgot-password']}>
      <ForgotPassword />
    </MemoryRouter>,
  );
}

async function submitUsername(value: string) {
  const user = userEvent.setup();
  await user.type(screen.getByLabelText(/username/i), value);
  await user.click(screen.getByRole('button', { name: 'Send reset link' }));
}

describe('ForgotPassword page', () => {
  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('shows a username form and no password field', () => {
    renderPage();
    expect(screen.getByRole('heading', { name: 'Forgot password', level: 1 })).toBeInTheDocument();
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/password/i)).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to sign in' })).toHaveAttribute('href', '/login');
  });

  it('focuses the username field on open', () => {
    renderPage();
    expect(screen.getByLabelText(/username/i)).toHaveFocus();
  });

  it('asks for a username when it is empty, without calling the server', async () => {
    renderPage();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Send reset link' }));
    expect(screen.getByText('Enter your username.')).toBeInTheDocument();
    expect(screen.getByLabelText(/username/i)).toBeInvalid();
    expect(request).not.toHaveBeenCalled();
  });

  it('treats a blank username as empty', async () => {
    renderPage();
    await submitUsername('   ');
    expect(screen.getByText('Enter your username.')).toBeInTheDocument();
    expect(request).not.toHaveBeenCalled();
  });

  it('refuses a username over 100 bytes with the validation message, without calling the server', async () => {
    renderPage();
    await submitUsername('a'.repeat(101));
    expect(await screen.findByText(/could not be used/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/username/i)).toBeInvalid();
    expect(request).not.toHaveBeenCalled();
  });

  it('accepts a username of exactly 100 bytes', async () => {
    request.mockResolvedValue(undefined);
    renderPage();
    await submitUsername('a'.repeat(100));
    expect(await screen.findByRole('status')).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(1);
  });

  it('refuses 26 emoji (104 bytes) but accepts 25 (100 bytes)', async () => {
    request.mockResolvedValue(undefined);
    const first = renderPage();
    await submitUsername('🙂'.repeat(26));
    expect(await screen.findByText(/could not be used/i)).toBeInTheDocument();
    expect(request).not.toHaveBeenCalled();
    first.unmount();
    renderPage();
    await submitUsername('🙂'.repeat(25));
    expect(await screen.findByRole('status')).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(1);
  });

  it('shows the neutral confirmation after a 202 and sends the trimmed username', async () => {
    request.mockResolvedValue(undefined);
    renderPage();
    await submitUsername('  alice ');
    expect(await screen.findByRole('status')).toHaveTextContent(NEUTRAL_RESET_MESSAGE);
    expect(request).toHaveBeenCalledWith('alice');
    expect(screen.queryByLabelText(/username/i)).not.toBeInTheDocument();
  });

  it('moves focus to the confirmation', async () => {
    request.mockResolvedValue(undefined);
    renderPage();
    await submitUsername('alice');
    const status = await screen.findByRole('status');
    await waitFor(() => expect(status).toHaveFocus());
  });

  it.each([
    ['a network error', new AxiosError('Network Error', AxiosError.ERR_NETWORK)],
    ['a 429', httpError(429)],
    ['a 413', httpError(413)],
    ['a 500', httpError(500)],
  ])('shows the same neutral confirmation for %s', async (_label, error) => {
    request.mockRejectedValue(error);
    renderPage();
    await submitUsername('alice');
    expect(await screen.findByRole('status')).toHaveTextContent(NEUTRAL_RESET_MESSAGE);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('shows a validation message for a 400 and no confirmation', async () => {
    request.mockRejectedValue(httpError(400));
    renderPage();
    await submitUsername('a'.repeat(101));
    expect(await screen.findByText(/could not be used/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/username/i)).toBeInvalid();
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(screen.getByLabelText(/username/i)).toHaveValue('a'.repeat(101));
  });

  it('disables the button while the request runs', async () => {
    let finish: () => void = () => {};
    request.mockReturnValue(new Promise<void>((resolve) => (finish = resolve)));
    renderPage();
    await submitUsername('alice');
    expect(screen.getByRole('button', { name: 'Sending…' })).toBeDisabled();
    finish();
    expect(await screen.findByRole('status')).toBeInTheDocument();
  });

  it('gives the same text for an existing and an unknown account (no enumeration)', async () => {
    request.mockResolvedValueOnce(undefined).mockRejectedValueOnce(httpError(500));
    const first = renderPage();
    await submitUsername('alice');
    const known = (await screen.findByRole('status')).textContent;
    first.unmount();
    renderPage();
    await submitUsername('nobody-here');
    const unknown = (await screen.findByRole('status')).textContent;
    expect(known).toBe(unknown);
  });

  it('does not log the username', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    request.mockRejectedValue(httpError(500));
    renderPage();
    await submitUsername('secret-name');
    await screen.findByRole('status');
    expect(JSON.stringify(spy.mock.calls)).not.toContain('secret-name');
  });
});

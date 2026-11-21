import { isAxiosError } from 'axios';
import { describeWait, retryAfterSeconds } from '../../utils/retryAfter';

/** The one message for every credential failure: it never tells whether the user exists. */
export const INVALID_CREDENTIALS_MESSAGE = 'Invalid username or password';
export const RATE_LIMITED_MESSAGE = 'Too many sign-in attempts. Wait a few minutes and try again.';
export const SERVER_ERROR_MESSAGE = 'Sign-in failed because of a server error. Try again later.';
export const NETWORK_ERROR_MESSAGE = 'Cannot reach the server. Check your connection and try again.';
export const GENERIC_ERROR_MESSAGE = 'Sign-in failed. Try again.';

/**
 * Map a failed sign-in to a message. The server text is never shown: a 401
 * (unknown user, wrong password, disabled account) gets one generic text, and
 * other refusals (403 cross-site, 415, 400) get a generic text too.
 * @param error - The error of the login call
 * @returns The message for the error alert
 */
export function describeLoginError(error: unknown): string {
  if (!isAxiosError(error)) return GENERIC_ERROR_MESSAGE;
  const status = error.response?.status;
  if (status === undefined) return NETWORK_ERROR_MESSAGE;
  if (status === 401) return INVALID_CREDENTIALS_MESSAGE;
  if (status === 429) {
    const wait = retryAfterSeconds(error);
    return wait === undefined ? RATE_LIMITED_MESSAGE : `Too many sign-in attempts. Wait ${describeWait(wait)} and try again.`;
  }
  if (status >= 500) return SERVER_ERROR_MESSAGE;
  return GENERIC_ERROR_MESSAGE;
}

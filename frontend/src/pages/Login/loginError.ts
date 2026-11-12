import { isAxiosError } from 'axios';

/** The one message for every credential failure: it never tells whether the user exists. */
export const INVALID_CREDENTIALS_MESSAGE = 'Invalid username or password';
export const RATE_LIMITED_MESSAGE = 'Too many sign-in attempts. Wait a few minutes and try again.';
export const SERVER_ERROR_MESSAGE = 'Sign-in failed because of a server error. Try again later.';
export const NETWORK_ERROR_MESSAGE = 'Cannot reach the server. Check your connection and try again.';
export const GENERIC_ERROR_MESSAGE = 'Sign-in failed. Try again.';

/** Longest Retry-After delay that the message names, in seconds. */
const MAX_RETRY_AFTER_SECONDS = 60 * 60;

/**
 * Read the Retry-After header (seconds or an HTTP date) of an error response.
 * @param error - The thrown error (usually an AxiosError)
 * @param now - Current time in milliseconds (for tests)
 * @returns The delay in whole seconds, or undefined when absent, invalid or too long
 */
export function retryAfterSeconds(error: unknown, now = Date.now()): number | undefined {
  if (!isAxiosError(error) || !error.response) return undefined;
  const headers = error.response.headers as ({ get?: (name: string) => unknown } & Record<string, unknown>) | undefined;
  // AxiosHeaders.get is case-insensitive; a plain object has lower-case names.
  const raw: unknown = typeof headers?.get === 'function' ? headers.get('retry-after') : headers?.['retry-after'];
  if (typeof raw !== 'string' && typeof raw !== 'number') return undefined;
  const text = String(raw).trim();
  let seconds: number;
  if (/^\d+$/.test(text)) {
    seconds = Number(text);
  } else {
    const date = Date.parse(text);
    if (Number.isNaN(date)) return undefined;
    seconds = Math.ceil((date - now) / 1000);
  }
  if (seconds <= 0 || seconds > MAX_RETRY_AFTER_SECONDS) return undefined;
  return seconds;
}

/** "N seconds" or "N minutes" (rounded up). */
function describeWait(seconds: number): string {
  if (seconds < 60) return seconds === 1 ? '1 second' : `${seconds} seconds`;
  const minutes = Math.ceil(seconds / 60);
  return minutes === 1 ? '1 minute' : `${minutes} minutes`;
}

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

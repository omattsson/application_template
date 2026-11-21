import { AxiosHeaders, isAxiosError } from 'axios';

/**
 * Shared texts and helpers of the self-service password change
 * (`PUT /api/v1/auth/me/password`). The two server texts are the ones that the
 * backend sends for a wrong current password (401) and for a new password that
 * equals the current one (400, also checked in the browser).
 */

/** The 401 text of a wrong current password (a failure that counts on the account throttle). */
export const CURRENT_PASSWORD_INCORRECT_MESSAGE = 'Current password is incorrect';
/** The 400 text of a new password that equals the current one. */
export const PASSWORD_UNCHANGED_MESSAGE = 'The new password must differ from the current password';

/**
 * Read the `error` text of a JSON error body.
 * @param err - Anything that was thrown
 * @returns The text, or null when the error has no JSON body with a string `error`
 */
export function errorTextOf(err: unknown): string | null {
  const data: unknown = isAxiosError(err) ? err.response?.data : undefined;
  if (data && typeof data === 'object') {
    const text = (data as { error?: unknown }).error;
    if (typeof text === 'string') return text;
  }
  return null;
}

/**
 * Read the `Retry-After` header of a 429 answer.
 * @param err - Anything that was thrown
 * @returns The wait in whole seconds (at least 1), or null when the header is missing or is not a number of seconds
 */
export function retryAfterSeconds(err: unknown): number | null {
  if (!isAxiosError(err) || !err.response) return null;
  const value: unknown = AxiosHeaders.from(err.response.headers as AxiosHeaders).get('retry-after');
  const text = typeof value === 'string' || typeof value === 'number' ? String(value).trim() : '';
  if (!/^\d+$/.test(text)) return null;
  const seconds = Number(text);
  return Number.isSafeInteger(seconds) ? Math.max(1, seconds) : null;
}

/**
 * Say a wait in words.
 * @param seconds - The wait in seconds (at least 1)
 * @returns For example `1 second`, `45 seconds`, `15 minutes`
 */
export function describeWait(seconds: number): string {
  if (seconds < 60) return seconds === 1 ? '1 second' : `${seconds} seconds`;
  const minutes = Math.ceil(seconds / 60);
  return minutes === 1 ? '1 minute' : `${minutes} minutes`;
}

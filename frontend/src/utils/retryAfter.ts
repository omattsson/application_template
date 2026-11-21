import { isAxiosError } from 'axios';

/** Longest Retry-After delay that a message names, in seconds. */
const MAX_RETRY_AFTER_SECONDS = 60 * 60;

/**
 * Read the Retry-After header (seconds or an HTTP date) of an error response.
 * @param error - The thrown error (usually an AxiosError)
 * @param now - Current time in milliseconds (for tests)
 * @returns The delay in whole seconds, or undefined when absent, invalid, not in the future or longer than one hour
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

/**
 * Say a wait in words.
 * @param seconds - The wait in seconds (at least 1)
 * @returns For example `1 second`, `45 seconds`, `15 minutes` (minutes are rounded up)
 */
export function describeWait(seconds: number): string {
  if (seconds < 60) return seconds === 1 ? '1 second' : `${seconds} seconds`;
  const minutes = Math.ceil(seconds / 60);
  return minutes === 1 ? '1 minute' : `${minutes} minutes`;
}

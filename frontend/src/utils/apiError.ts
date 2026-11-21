import { isAxiosError } from 'axios';

const NETWORK_ERROR_MESSAGE = 'Network error. Check your connection and try again.';
const SERVER_ERROR_MESSAGE = 'Something went wrong on the server. Please try again.';
const UNKNOWN_ERROR_MESSAGE = 'An unexpected error occurred.';

/** Read a non-empty string `error` or `message` field from a JSON response body. */
function readBodyMessage(data: unknown): string | null {
  if (data && typeof data === 'object') {
    const body = data as { error?: unknown; message?: unknown };
    if (typeof body.error === 'string' && body.error) return body.error;
    if (typeof body.message === 'string' && body.message) return body.message;
  }
  return null;
}

/**
 * Turn anything that was thrown into a message that is safe to show a user.
 * The backend answers errors with `{"error": "..."}`; that text is used for
 * a 4xx response. A 5xx never shows its body, so internal detail cannot reach
 * the page.
 * @param err - The thrown value (usually an AxiosError, but any value is accepted)
 * @returns A user-facing message. Never throws and never returns an empty string.
 */
export function getApiErrorMessage(err: unknown): string {
  if (isAxiosError(err)) {
    const response = err.response;
    if (!response) return NETWORK_ERROR_MESSAGE;
    if (response.status >= 500) return SERVER_ERROR_MESSAGE;
    return readBodyMessage(response.data) ?? `Request failed with status ${response.status}`;
  }
  if (err instanceof Error && err.message) return err.message;
  return UNKNOWN_ERROR_MESSAGE;
}

/**
 * Read the `error` text of a JSON error body, as the backend sends it.
 * Unlike `getApiErrorMessage` it applies no 5xx rule and no fallback: use it
 * to compare the server text with a known one, never to show it unchecked.
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

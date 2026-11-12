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

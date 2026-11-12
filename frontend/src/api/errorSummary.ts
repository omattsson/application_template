/** The fields of an AxiosError that are safe to log. */
interface RequestErrorLike {
  isAxiosError: true;
  code?: unknown;
  config?: { method?: unknown; url?: unknown };
  response?: { status?: unknown };
}

function isRequestError(error: unknown): error is RequestErrorLike {
  return typeof error === 'object' && error !== null && (error as { isAxiosError?: unknown }).isAxiosError === true;
}

/**
 * Describe a failed request for the console without any secret. An
 * AxiosError carries the request config: the body (a login password) and the
 * headers (the bearer token). Only the method, the URL path without its query,
 * the HTTP status and the axios error code are kept. Never log the error
 * object itself.
 * @param error - Anything that was thrown
 * @returns For example `POST /api/v1/auth/login -> 401 ERR_BAD_REQUEST`
 */
export function summarizeRequestError(error: unknown): string {
  if (isRequestError(error)) {
    const method = typeof error.config?.method === 'string' ? error.config.method.toUpperCase() : 'REQUEST';
    const path = typeof error.config?.url === 'string' ? error.config.url.split(/[?#]/, 1)[0] : '';
    const status = typeof error.response?.status === 'number' ? error.response.status : 'no response';
    const code = typeof error.code === 'string' ? ` ${error.code}` : '';
    return `${method} ${path} -> ${status}${code}`;
  }
  // Errors created by this app (for example a malformed answer) carry no request data.
  if (error instanceof Error) return `${error.name}: ${error.message}`;
  return 'Unknown error';
}

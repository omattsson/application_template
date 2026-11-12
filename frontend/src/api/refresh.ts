import axios from 'axios';
import { axiosConfig } from './config';
import { applyRefreshedToken, endSession, getSession, sessionEpoch } from './session';
import { isTokenUsable } from '../utils/jwt';

/**
 * Header that the backend requires on every cookie-authenticated auth
 * endpoint (login, refresh, logout): any non-empty value, and the request
 * must not be cross-site (backend requireSameSiteRequest).
 */
export const CSRF_HEADERS = { 'X-Requested-With': 'XMLHttpRequest' } as const;

/** Web Locks name shared by all tabs of this origin. */
export const REFRESH_LOCK_NAME = 'auth-refresh';

/**
 * Client of the auth endpoints. It has no 401 interceptor (a refused refresh
 * must not trigger another refresh), sends cookies, and has NO timeout and no
 * AbortSignal: an aborted refresh can still reach the server, rotate the
 * single-use refresh token and set a cookie the browser then never stores,
 * which ends the session. A hung refresh holds the cross-tab lock until the
 * server or the network gives up; that is the lesser harm.
 */
export const authApi = axios.create({ ...axiosConfig, withCredentials: true, timeout: 0 });

/** Thrown when a refresh finishes after a login or logout started a new session. */
export class SessionChangedError extends Error {
  constructor() {
    super('Session ended or changed during the refresh');
    this.name = 'SessionChangedError';
  }
}

// In-tab single flight: concurrent callers in one tab share one promise.
let inFlight: Promise<string> | null = null;

/**
 * Check whether an error means the session is invalid.
 * @param error - Error thrown by {@link refreshAccessToken} or an API call
 * @returns True for HTTP 401 or 403; false for network errors and 5xx answers
 */
export function isAuthRejection(error: unknown): boolean {
  if (typeof error !== 'object' || error === null || !('response' in error)) return false;
  const { response } = error as { response?: { status?: unknown } };
  return response?.status === 401 || response?.status === 403;
}

/** A usable token of this tab that differs from `failed` (adopted from another tab, or refreshed meanwhile). */
function newerToken(failed: string | null): string | null {
  const current = getSession().token;
  return current && current !== failed && isTokenUsable(current) ? current : null;
}

/**
 * @param failed - The token that was refused
 * @param startedIn - {@link sessionEpoch} when the refresh was requested, before the lock wait
 */
async function refreshUnderLock(failed: string | null, startedIn: number): Promise<string> {
  // Another tab may have refreshed while this tab waited for the lock and
  // shared its token on the session channel (or a login happened here): use
  // it instead of rotating the refresh cookie again.
  const adopted = newerToken(failed);
  if (adopted) return adopted;
  // A logout (here or broadcast by another tab) or a login during the lock
  // wait: the refresh was for a session that no longer exists. Send nothing.
  if (sessionEpoch() !== startedIn) throw new SessionChangedError();

  let token: unknown;
  try {
    const { data } = await authApi.post<{ token?: unknown }>('/api/v1/auth/refresh', {}, { headers: CSRF_HEADERS });
    token = data && typeof data === 'object' ? data.token : undefined;
  } catch (error) {
    // A tab without lock support may have refreshed in parallel.
    const late = newerToken(failed);
    if (late) return late;
    // Only an explicit rejection ends the session. A network error or a 5xx
    // keeps it, so that a later request can try again.
    if (isAuthRejection(error) && sessionEpoch() === startedIn) endSession();
    throw error;
  }
  if (typeof token !== 'string' || token === '') {
    throw new Error('The refresh answer has no access token');
  }
  if (!applyRefreshedToken(token, startedIn)) throw new SessionChangedError();
  return token;
}

function hasWebLocks(): boolean {
  return typeof navigator !== 'undefined' && typeof navigator.locks?.request === 'function';
}

/**
 * Get a new access token from the httpOnly refresh cookie. Every refresh of
 * the app goes through this function.
 *
 * - In one tab, concurrent callers share one request.
 * - Across tabs, the Web Lock `auth-refresh` lets one tab at a time call the
 *   endpoint. The server accepts a rotated refresh token only once more
 *   within a short grace, so two tabs sending the same cookie at once could
 *   end the session. A tab that waited for the lock first checks whether
 *   another tab shared a newer token (session channel) and uses it; if not,
 *   it refreshes with the cookie that the other tab's rotation already
 *   replaced, which is safe.
 * - Without Web Locks only the in-tab single flight applies (the server's
 *   reuse grace then covers two tabs refreshing at the same moment).
 *
 * The request is never aborted, also not when the component that asked for
 * it unmounts.
 * @param failedToken - The token that the server refused (default: the current token). A newer usable token is returned without a request.
 * @returns The new access token (also installed in the session)
 * @throws The request error; a 401/403 has ended the session
 * @see POST /api/v1/auth/refresh
 */
export function refreshAccessToken(failedToken?: string | null): Promise<string> {
  if (getSession().mode !== 'enabled') return Promise.reject(new Error('Auth is disabled'));
  const failed = failedToken === undefined ? getSession().token : failedToken;
  const newer = newerToken(failed);
  if (newer) return Promise.resolve(newer);
  if (inFlight) return inFlight;

  // Sampled before the lock wait, so a logout while queued cancels the request.
  const startedIn = sessionEpoch();
  const run = (): Promise<string> => refreshUnderLock(failed, startedIn);
  const pending = hasWebLocks() ? navigator.locks.request(REFRESH_LOCK_NAME, run) : run();
  const shared = pending.finally(() => {
    if (inFlight === shared) inFlight = null;
  });
  inFlight = shared;
  return shared;
}

/** Forget the in-flight refresh. Tests only. */
export function resetRefreshForTesting(): void {
  inFlight = null;
}

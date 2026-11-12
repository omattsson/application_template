import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react';
import type { ReactNode } from 'react';
import { authService } from '../api/client';
import { API_BASE_URL } from '../api/config';
import { isAuthRejection, refreshAccessToken } from '../api/refresh';
import { endSession, getSession, setAuthMode, startSession, subscribeSession } from '../api/session';
import { useUIConfig } from './UIConfigContext';
import { tokenExpiresAt, tokenIssuedAt } from '../utils/jwt';
import type { AuthUser, Role } from '../utils/jwt';
import { safeReturnPath } from '../utils/returnPath';
import { redirectBrowser } from '../utils/navigation';

export interface AuthContextValue {
  /** The backend has AUTH_ENABLED=true (false while loading and with auth off). */
  authEnabled: boolean;
  /** True until the UI config is read and, with auth on, the session restore finished. */
  isLoading: boolean;
  isAuthenticated: boolean;
  /** Display user (decoded hints; the backend decides every permission). */
  user: AuthUser | null;
  role: Role | null;
  /** Sign in with a password; rejects with the request error. */
  login: (username: string, password: string) => Promise<void>;
  /** End the session here, in the other tabs and on the server. Never rejects. */
  logout: () => Promise<void>;
  /** Leave for the identity provider; after the callback the app returns to `next` (validated). */
  loginWithOIDC: (next?: string | null) => void;
}

/** sessionStorage key of the page to return to after the OIDC login (a path, never a token). */
export const OIDC_RETURN_PATH_KEY = 'auth-return-path';

/** Refresh this long before expiry (at most half the token lifetime). */
const REFRESH_MARGIN_MS = 60_000;
/** Never schedule a refresh sooner than this (protects against a refresh loop). */
const MIN_REFRESH_DELAY_MS = 1_000;
/** Largest setTimeout delay browsers support. */
const MAX_TIMER_MS = 2_147_483_647;
/** Retry delay after a refresh failed for a reason other than a rejection (network, 5xx). */
const REFRESH_RETRY_MS = 30_000;

/**
 * Delay until the scheduled refresh of a token that was just received.
 *
 * The delay comes from the token lifetime (exp - iat), not from the client
 * clock, so a client clock that is off cannot make every token look expired
 * and cause a refresh loop. Without iat the client clock is used.
 * @param token - The access token
 * @param now - Current time in milliseconds (for tests)
 * @returns Milliseconds, or null when the token has no exp
 */
export function refreshDelayMs(token: string, now = Date.now()): number | null {
  const exp = tokenExpiresAt(token);
  if (exp === null) return null;
  const iat = tokenIssuedAt(token);
  let delay: number;
  if (iat !== null && iat < exp) {
    const lifetime = exp - iat;
    delay = lifetime - Math.min(REFRESH_MARGIN_MS, lifetime / 2);
  } else {
    delay = exp - now - REFRESH_MARGIN_MS;
  }
  return Math.min(Math.max(delay, MIN_REFRESH_DELAY_MS), MAX_TIMER_MS);
}

/**
 * Read the stored OIDC return path. The value is untrusted (any script of the
 * origin can write it) and is validated here.
 * @returns A validated same-origin path (default `/`)
 */
export function readOidcReturnPath(): string {
  try {
    return safeReturnPath(sessionStorage.getItem(OIDC_RETURN_PATH_KEY));
  } catch {
    return '/';
  }
}

/** Remove the stored OIDC return path. */
export function clearOidcReturnPath(): void {
  try {
    sessionStorage.removeItem(OIDC_RETURN_PATH_KEY);
  } catch {
    // Storage blocked: nothing stored.
  }
}

const AuthContext = createContext<AuthContextValue | undefined>(undefined);

/**
 * Optional authentication. With auth off (`auth_enabled` false) it does
 * nothing: no refresh, no token, every page open. With auth on it restores the
 * session from the refresh cookie once, keeps the access token in memory
 * (api/session), refreshes it before expiry and offers login and logout.
 */
export function AuthProvider({ children }: { children: ReactNode }) {
  const { config, loading: configLoading } = useUIConfig();
  const session = useSyncExternalStore(subscribeSession, getSession);
  const [restoring, setRestoring] = useState(true);
  const restoreStarted = useRef(false);

  // Initial session restore. With auth on this is the one refresh after a
  // reload or after the OIDC redirect (/auth/callback waits for it).
  useEffect(() => {
    if (configLoading || restoreStarted.current) return;
    restoreStarted.current = true;
    if (!config.auth_enabled) {
      setAuthMode('disabled');
      setRestoring(false);
      return;
    }
    setAuthMode('enabled');
    refreshAccessToken()
      .catch(() => {
        // No cookie or refused: anonymous. A network error leaves the visitor
        // anonymous too; the login page can retry.
      })
      .finally(() => setRestoring(false));
  }, [configLoading, config.auth_enabled]);

  // Refresh before the token expires, so the API and the WebSocket (closed by
  // the server at token expiry) always have a valid token. Clearing the timer
  // never aborts a refresh that already started.
  useEffect(() => {
    const token = session.token;
    if (session.mode !== 'enabled' || !token) return;
    const delay = refreshDelayMs(token);
    if (delay === null) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const attempt = () => {
      refreshAccessToken(token).catch((error: unknown) => {
        // A rejection ended the session. Otherwise try again later.
        if (!cancelled && !isAuthRejection(error)) timer = setTimeout(attempt, REFRESH_RETRY_MS);
      });
    };
    timer = setTimeout(attempt, delay);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [session.mode, session.token]);

  const login = useCallback(async (username: string, password: string) => {
    const { token, user } = await authService.login(username, password);
    startSession(token, user);
  }, []);

  const logout = useCallback(async () => {
    const token = getSession().token;
    // Local state first: even if the server call fails, this browser is signed out.
    endSession({ broadcast: true });
    try {
      await authService.logout(token);
    } catch {
      // Best effort; the access token expires on its own.
    }
  }, []);

  const loginWithOIDC = useCallback((next?: string | null) => {
    try {
      sessionStorage.setItem(OIDC_RETURN_PATH_KEY, safeReturnPath(next));
    } catch {
      // Storage blocked: the callback goes home.
    }
    redirectBrowser(`${API_BASE_URL}/api/v1/auth/oidc/login`);
  }, []);

  const authEnabled = !configLoading && config.auth_enabled;
  const isLoading = configLoading || restoring;
  const isAuthenticated = authEnabled && session.mode === 'enabled' && session.token !== null;
  const user = isAuthenticated ? session.user : null;

  const value = useMemo<AuthContextValue>(
    () => ({
      authEnabled,
      isLoading,
      isAuthenticated,
      user,
      role: user?.role ?? null,
      login,
      logout,
      loginWithOIDC,
    }),
    [authEnabled, isLoading, isAuthenticated, user, login, logout, loginWithOIDC],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

/**
 * The auth state and actions.
 * @returns The auth context value
 */
export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (ctx === undefined) throw new Error('useAuth must be used within <AuthProvider>');
  return ctx;
}

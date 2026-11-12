import { isTokenUsable, userFromToken } from '../utils/jwt';
import type { AuthUser } from '../utils/jwt';

/**
 * In-memory session of this tab: the auth mode of the backend and the access
 * token. The access token is never written to localStorage, sessionStorage or
 * a cookie (an XSS payload could read those after the page is gone); a reload
 * restores it from the httpOnly refresh cookie (POST /api/v1/auth/refresh).
 *
 * The tabs of one browser share one session (the refresh cookie is shared):
 * a new token (login or refresh) and a logout are announced to the other tabs
 * on a BroadcastChannel. A tab adopts a token from another tab instead of
 * refreshing again, which also matters for the cross-tab refresh lock (see
 * refresh.ts).
 */

/** `unknown` until GET /api/v1/ui-config answered. */
export type AuthMode = 'unknown' | 'disabled' | 'enabled';

export interface SessionSnapshot {
  mode: AuthMode;
  /** The access token; always null unless mode is `enabled`. */
  token: string | null;
  /** Display user of the token (login response or decoded claims); null when anonymous or not decodable. */
  user: AuthUser | null;
}

/** Name of the BroadcastChannel shared by the tabs of this origin. */
export const SESSION_CHANNEL_NAME = 'auth-session';

/** Messages on the session channel. */
type SessionMessage = { type: 'token'; token: string } | { type: 'logout' };

const ANONYMOUS: SessionSnapshot = { mode: 'unknown', token: null, user: null };

let snapshot: SessionSnapshot = ANONYMOUS;
/**
 * Incremented by every login and logout. A refresh that started in an older
 * epoch must not install its token (for example a refresh that finishes after
 * the user logged out).
 */
let epoch = 0;
let channel: BroadcastChannel | null = null;
const listeners = new Set<() => void>();

function emit(next: SessionSnapshot): void {
  snapshot = next;
  for (const listener of listeners) listener();
}

function parseMessage(data: unknown): SessionMessage | null {
  if (!data || typeof data !== 'object') return null;
  const msg = data as { type?: unknown; token?: unknown };
  if (msg.type === 'logout') return { type: 'logout' };
  if (msg.type === 'token' && typeof msg.token === 'string') return { type: 'token', token: msg.token };
  return null;
}

function onChannelMessage(event: MessageEvent): void {
  if (snapshot.mode !== 'enabled') return;
  const msg = parseMessage(event.data);
  if (!msg) return;
  if (msg.type === 'logout') {
    clearLocal();
    return;
  }
  // Only a usable token; the same user decoding as a local refresh.
  if (!isTokenUsable(msg.token) || msg.token === snapshot.token) return;
  emit({ ...snapshot, token: msg.token, user: userFromToken(msg.token) });
}

function openChannel(): void {
  if (channel || typeof BroadcastChannel === 'undefined') return;
  try {
    channel = new BroadcastChannel(SESSION_CHANNEL_NAME);
    channel.onmessage = onChannelMessage;
  } catch {
    channel = null;
  }
}

function broadcast(msg: SessionMessage): void {
  try {
    channel?.postMessage(msg);
  } catch {
    // Best effort: the other tabs then find out at their next refresh.
  }
}

function clearLocal(): void {
  epoch += 1;
  emit({ ...snapshot, token: null, user: null });
}

/** The current session. The same object until something changes (for useSyncExternalStore). */
export function getSession(): SessionSnapshot {
  return snapshot;
}

/**
 * Subscribe to session changes.
 * @param listener - Called after every change
 * @returns A function that removes the listener
 */
export function subscribeSession(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/**
 * Set the auth mode from the UI config. Leaving `enabled` drops the token.
 * @param mode - `enabled` when the backend has AUTH_ENABLED=true
 */
export function setAuthMode(mode: AuthMode): void {
  if (mode === 'enabled') openChannel();
  if (mode === snapshot.mode) return;
  emit(mode === 'enabled' ? { ...snapshot, mode } : { mode, token: null, user: null });
}

/**
 * The access token to send, or null (auth off, or anonymous).
 * @returns The bearer token of this tab
 */
export function getAccessToken(): string | null {
  return snapshot.mode === 'enabled' ? snapshot.token : null;
}

/** The current epoch, for {@link applyRefreshedToken}. */
export function sessionEpoch(): number {
  return epoch;
}

/**
 * Start a new session after a login: keep the token in memory and tell the
 * other tabs. Starts a new epoch, so an older refresh still in flight cannot
 * replace this token.
 * @param token - Access token from POST /api/v1/auth/login
 * @param user - The user of the login response; null to decode it from the token
 */
export function startSession(token: string, user: AuthUser | null): void {
  epoch += 1;
  emit({ ...snapshot, token, user: user ?? userFromToken(token) });
  broadcast({ type: 'token', token });
}

/**
 * Install a refreshed token, unless a login or logout happened since the
 * refresh started.
 * @param token - Access token from POST /api/v1/auth/refresh
 * @param startedIn - {@link sessionEpoch} when the refresh started
 * @returns False when the token was discarded
 */
export function applyRefreshedToken(token: string, startedIn: number): boolean {
  if (startedIn !== epoch || snapshot.mode !== 'enabled') return false;
  emit({ ...snapshot, token, user: userFromToken(token) });
  broadcast({ type: 'token', token });
  return true;
}

/**
 * End the session of this tab: forget the token and the user.
 * @param options.broadcast - Also log out the other tabs (an explicit logout). A refused refresh does not: the other tabs find out themselves.
 */
export function endSession(options: { broadcast?: boolean } = {}): void {
  clearLocal();
  if (options.broadcast) broadcast({ type: 'logout' });
}

/** Reset all module state. Tests only. */
export function resetSessionForTesting(): void {
  channel?.close();
  channel = null;
  epoch = 0;
  snapshot = ANONYMOUS;
}

/**
 * Messages the login page shows when the app sent the user there on purpose.
 *
 * The notice travels in sessionStorage, not in the router state: ending the
 * session makes the route guard redirect to the login page itself, and that
 * redirect replaces the history entry (and its state). Like the OIDC return
 * path (`auth-return-path`) it is a one-shot value: `takeLoginNotice` reads
 * and removes it, so a reload or a later visit shows nothing. What is stored
 * is a key of the closed list below plus the time, never text, and nothing
 * from the URL is ever shown.
 */
export const OWN_ACCOUNT_CHANGED = 'own-account-changed';

/** sessionStorage key of the pending notice. */
export const LOGIN_NOTICE_KEY = 'auth-login-notice';

/** A notice older than this is dropped (the user never reached the login page). */
const MAX_NOTICE_AGE_MS = 60_000;

const NOTICES: Readonly<Record<string, string>> = {
  [OWN_ACCOUNT_CHANGED]:
    'Your session ended because your own account was changed (role, status or deletion). Sign in again if you still have access.',
};

/** The text of a known notice key, or null. */
export function loginNoticeText(key: string): string | null {
  return Object.prototype.hasOwnProperty.call(NOTICES, key) ? NOTICES[key] : null;
}

/**
 * Leave a notice for the login page. Call it before the session ends.
 * @param key - A key of the closed list (for example {@link OWN_ACCOUNT_CHANGED})
 * @param now - Current time in milliseconds (for tests)
 */
export function rememberLoginNotice(key: string, now = Date.now()): void {
  try {
    sessionStorage.setItem(LOGIN_NOTICE_KEY, `${key}@${now}`);
  } catch {
    // Storage blocked: the login page shows no notice.
  }
}

/**
 * Read and remove the pending notice.
 * @param now - Current time in milliseconds (for tests)
 * @returns The text of a known, fresh notice, or null for anything else
 */
export function takeLoginNotice(now = Date.now()): string | null {
  let raw: string | null;
  try {
    raw = sessionStorage.getItem(LOGIN_NOTICE_KEY);
    if (raw !== null) sessionStorage.removeItem(LOGIN_NOTICE_KEY);
  } catch {
    return null;
  }
  if (raw === null) return null;
  const at = raw.lastIndexOf('@');
  if (at < 0) return null;
  const stamp = Number(raw.slice(at + 1));
  if (!Number.isFinite(stamp) || now - stamp > MAX_NOTICE_AGE_MS || stamp - now > MAX_NOTICE_AGE_MS) return null;
  return loginNoticeText(raw.slice(0, at));
}

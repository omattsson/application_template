/**
 * Messages the login page shows when the app sent the user there on purpose.
 * The app passes a key in the router state (`navigate('/login', { state: { notice: KEY } })`),
 * never text, and never through the URL: the key is looked up in this closed
 * list, so nothing a link or a query string carries can be shown.
 */
export const OWN_ACCOUNT_CHANGED = 'own-account-changed';

const NOTICES: Readonly<Record<string, string>> = {
  [OWN_ACCOUNT_CHANGED]:
    'Your session ended because your own account was changed (role, status or deletion). Sign in again if you still have access.',
};

/** The text of a known notice key, or null. */
export function loginNoticeText(key: string): string | null {
  return Object.prototype.hasOwnProperty.call(NOTICES, key) ? NOTICES[key] : null;
}

/**
 * Read the notice out of a router location state.
 * @param state - `location.state` (untrusted shape)
 * @returns The text of a known notice, or null for anything else
 */
export function loginNoticeFrom(state: unknown): string | null {
  if (typeof state !== 'object' || state === null) return null;
  const key = (state as { notice?: unknown }).notice;
  return typeof key === 'string' ? loginNoticeText(key) : null;
}

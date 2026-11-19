/**
 * Shared rules and helpers of the password reset pages.
 *
 * The token comes from the URL fragment of the mail link
 * (`<PASSWORD_RESET_URL>/reset-password#token=...`). A fragment never reaches
 * the server, so it is read here and removed from the address bar at once. It
 * is never stored, logged or shown.
 */

/** Shortest password the server accepts, in characters (the server counts runes). */
export const PASSWORD_MIN_CHARACTERS = 8;
/** Longest password the server accepts, in UTF-8 bytes. */
export const PASSWORD_MAX_BYTES = 72;

/** The server's own texts for the two password rules (shown as they are: they hold no secret). */
export const PASSWORD_TOO_SHORT_MESSAGE = 'Password must be at least 8 characters';
export const PASSWORD_TOO_LONG_MESSAGE = 'Password must be at most 72 bytes';
export const PASSWORD_MISMATCH_MESSAGE = 'The passwords do not match.';

/** The one answer of the request form: it never tells whether the account exists. */
export const NEUTRAL_RESET_MESSAGE =
  'If the account exists and can receive mail, a reset link has been sent. Check your inbox and spam folder. The link is valid for a limited time.';

/** Shape of a token of the mail link: 32 random bytes, URL-safe base64 without padding. */
const RESET_TOKEN_PATTERN = /^[A-Za-z0-9_-]{43}$/;

/**
 * Check a new password with the server's rules.
 * @param password - The password as typed
 * @returns The message for the first broken rule, or null when it is valid
 */
export function passwordProblem(password: string): string | null {
  if (Array.from(password).length < PASSWORD_MIN_CHARACTERS) return PASSWORD_TOO_SHORT_MESSAGE;
  if (new TextEncoder().encode(password).length > PASSWORD_MAX_BYTES) return PASSWORD_TOO_LONG_MESSAGE;
  return null;
}

/**
 * Read the reset token from a URL fragment.
 * @param hash - `window.location.hash` (with or without the leading `#`)
 * @returns The token, or null when the fragment has no well-formed token
 */
export function readResetToken(hash: string): string | null {
  const token = new URLSearchParams(hash.replace(/^#/, '')).get('token');
  return token !== null && RESET_TOKEN_PATTERN.test(token) ? token : null;
}

/**
 * Remove the fragment from the address bar. The history state is kept, so the
 * router's own history index is not lost.
 */
export function removeTokenFromAddress(): void {
  const { pathname, search, hash } = window.location;
  if (hash === '') return;
  window.history.replaceState(window.history.state, '', `${pathname}${search}`);
}

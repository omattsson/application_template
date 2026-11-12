/** Longest return path that is kept (longer values fall back). */
const MAX_RETURN_PATH_LENGTH = 2048;

/** Paths that must never be a return target: going back there would loop. */
const AUTH_PAGES = ['/login', '/auth/callback'];

/** Placeholder origin to parse a relative path. Only used to check that parsing keeps the path relative. */
const PROBE_ORIGIN = 'http://return-path.invalid';

/**
 * Validate a return path (the `next` parameter or the stored OIDC return
 * path) so that it cannot send the user to another site (open redirect).
 *
 * Accepted: a relative path that starts with exactly one `/`, has no
 * backslash and no control or whitespace character, and stays on this
 * origin after URL parsing. The login and callback pages are refused.
 * @param raw - The untrusted value
 * @param fallback - Returned for any refused value (default `/`)
 * @returns A same-origin path with its query and fragment
 */
export function safeReturnPath(raw: string | null | undefined, fallback = '/'): string {
  if (typeof raw !== 'string' || raw.length === 0 || raw.length > MAX_RETURN_PATH_LENGTH) return fallback;
  if (!raw.startsWith('/') || raw.startsWith('//')) return fallback;
  // Backslashes are read as slashes by browsers ("/\evil.com" is "//evil.com").
  // Control characters and whitespace are dropped by the URL parser, which
  // could join two slashes ("/\t/evil.com").
  if (/[\\\s\p{Cc}]/u.test(raw)) return fallback;
  let parsed: URL;
  try {
    parsed = new URL(raw, PROBE_ORIGIN);
  } catch {
    return fallback;
  }
  if (parsed.origin !== PROBE_ORIGIN) return fallback;
  if (AUTH_PAGES.includes(parsed.pathname.replace(/\/+$/, '') || '/')) return fallback;
  return `${parsed.pathname}${parsed.search}${parsed.hash}`;
}

/** The parts of a router location that make up a return path. */
export interface LocationLike {
  pathname: string;
  search: string;
  hash: string;
}

/**
 * Build the login URL that brings the user back to `location` after sign-in.
 * @param location - The page the user wanted
 * @returns `/login`, with `?next=` unless the target is the home page
 */
export function loginPathFor(location: LocationLike): string {
  const target = safeReturnPath(`${location.pathname}${location.search}${location.hash}`);
  return target === '/' ? '/login' : `/login?next=${encodeURIComponent(target)}`;
}

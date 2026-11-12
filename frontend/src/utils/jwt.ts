/**
 * Read-only helpers for the JWT access token. The SPA never verifies the
 * signature: the decoded claims are display and role hints only. The backend
 * checks every request; never use these values for an authorization decision.
 */

/** Roles of the backend (models.RoleUser, RoleDevOps, RoleAdmin). */
export type Role = 'user' | 'devops' | 'admin';

export const ROLES: readonly Role[] = ['user', 'devops', 'admin'];

/** Claims of the access token (backend middleware.Claims). All are optional: a token may be malformed. */
export interface JwtPayload {
  user_id?: unknown;
  username?: unknown;
  display_name?: unknown;
  role?: unknown;
  email?: unknown;
  auth_provider?: unknown;
  sid?: unknown;
  exp?: unknown;
  iat?: unknown;
  [claim: string]: unknown;
}

/** The signed-in user as the UI shows it. */
export interface AuthUser {
  id: string;
  username: string;
  display_name: string;
  /** Null when the role is missing or not a known role (the UI then grants nothing). */
  role: Role | null;
  email?: string;
  auth_provider?: string;
}

/**
 * Narrow a value to a known role.
 * @param value - Any value (for example a claim)
 * @returns The role, or null when the value is not a known role
 */
export function toRole(value: unknown): Role | null {
  return typeof value === 'string' && (ROLES as readonly string[]).includes(value) ? (value as Role) : null;
}

/** Decode one base64url segment to a UTF-8 string. Throws on invalid input. */
function decodeSegment(segment: string): string {
  if (!/^[A-Za-z0-9_-]+={0,2}$/.test(segment)) throw new Error('not base64url');
  const base64 = segment.replace(/-/g, '+').replace(/_/g, '/');
  const padded = base64.padEnd(base64.length + ((4 - (base64.length % 4)) % 4), '=');
  const binary = atob(padded);
  const bytes = Uint8Array.from(binary, (ch) => ch.charCodeAt(0));
  return new TextDecoder('utf-8', { fatal: true }).decode(bytes);
}

/**
 * Decode the payload of a JWT without verifying the signature.
 * @param token - Encoded JWT (header.payload.signature)
 * @returns The claims object, or null for anything that is not a JWT with a JSON object payload. Never throws.
 */
export function decodeJwtPayload(token: string): JwtPayload | null {
  if (typeof token !== 'string') return null;
  const parts = token.split('.');
  if (parts.length !== 3 || !parts[1]) return null;
  try {
    const parsed: unknown = JSON.parse(decodeSegment(parts[1]));
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
    return parsed as JwtPayload;
  } catch {
    return null;
  }
}

/** Read a numeric claim (seconds) in milliseconds, or null. */
function claimMs(payload: JwtPayload | null, claim: 'exp' | 'iat'): number | null {
  const value = payload?.[claim];
  return typeof value === 'number' && Number.isFinite(value) ? value * 1000 : null;
}

/**
 * Expiry of a token.
 * @param token - Encoded JWT
 * @returns The `exp` claim in Unix milliseconds, or null when it is missing or the token is malformed
 */
export function tokenExpiresAt(token: string): number | null {
  return claimMs(decodeJwtPayload(token), 'exp');
}

/**
 * Issue time of a token.
 * @param token - Encoded JWT
 * @returns The `iat` claim in Unix milliseconds, or null
 */
export function tokenIssuedAt(token: string): number | null {
  return claimMs(decodeJwtPayload(token), 'iat');
}

/**
 * Check whether a token decodes and is not past its `exp` (by the client clock).
 * @param token - Encoded JWT, or null
 * @param now - Current time in milliseconds (for tests)
 * @returns True when the token can be sent to the API
 */
export function isTokenUsable(token: string | null | undefined, now = Date.now()): token is string {
  if (!token) return false;
  const exp = tokenExpiresAt(token);
  return exp !== null && now < exp;
}

/**
 * Build the display user of a token.
 * @param token - Encoded JWT
 * @returns The user, or null when the token is malformed or has no string `user_id` and `username`
 */
export function userFromToken(token: string): AuthUser | null {
  const p = decodeJwtPayload(token);
  if (!p || typeof p.user_id !== 'string' || typeof p.username !== 'string' || !p.username) return null;
  const user: AuthUser = {
    id: p.user_id,
    username: p.username,
    display_name: typeof p.display_name === 'string' && p.display_name ? p.display_name : p.username,
    role: toRole(p.role),
  };
  if (typeof p.email === 'string' && p.email) user.email = p.email;
  if (typeof p.auth_provider === 'string' && p.auth_provider) user.auth_provider = p.auth_provider;
  return user;
}

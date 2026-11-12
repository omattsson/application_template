import { describe, it, expect, afterEach, vi } from 'vitest';
import { decodeJwtPayload, isTokenUsable, tokenExpiresAt, userFromToken } from '../jwt';
import { makeJwt, tokenFor } from '../../test/mockHttp';

describe('decodeJwtPayload', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.clearAllMocks();
  });

  it('decodes the claims of a base64url token', () => {
    const token = makeJwt({ user_id: 'u1', username: 'alice', role: 'admin', exp: 2000000000 });
    expect(decodeJwtPayload(token)).toEqual({ user_id: 'u1', username: 'alice', role: 'admin', exp: 2000000000 });
  });

  it('decodes UTF-8 text in claims', () => {
    const token = makeJwt({ username: 'bjorn', display_name: 'Björn Ångström', exp: 2000000000 });
    expect(decodeJwtPayload(token)?.display_name).toBe('Björn Ångström');
  });

  it.each([
    ['empty string', ''],
    ['one part', 'abc'],
    ['two parts', 'abc.def'],
    ['invalid base64', 'a.!!!.c'],
    ['payload is not JSON', `a.${btoa('not json')}.c`],
    ['payload is an array', `a.${btoa('[1,2]')}.c`],
    ['payload is a number', `a.${btoa('42')}.c`],
    ['payload is null', `a.${btoa('null')}.c`],
  ])('returns null for a malformed token (%s) without throwing', (_name, token) => {
    expect(() => decodeJwtPayload(token)).not.toThrow();
    expect(decodeJwtPayload(token)).toBeNull();
  });

  it('returns null for a value that is not a string', () => {
    expect(decodeJwtPayload(undefined as unknown as string)).toBeNull();
    expect(decodeJwtPayload(null as unknown as string)).toBeNull();
  });
});

describe('tokenExpiresAt and isTokenUsable', () => {
  it('reads exp in milliseconds', () => {
    expect(tokenExpiresAt(makeJwt({ exp: 1700000000 }))).toBe(1700000000000);
  });

  it('returns null when exp is missing or not a number', () => {
    expect(tokenExpiresAt(makeJwt({}))).toBeNull();
    expect(tokenExpiresAt(makeJwt({ exp: 'soon' }))).toBeNull();
    expect(tokenExpiresAt('garbage')).toBeNull();
  });

  it('accepts an unexpired token and rejects expired, malformed and empty ones', () => {
    expect(isTokenUsable(tokenFor('alice', 'user', 600))).toBe(true);
    expect(isTokenUsable(tokenFor('alice', 'user', -10))).toBe(false);
    expect(isTokenUsable('garbage')).toBe(false);
    expect(isTokenUsable(null)).toBe(false);
    expect(isTokenUsable(makeJwt({ username: 'no-exp' }))).toBe(false);
  });
});

describe('userFromToken', () => {
  it('builds a display user from the claims', () => {
    const token = makeJwt({
      user_id: 'u1',
      username: 'alice',
      display_name: 'Alice A',
      role: 'devops',
      email: 'alice@example.com',
      auth_provider: 'oidc',
      exp: 2000000000,
    });
    expect(userFromToken(token)).toEqual({
      id: 'u1',
      username: 'alice',
      display_name: 'Alice A',
      role: 'devops',
      email: 'alice@example.com',
      auth_provider: 'oidc',
    });
  });

  it('falls back to the username for the display name and drops an unknown role', () => {
    const user = userFromToken(makeJwt({ user_id: 'u2', username: 'bob', role: 'superuser', exp: 2000000000 }));
    expect(user?.display_name).toBe('bob');
    expect(user?.role).toBeNull();
  });

  it('returns null for a malformed token or claims of the wrong type', () => {
    expect(userFromToken('garbage')).toBeNull();
    expect(userFromToken(makeJwt({ user_id: 7, username: 'x' }))).toBeNull();
    expect(userFromToken(makeJwt({ user_id: 'u3' }))).toBeNull();
  });
});

import { describe, it, expect } from 'vitest';
import { decideRun, isStrict } from '../../e2e/skipDecision';
import type { DecisionInput } from '../../e2e/skipDecision';

const AUTH_ON = { kind: 'responded', authEnabled: true } as const;
const AUTH_OFF = { kind: 'responded', authEnabled: false } as const;
const UNREACHABLE = { kind: 'unreachable', reason: 'connect ECONNREFUSED' } as const;

const ADMIN_PAGE = { needsAuth: true, needsSignIn: true, signInWhenAuthOn: true };
const LOGIN_PAGE = { needsAuth: true, needsSignIn: false, signInWhenAuthOn: false };
const ITEMS_PAGE = { needsAuth: false, needsSignIn: false, signInWhenAuthOn: true };

function input(overrides: Partial<DecisionInput>): DecisionInput {
  return { backend: AUTH_ON, ...ADMIN_PAGE, hasPassword: true, strict: false, ...overrides };
}

describe('isStrict', () => {
  it('is on for 1 only', () => {
    expect(isStrict({ A11Y_REQUIRE_AUTH: '1' })).toBe(true);
    expect(isStrict({ A11Y_REQUIRE_AUTH: '0' })).toBe(false);
    expect(isStrict({ A11Y_REQUIRE_AUTH: '' })).toBe(false);
    expect(isStrict({})).toBe(false);
  });
});

describe('decideRun', () => {
  it('runs a signed-in page with auth on and a password', () => {
    expect(decideRun(input({}))).toEqual({ action: 'run', signIn: true });
  });

  it('runs the login page without a sign-in', () => {
    expect(decideRun(input({ ...LOGIN_PAGE, hasPassword: false }))).toEqual({ action: 'run', signIn: false });
  });

  it('runs Items without a sign-in when auth is off, with one when auth is on', () => {
    expect(decideRun(input({ ...ITEMS_PAGE, backend: AUTH_OFF, hasPassword: false }))).toEqual({ action: 'run', signIn: false });
    expect(decideRun(input({ ...ITEMS_PAGE }))).toEqual({ action: 'run', signIn: true });
  });

  describe('without the strict switch', () => {
    it('skips with a warning when the backend is unreachable, naming the reason', () => {
      const d = decideRun(input({ backend: UNREACHABLE }));
      expect(d).toMatchObject({ action: 'skip', warn: true });
      expect(d.action === 'skip' && d.reason).toContain('ECONNREFUSED');
    });

    it('skips a page that needs auth when auth is off', () => {
      expect(decideRun(input({ backend: AUTH_OFF }))).toMatchObject({ action: 'skip' });
      expect(decideRun(input({ backend: AUTH_OFF, ...LOGIN_PAGE }))).toMatchObject({ action: 'skip' });
    });

    it('skips a signed-in page without ADMIN_PASSWORD, also Items with auth on', () => {
      expect(decideRun(input({ hasPassword: false }))).toMatchObject({ action: 'skip' });
      expect(decideRun(input({ ...ITEMS_PAGE, hasPassword: false }))).toMatchObject({ action: 'skip' });
    });

    it('always warns on a skip', () => {
      for (const d of [
        decideRun(input({ backend: UNREACHABLE })),
        decideRun(input({ backend: AUTH_OFF })),
        decideRun(input({ hasPassword: false })),
      ]) {
        expect(d).toMatchObject({ action: 'skip', warn: true });
      }
    });
  });

  describe('with the strict switch', () => {
    it('fails when the backend is unreachable, for every page', () => {
      for (const page of [ADMIN_PAGE, LOGIN_PAGE, ITEMS_PAGE]) {
        expect(decideRun(input({ ...page, backend: UNREACHABLE, strict: true }))).toMatchObject({ action: 'fail' });
      }
    });

    it('fails when auth is off for a page that needs auth', () => {
      const d = decideRun(input({ backend: AUTH_OFF, strict: true }));
      expect(d).toMatchObject({ action: 'fail' });
      expect(d.action === 'fail' && d.reason).toContain('AUTH_ENABLED');
    });

    it('fails when ADMIN_PASSWORD is missing', () => {
      const d = decideRun(input({ hasPassword: false, strict: true }));
      expect(d).toMatchObject({ action: 'fail' });
      expect(d.action === 'fail' && d.reason).toContain('ADMIN_PASSWORD');
    });

    it('still runs Items with auth off (it does not need auth)', () => {
      expect(decideRun(input({ ...ITEMS_PAGE, backend: AUTH_OFF, strict: true }))).toEqual({ action: 'run', signIn: false });
    });

    it('runs when everything is in place', () => {
      expect(decideRun(input({ strict: true }))).toEqual({ action: 'run', signIn: true });
    });
  });
});

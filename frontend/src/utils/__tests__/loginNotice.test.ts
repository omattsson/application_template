import { describe, it, expect } from 'vitest';
import { OWN_ACCOUNT_CHANGED, loginNoticeFrom, loginNoticeText } from '../loginNotice';

describe('loginNotice', () => {
  it('returns the text of a known notice key from the router state', () => {
    expect(loginNoticeFrom({ notice: OWN_ACCOUNT_CHANGED })).toBe(loginNoticeText(OWN_ACCOUNT_CHANGED));
    expect(loginNoticeText(OWN_ACCOUNT_CHANGED)).toMatch(/your own account/i);
  });

  it.each([
    ['no state', null],
    ['undefined', undefined],
    ['a string', 'own-account-changed'],
    ['an object without notice', {}],
    ['an unknown key', { notice: 'something-else' }],
    ['free text', { notice: '<b>Call 555-0100</b>' }],
    ['a non-string key', { notice: 42 }],
    ['an inherited property name', { notice: 'toString' }],
  ])('shows nothing for %s', (_label, state) => {
    expect(loginNoticeFrom(state)).toBeNull();
  });
});

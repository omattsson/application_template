import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  LOGIN_NOTICE_KEY,
  OWN_ACCOUNT_CHANGED,
  loginNoticeText,
  rememberLoginNotice,
  takeLoginNotice,
} from '../loginNotice';

describe('loginNotice', () => {
  beforeEach(() => sessionStorage.clear());
  afterEach(() => {
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  it('returns the text of a remembered known key, once', () => {
    rememberLoginNotice(OWN_ACCOUNT_CHANGED);
    expect(takeLoginNotice()).toBe(loginNoticeText(OWN_ACCOUNT_CHANGED));
    expect(loginNoticeText(OWN_ACCOUNT_CHANGED)).toMatch(/your own account/i);
    expect(takeLoginNotice()).toBeNull();
    expect(sessionStorage.getItem(LOGIN_NOTICE_KEY)).toBeNull();
  });

  it('drops a notice older than a minute (the user never reached the login page)', () => {
    rememberLoginNotice(OWN_ACCOUNT_CHANGED, 1_000);
    expect(takeLoginNotice(1_000 + 61_000)).toBeNull();
    expect(sessionStorage.getItem(LOGIN_NOTICE_KEY)).toBeNull();
  });

  it('shows nothing when none was remembered', () => {
    expect(takeLoginNotice()).toBeNull();
  });

  it.each([
    ['an unknown key', 'something-else@1000'],
    ['free text', '<b>Call 555-0100</b>@1000'],
    ['an inherited property name', 'toString@1000'],
    ['a value without a time', OWN_ACCOUNT_CHANGED],
    ['a bad time', `${OWN_ACCOUNT_CHANGED}@soon`],
  ])('shows nothing for %s', (_label, stored) => {
    sessionStorage.setItem(LOGIN_NOTICE_KEY, stored);
    expect(takeLoginNotice(1_000)).toBeNull();
  });

  it('does not throw when sessionStorage is blocked', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(() => rememberLoginNotice(OWN_ACCOUNT_CHANGED)).not.toThrow();
    expect(takeLoginNotice()).toBeNull();
  });
});

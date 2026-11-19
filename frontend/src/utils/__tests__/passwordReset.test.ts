import { describe, it, expect, afterEach, vi } from 'vitest';
import {
  NEUTRAL_RESET_MESSAGE,
  PASSWORD_MAX_BYTES,
  PASSWORD_MIN_CHARACTERS,
  PASSWORD_TOO_LONG_MESSAGE,
  PASSWORD_TOO_SHORT_MESSAGE,
  passwordProblem,
  readResetToken,
  removeTokenFromAddress,
} from '../passwordReset';

const TOKEN = 'A'.repeat(43);

describe('passwordProblem', () => {
  it('accepts 8 characters and 72 bytes, and nothing else is checked', () => {
    expect(PASSWORD_MIN_CHARACTERS).toBe(8);
    expect(PASSWORD_MAX_BYTES).toBe(72);
    expect(passwordProblem('12345678')).toBeNull();
    expect(passwordProblem('a'.repeat(72))).toBeNull();
  });

  it('refuses fewer than 8 characters with the server text', () => {
    expect(passwordProblem('1234567')).toBe(PASSWORD_TOO_SHORT_MESSAGE);
    expect(passwordProblem('')).toBe(PASSWORD_TOO_SHORT_MESSAGE);
  });

  it('counts characters for the minimum (like the server) and bytes for the maximum', () => {
    // Seven characters, but 28 bytes: too short.
    expect(passwordProblem('🙂🙂🙂🙂🙂🙂🙂')).toBe(PASSWORD_TOO_SHORT_MESSAGE);
    // Eight characters, 32 bytes: fine.
    expect(passwordProblem('🙂🙂🙂🙂🙂🙂🙂🙂')).toBeNull();
  });

  it('refuses more than 72 bytes with the server text', () => {
    expect(passwordProblem('a'.repeat(73))).toBe(PASSWORD_TOO_LONG_MESSAGE);
    // 37 characters of 2 bytes each: 74 bytes.
    expect(passwordProblem('é'.repeat(37))).toBe(PASSWORD_TOO_LONG_MESSAGE);
  });
});

describe('readResetToken', () => {
  it('reads a 43-character token from the fragment', () => {
    expect(readResetToken(`#token=${TOKEN}`)).toBe(TOKEN);
    expect(readResetToken(`token=${TOKEN}`)).toBe(TOKEN);
  });

  it('accepts the URL-safe alphabet and nothing else', () => {
    const urlSafe = 'aZ09_-'.repeat(8).slice(0, 43);
    expect(readResetToken(`#token=${urlSafe}`)).toBe(urlSafe);
    expect(readResetToken(`#token=${'A'.repeat(42)}`)).toBeNull();
    expect(readResetToken(`#token=${'A'.repeat(44)}`)).toBeNull();
    expect(readResetToken(`#token=${'A'.repeat(42)}+`)).toBeNull();
    expect(readResetToken(`#token=${'A'.repeat(42)}%2F`)).toBeNull();
  });

  it('returns null without a token parameter, and ignores a token in the query', () => {
    expect(readResetToken('')).toBeNull();
    expect(readResetToken('#other=1')).toBeNull();
    expect(readResetToken(`#TOKEN=${TOKEN}`)).toBeNull();
  });
});

describe('removeTokenFromAddress', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState(null, '', '/');
  });

  it('removes the fragment and keeps the path, the query and the history state', () => {
    window.history.replaceState({ idx: 4 }, '', `/reset-password?lang=en#token=${TOKEN}`);
    const replace = vi.spyOn(window.history, 'replaceState');
    removeTokenFromAddress();
    expect(replace).toHaveBeenCalledWith({ idx: 4 }, '', '/reset-password?lang=en');
    expect(window.location.hash).toBe('');
    expect(window.location.pathname).toBe('/reset-password');
    expect(window.location.search).toBe('?lang=en');
  });
});

describe('neutral message', () => {
  it('does not say whether an account exists', () => {
    expect(NEUTRAL_RESET_MESSAGE).toMatch(/if the account exists/i);
  });
});

import { describe, it, expect } from 'vitest';
import { DEFAULT_EXPIRY_DAYS, EXPIRY_OPTIONS, KEY_NAME_MAX, validateKeyName } from '../validation';

describe('validateKeyName', () => {
  it.each(['ci', 'Build server 2', 'deploy.prod_v1', 'a:b@c/d(e)+f-g', 'Åsa Ö 7', '名前'])('accepts %j', (name) => {
    expect(validateKeyName(name)).toBeUndefined();
  });

  it('accepts exactly the maximum length', () => {
    expect(validateKeyName('a'.repeat(KEY_NAME_MAX))).toBeUndefined();
  });

  it('counts code points, as the server does', () => {
    // U+1D49C is outside the BMP: two UTF-16 units but one code point. The server counts code points.
    expect(validateKeyName('𝒜'.repeat(KEY_NAME_MAX))).toBeUndefined();
    expect(validateKeyName('𝒜'.repeat(KEY_NAME_MAX + 1))).toBeDefined();
  });

  it('trims the name before the checks, as the server does', () => {
    expect(validateKeyName('   ')).toBe('Enter a name for the key.');
    expect(validateKeyName('  ci  ')).toBeUndefined();
  });

  it('requires a name', () => {
    expect(validateKeyName('')).toBe('Enter a name for the key.');
  });

  it('refuses a name over the maximum length', () => {
    expect(validateKeyName('a'.repeat(KEY_NAME_MAX + 1))).toBe('The name can be at most 100 characters.');
  });

  it.each(['a!b', 'a#b', 'a,b', 'a=b', 'a"b', "a'b", 'a<b>', 'a\tb', 'a\nb', 'a;b', 'a&b'])(
    'refuses a character outside the server set: %j',
    (name) => {
      expect(validateKeyName(name)).toBe('Use only letters, digits, spaces and . _ - : @ / ( ) +.');
    },
  );
});

describe('expiry options', () => {
  it('offers 30, 90, 180 and 365 days, with 90 as the default', () => {
    expect(EXPIRY_OPTIONS).toEqual([30, 90, 180, 365]);
    expect(DEFAULT_EXPIRY_DAYS).toBe(90);
    expect(EXPIRY_OPTIONS).toContain(DEFAULT_EXPIRY_DAYS);
  });
});

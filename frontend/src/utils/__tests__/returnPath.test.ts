import { describe, it, expect } from 'vitest';
import { safeReturnPath, loginPathFor } from '../returnPath';

describe('safeReturnPath', () => {
  it.each([
    ['/', '/'],
    ['/items', '/items'],
    ['/items?limit=5#top', '/items?limit=5#top'],
    ['/health/', '/health/'],
  ])('keeps the same-origin path %s', (input, expected) => {
    expect(safeReturnPath(input)).toBe(expected);
  });

  it.each([
    ['null', null],
    ['undefined', undefined],
    ['empty', ''],
    ['protocol-relative URL', '//evil.example.com/items'],
    ['protocol-relative URL with backslash', '/\\evil.example.com'],
    ['backslash first', '\\\\evil.example.com'],
    ['absolute http URL', 'https://evil.example.com/'],
    ['javascript URL', 'javascript:alert(1)'],
    ['data URL', 'data:text/html,hi'],
    ['relative path without slash', 'items'],
    ['leading whitespace', ' //evil.example.com'],
    ['tab inside the slashes', '/\t/evil.example.com'],
    ['newline inside the slashes', '/\n/evil.example.com'],
    ['the login page (loop)', '/login'],
    ['the login page with a query', '/login?next=/items'],
    ['the OIDC callback page', '/auth/callback'],
    ['very long', `/${'a'.repeat(3000)}`],
  ])('falls back to / for %s', (_name, input) => {
    expect(safeReturnPath(input as string | null | undefined)).toBe('/');
  });

  it('uses the given fallback', () => {
    expect(safeReturnPath('//evil.example.com', '/home')).toBe('/home');
  });
});

describe('loginPathFor', () => {
  it('puts the encoded intended path in next', () => {
    expect(loginPathFor({ pathname: '/items', search: '?a=1', hash: '#x' })).toBe(
      `/login?next=${encodeURIComponent('/items?a=1#x')}`,
    );
  });

  it('omits next for the home page', () => {
    expect(loginPathFor({ pathname: '/', search: '', hash: '' })).toBe('/login');
  });
});

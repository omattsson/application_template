import { describe, it, expect } from 'vitest';
import { AxiosError, AxiosHeaders } from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import { describeWait, errorTextOf, retryAfterSeconds } from '../passwordChange';

function httpError(status: number, data: unknown, headers: Record<string, string> = {}): AxiosError {
  const config = { headers: new AxiosHeaders() } as InternalAxiosRequestConfig;
  return new AxiosError('failed', AxiosError.ERR_BAD_REQUEST, config, {}, { data, status, statusText: '', headers, config });
}

describe('errorTextOf', () => {
  it('reads the error text of a JSON body', () => {
    expect(errorTextOf(httpError(401, { error: 'Current password is incorrect' }))).toBe('Current password is incorrect');
  });

  it.each([
    ['no body', httpError(401, undefined)],
    ['a body without error', httpError(401, { message: 'x' })],
    ['a non-string error', httpError(401, { error: 5 })],
    ['a network error', new AxiosError('Network Error', AxiosError.ERR_NETWORK)],
    ['a plain Error', new Error('x')],
  ])('returns null for %s', (_name, err) => {
    expect(errorTextOf(err)).toBeNull();
  });
});

describe('retryAfterSeconds', () => {
  it('reads whole seconds, whatever the header case', () => {
    expect(retryAfterSeconds(httpError(429, {}, { 'Retry-After': '900' }))).toBe(900);
    expect(retryAfterSeconds(httpError(429, {}, { 'retry-after': '30' }))).toBe(30);
  });

  it('raises 0 to 1', () => {
    expect(retryAfterSeconds(httpError(429, {}, { 'retry-after': '0' }))).toBe(1);
  });

  it.each(['', 'soon', '-5', '1.5', 'Wed, 21 Oct 2026 07:28:00 GMT'])('returns null for %j', (value) => {
    expect(retryAfterSeconds(httpError(429, {}, { 'retry-after': value }))).toBeNull();
  });

  it('returns null without the header and for other errors', () => {
    expect(retryAfterSeconds(httpError(429, {}))).toBeNull();
    expect(retryAfterSeconds(new Error('x'))).toBeNull();
  });
});

describe('describeWait', () => {
  it.each([
    [1, '1 second'],
    [45, '45 seconds'],
    [60, '1 minute'],
    [61, '2 minutes'],
    [900, '15 minutes'],
  ])('%i seconds -> %s', (seconds, text) => {
    expect(describeWait(seconds)).toBe(text);
  });
});

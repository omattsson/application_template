import { describe, it, expect } from 'vitest';
import { AxiosError, AxiosHeaders } from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import { describeWait, retryAfterSeconds } from '../retryAfter';

function httpError(headers: Record<string, string> = {}, plain = false): AxiosError {
  const config = { headers: new AxiosHeaders() } as InternalAxiosRequestConfig;
  const responseHeaders = plain ? headers : new AxiosHeaders(headers);
  return new AxiosError('failed', AxiosError.ERR_BAD_REQUEST, config, {}, {
    data: {},
    status: 429,
    statusText: '',
    headers: responseHeaders,
    config,
  });
}

describe('retryAfterSeconds', () => {
  it('reads whole seconds', () => {
    expect(retryAfterSeconds(httpError({ 'retry-after': '30' }))).toBe(30);
    expect(retryAfterSeconds(httpError({ 'retry-after': ' 900 ' }))).toBe(900);
  });

  it('reads the header whatever its case (AxiosHeaders) and from a plain lower-case object', () => {
    expect(retryAfterSeconds(httpError({ 'Retry-After': '900' }))).toBe(900);
    expect(retryAfterSeconds(httpError({ 'retry-after': '45' }, true))).toBe(45);
  });

  it('reads an HTTP date relative to now, rounded up', () => {
    const now = Date.parse('Wed, 21 Oct 2026 07:28:00 GMT');
    expect(retryAfterSeconds(httpError({ 'retry-after': 'Wed, 21 Oct 2026 07:28:30 GMT' }), now)).toBe(30);
    expect(retryAfterSeconds(httpError({ 'retry-after': 'Wed, 21 Oct 2026 07:28:30 GMT' }), now - 500)).toBe(31);
  });

  it('returns undefined for an HTTP date that is not in the future', () => {
    const now = Date.parse('Wed, 21 Oct 2026 07:28:00 GMT');
    expect(retryAfterSeconds(httpError({ 'retry-after': 'Wed, 21 Oct 2026 07:28:00 GMT' }), now)).toBeUndefined();
    expect(retryAfterSeconds(httpError({ 'retry-after': 'Wed, 21 Oct 2026 07:00:00 GMT' }), now)).toBeUndefined();
  });

  it('caps the wait at one hour', () => {
    expect(retryAfterSeconds(httpError({ 'retry-after': '3600' }))).toBe(3600);
    expect(retryAfterSeconds(httpError({ 'retry-after': '3601' }))).toBeUndefined();
    const now = Date.parse('Wed, 21 Oct 2026 07:28:00 GMT');
    expect(retryAfterSeconds(httpError({ 'retry-after': 'Wed, 21 Oct 2026 09:28:00 GMT' }), now)).toBeUndefined();
  });

  it.each(['99999999999999999999', '9007199254740993', '1000000000'])('returns undefined for the huge integer %s', (value) => {
    expect(retryAfterSeconds(httpError({ 'retry-after': value }))).toBeUndefined();
  });

  it.each(['', 'soon', '-5', '1.5', '0'])('returns undefined for %j', (value) => {
    expect(retryAfterSeconds(httpError({ 'retry-after': value }))).toBeUndefined();
  });

  it('returns undefined without the header, without a response and for other errors', () => {
    expect(retryAfterSeconds(httpError())).toBeUndefined();
    expect(retryAfterSeconds(new AxiosError('Network Error', AxiosError.ERR_NETWORK))).toBeUndefined();
    expect(retryAfterSeconds(new Error('x'))).toBeUndefined();
    expect(retryAfterSeconds(undefined)).toBeUndefined();
  });
});

describe('describeWait', () => {
  it.each([
    [1, '1 second'],
    [45, '45 seconds'],
    [60, '1 minute'],
    [61, '2 minutes'],
    [900, '15 minutes'],
    [3600, '60 minutes'],
  ])('%i seconds -> %s', (seconds, text) => {
    expect(describeWait(seconds)).toBe(text);
  });
});

import { describe, it, expect } from 'vitest';
import { AxiosError, AxiosHeaders } from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import { getApiErrorMessage } from '../apiError';

/** An AxiosError as thrown by axios for a non-2xx response. */
function httpError(status: number, data: unknown): AxiosError {
  const config = { headers: new AxiosHeaders() } as InternalAxiosRequestConfig;
  return new AxiosError('Request failed', 'ERR_BAD_REQUEST', config, undefined, {
    status,
    statusText: '',
    data,
    headers: {},
    config,
  });
}

describe('getApiErrorMessage', () => {
  it('returns the {error} message of an axios error body', () => {
    expect(getApiErrorMessage(httpError(404, { error: 'item not found' }))).toBe('item not found');
  });

  it('returns the {message} of the body when there is no {error}', () => {
    expect(getApiErrorMessage(httpError(400, { message: 'name is required' }))).toBe('name is required');
  });

  it('prefers {error} over {message}', () => {
    expect(getApiErrorMessage(httpError(400, { error: 'bad input', message: 'other' }))).toBe('bad input');
  });

  it('ignores an empty or non-string {error}', () => {
    expect(getApiErrorMessage(httpError(400, { error: '', message: 'fallback text' }))).toBe('fallback text');
    expect(getApiErrorMessage(httpError(400, { error: 42 }))).toBe('Request failed with status 400');
  });

  it('reports the status when the body carries no message', () => {
    expect(getApiErrorMessage(httpError(429, ''))).toBe('Request failed with status 429');
    expect(getApiErrorMessage(httpError(418, '<html>Teapot</html>'))).toBe('Request failed with status 418');
  });

  it.each([500, 502, 503])('never shows the body of a %i', (status) => {
    expect(getApiErrorMessage(httpError(status, { error: 'pq: password authentication failed' }))).toBe(
      'Something went wrong on the server. Please try again.',
    );
  });

  it('handles a network error (no response)', () => {
    const err = new AxiosError('Network Error', 'ERR_NETWORK');
    expect(getApiErrorMessage(err)).toBe('Network error. Check your connection and try again.');
  });

  it('handles a plain Error with the message', () => {
    expect(getApiErrorMessage(new Error('Something broke'))).toBe('Something broke');
  });

  it.each([
    ['null', null],
    ['undefined', undefined],
    ['a string', 'boom'],
    ['a number', 42],
    ['an object', { foo: 'bar' }],
  ])('handles an unknown thrown value: %s', (_name, value) => {
    expect(getApiErrorMessage(value)).toBe('An unexpected error occurred.');
  });
});

import { AxiosError, AxiosHeaders } from 'axios';
import type { AxiosInstance, AxiosResponse, InternalAxiosRequestConfig } from 'axios';
import { vi } from 'vitest';

/** What a route handler of the fake backend answers. */
export type MockReply =
  | { status: number; data?: unknown; headers?: Record<string, string> }
  | { networkError: true };

/** Route handler of the fake backend: gets the final request config (after interceptors). */
export type MockHandler = (config: InternalAxiosRequestConfig) => MockReply | Promise<MockReply>;

/** Build the response (or the rejection) that a real axios adapter would produce. */
function settle(config: InternalAxiosRequestConfig, reply: MockReply): Promise<AxiosResponse> {
  if ('networkError' in reply) {
    return Promise.reject(new AxiosError('Network Error', AxiosError.ERR_NETWORK, config, {}));
  }
  const response: AxiosResponse = {
    data: reply.data,
    status: reply.status,
    statusText: String(reply.status),
    headers: new AxiosHeaders(reply.headers ?? {}),
    config,
    request: {},
  };
  if (reply.status >= 200 && reply.status < 300) return Promise.resolve(response);
  const code = reply.status >= 500 ? AxiosError.ERR_BAD_RESPONSE : AxiosError.ERR_BAD_REQUEST;
  return Promise.reject(new AxiosError(`Request failed with status code ${reply.status}`, code, config, {}, response));
}

/**
 * Replace the network layer of axios instances with a fake backend. The
 * interceptors of the instances still run, so tests see the real headers,
 * body and options of every request.
 * @param instances - The axios instances to fake (the API client and the auth client)
 * @param handler - Answers every request
 * @returns The adapter mock: `adapter.mock.calls[i][0]` is the request config
 */
export function installMockBackend(instances: AxiosInstance[], handler: MockHandler) {
  const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => settle(config, await handler(config)));
  for (const instance of instances) {
    instance.defaults.adapter = adapter;
  }
  return adapter;
}

/** Path of a request without the base URL (`/api`), for example `/api/v1/auth/refresh`. */
export function pathOf(config: InternalAxiosRequestConfig): string {
  return config.url ?? '';
}

/** Read a request header from an axios request config. */
export function headerOf(config: InternalAxiosRequestConfig, name: string): string | undefined {
  const value = AxiosHeaders.from(config.headers).get(name);
  return typeof value === 'string' ? value : undefined;
}

/** Build an unsigned JWT with the given claims (the SPA never verifies signatures). */
export function makeJwt(claims: Record<string, unknown>): string {
  const encode = (value: unknown) =>
    btoa(String.fromCharCode(...new TextEncoder().encode(JSON.stringify(value))))
      .replace(/\+/g, '-')
      .replace(/\//g, '_')
      .replace(/=+$/, '');
  return `${encode({ alg: 'HS256', typ: 'JWT' })}.${encode(claims)}.signature`;
}

/** A JWT for `username` with `role` that expires `ttlSeconds` from now. */
export function tokenFor(username: string, role = 'user', ttlSeconds = 900, extra: Record<string, unknown> = {}): string {
  const now = Math.floor(Date.now() / 1000);
  return makeJwt({
    user_id: `id-${username}`,
    username,
    display_name: `${username[0].toUpperCase()}${username.slice(1)} Person`,
    role,
    iat: now,
    exp: now + ttlSeconds,
    ...extra,
  });
}

/**
 * Flatten everything reachable from logged arguments into one string: every
 * string value and key of every own property (also non-enumerable ones such
 * as the message and the trace of an Error), recursively. Handles cycles.
 * Used to assert that a secret never reaches the console.
 * @param args - The arguments of console calls
 * @returns All reachable text
 */
export function reachableText(args: unknown[]): string {
  const seen = new WeakSet<object>();
  const out: string[] = [];
  const visit = (value: unknown, depth: number) => {
    if (depth > 12) return;
    if (typeof value === 'string') {
      out.push(value);
      return;
    }
    if (typeof value === 'number' || typeof value === 'boolean') {
      out.push(String(value));
      return;
    }
    if (!value || (typeof value !== 'object' && typeof value !== 'function')) return;
    if (seen.has(value as object)) return;
    seen.add(value as object);
    for (const key of Object.getOwnPropertyNames(value)) {
      out.push(key);
      let child: unknown;
      try {
        child = (value as Record<string, unknown>)[key];
      } catch {
        continue;
      }
      visit(child, depth + 1);
    }
    if (value instanceof Map) value.forEach((v, k) => { visit(k, depth + 1); visit(v, depth + 1); });
  };
  args.forEach((arg) => visit(arg, 0));
  return out.join('\n');
}

/** A promise with its resolve function exposed. */
export function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

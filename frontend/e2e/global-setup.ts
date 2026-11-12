import { randomBytes } from 'node:crypto';
import { request, type APIRequestContext, type APIResponse } from '@playwright/test';
import {
  ADMIN_PASSWORD,
  ADMIN_USERNAME,
  AUTH_RUN,
  BACKEND_API,
  BACKEND_URL,
  USER_DISPLAY_NAME,
  USER_USERNAME,
} from './authEnv';

/**
 * Runs once before all specs, against the backend itself (BACKEND_URL; the
 * browser goes through the dev server instead, see authEnv.ts).
 *
 * - Checks that the backend runs in the mode of the run: E2E_AUTH=1 needs
 *   AUTH_ENABLED=true, a run without it needs auth off. A mismatch fails the
 *   run at once instead of failing every page.
 * - Auth on: signs in as the local administrator and creates the non-admin
 *   account USER_USERNAME (role user) with a random password, which it hands
 *   to the workers in E2E_USER_PASSWORD. An account left over from an earlier
 *   run on the same database gets the new password.
 * - Makes sure that one item exists: the Items specs and the accessibility
 *   check of Items need a table with data (an empty database shows "No items
 *   found." and no table).
 */
export default async function globalSetup(): Promise<void> {
  const ctx = await request.newContext({ baseURL: BACKEND_URL });
  try {
    const authEnabled = await backendAuthEnabled(ctx);
    if (authEnabled === null) {
      if (AUTH_RUN) throw new Error(`E2E_AUTH=1, but the backend at ${BACKEND_URL} is not reachable. Run make test-e2e-auth.`);
      console.warn(`\n[global-setup] The backend at ${BACKEND_URL} is not reachable: no test data was created. Run make test-e2e.\n`);
      return;
    }
    if (authEnabled !== AUTH_RUN) {
      throw new Error(
        AUTH_RUN
          ? 'E2E_AUTH=1, but the backend runs with auth off. Run make test-e2e-auth.'
          : 'The backend runs with AUTH_ENABLED=true, but E2E_AUTH is not 1. Run make test-e2e-auth for the auth specs, or start the backend with auth off.',
      );
    }

    let token: string | null = null;
    if (AUTH_RUN) {
      if (!ADMIN_PASSWORD) throw new Error('E2E_AUTH=1 needs ADMIN_PASSWORD (the local administrator of the backend).');
      token = await login(ctx, ADMIN_USERNAME, ADMIN_PASSWORD);
      const password = randomBytes(18).toString('base64url');
      await ensureUser(ctx, token, password);
      process.env.E2E_USER_PASSWORD = password;
    }
    await ensureItem(ctx, token);
    if (token) await logout(ctx, token);
  } finally {
    await ctx.dispose();
  }
}

async function failure(what: string, res: APIResponse): Promise<Error> {
  return new Error(`${what}: ${res.status()} ${(await res.text()).slice(0, 200)}`);
}

/** auth_enabled of GET /api/v1/ui-config, or null when the backend does not answer one. */
async function backendAuthEnabled(ctx: APIRequestContext): Promise<boolean | null> {
  try {
    const res = await ctx.get(`${BACKEND_API}/ui-config`);
    if (!res.ok()) return null;
    const body = (await res.json()) as { auth_enabled?: unknown } | null;
    return typeof body?.auth_enabled === 'boolean' ? body.auth_enabled : null;
  } catch {
    return null;
  }
}

/** The login of a session needs the CSRF header and a JSON body (backend requireSessionLoginRequest). */
const SESSION_HEADERS = { 'X-Requested-With': 'XMLHttpRequest', 'Content-Type': 'application/json' };

async function login(ctx: APIRequestContext, username: string, password: string): Promise<string> {
  const res = await ctx.post(`${BACKEND_API}/auth/login`, { headers: SESSION_HEADERS, data: { username, password } });
  if (!res.ok()) throw await failure(`login as ${username}`, res);
  const body = (await res.json()) as { token?: unknown };
  if (typeof body.token !== 'string' || !body.token) throw new Error(`login as ${username}: the answer has no token`);
  return body.token;
}

async function logout(ctx: APIRequestContext, token: string): Promise<void> {
  const res = await ctx.post(`${BACKEND_API}/auth/logout`, {
    headers: { ...SESSION_HEADERS, Authorization: `Bearer ${token}` },
    data: {},
  });
  if (!res.ok()) throw await failure('logout of the setup session', res);
}

const bearer = (token: string | null): Record<string, string> => (token ? { Authorization: `Bearer ${token}` } : {});

/** Create the non-admin account, or set the new password of an existing one. */
async function ensureUser(ctx: APIRequestContext, token: string, password: string): Promise<void> {
  const created = await ctx.post(`${BACKEND_API}/users`, {
    headers: bearer(token),
    data: { username: USER_USERNAME, password, display_name: USER_DISPLAY_NAME, email: '', role: 'user' },
  });
  if (created.status() === 201) return;
  if (created.status() !== 409) throw await failure(`create the user ${USER_USERNAME}`, created);

  // Left over from an earlier run on this database: find it and reset it.
  for (let page = 1; ; page += 1) {
    const res = await ctx.get(`${BACKEND_API}/users`, { headers: bearer(token), params: { page, pageSize: 100 } });
    if (!res.ok()) throw await failure('list the users', res);
    const body = (await res.json()) as { data: { id: string; username: string; role: string; disabled: boolean }[] };
    const user = body.data.find((u) => u.username === USER_USERNAME);
    if (user) {
      if (user.role !== 'user' || user.disabled) {
        throw new Error(`${USER_USERNAME} exists with role ${user.role} (disabled: ${user.disabled}); remove it or use a fresh database`);
      }
      const reset = await ctx.put(`${BACKEND_API}/users/${user.id}`, {
        headers: bearer(token),
        data: { password, display_name: USER_DISPLAY_NAME },
      });
      if (!reset.ok()) throw await failure(`reset the password of ${USER_USERNAME}`, reset);
      return;
    }
    if (body.data.length < 100) throw new Error(`${USER_USERNAME} exists (409) but is not in the user list`);
  }
}

/** At least one item, so that the Items page shows a table. */
async function ensureItem(ctx: APIRequestContext, token: string | null): Promise<void> {
  const list = await ctx.get(`${BACKEND_API}/items`, { headers: bearer(token), params: { limit: 1 } });
  if (!list.ok()) throw await failure('list the items', list);
  const items = (await list.json()) as unknown[];
  if (Array.isArray(items) && items.length > 0) return;
  const res = await ctx.post(`${BACKEND_API}/items`, {
    headers: bearer(token),
    data: { name: 'E2E seed item', price: 9.99 },
  });
  if (res.status() !== 201) throw await failure('create the seed item', res);
}

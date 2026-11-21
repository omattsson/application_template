import { request, test, expect, type APIRequestContext } from '@playwright/test';
import { ADMIN_PASSWORD, ADMIN_USERNAME, AUTH_RUN, BACKEND_API, BACKEND_URL, signIn } from './authEnv';

// The audit log page (`/admin/audit-logs`) with the backend at AUTH_ENABLED=true
// (`make test-e2e-auth`). The administrator creates an item through the API, so
// the backend writes an audit entry for it. The page then shows that entry when
// it is filtered by the action `create` and the entity type `item`. The item is
// deleted again in a `finally`, also when a step fails.

/** The session login needs the CSRF header and a JSON body (backend requireSessionLoginRequest). */
const SESSION_HEADERS = { 'X-Requested-With': 'XMLHttpRequest', 'Content-Type': 'application/json' };

const bearer = (token: string): Record<string, string> => ({ Authorization: `Bearer ${token}` });

/** Sign in through the API (not the browser) and return the access token of that session. */
async function apiLogin(api: APIRequestContext, username: string, password: string): Promise<string> {
  const res = await api.post(`${BACKEND_API}/auth/login`, { headers: SESSION_HEADERS, data: { username, password } });
  if (!res.ok()) throw new Error(`login as ${username}: ${res.status()}`);
  const body = (await res.json()) as { token?: unknown };
  if (typeof body.token !== 'string' || !body.token) throw new Error(`login as ${username}: the answer has no token`);
  return body.token;
}

async function apiLogout(api: APIRequestContext, token: string): Promise<void> {
  const res = await api.post(`${BACKEND_API}/auth/logout`, {
    headers: { ...SESSION_HEADERS, ...bearer(token) },
    data: {},
  });
  if (!res.ok()) throw new Error(`logout of the setup session: ${res.status()}`);
}

test.describe('Audit log (AUTH_ENABLED=true)', () => {
  test.skip(!AUTH_RUN, 'needs the backend with AUTH_ENABLED=true: run make test-e2e-auth (E2E_AUTH=1)');

  test('the administrator filters the audit log and finds the entry of a new item', async ({ page }) => {
    const api = await request.newContext({ baseURL: BACKEND_URL });
    let token: string | null = null;
    let itemId: number | null = null;
    try {
      token = await apiLogin(api, ADMIN_USERNAME, ADMIN_PASSWORD);
      const created = await api.post(`${BACKEND_API}/items`, {
        headers: bearer(token),
        data: { name: `e2e-audit-${Date.now()}`, price: 1.5 },
      });
      expect(created.status()).toBe(201);
      itemId = ((await created.json()) as { id: number }).id;

      await signIn(page, ADMIN_USERNAME, ADMIN_PASSWORD, '/admin/audit-logs');
      await expect(page.getByRole('heading', { name: 'Audit log', level: 1 })).toBeVisible({ timeout: 10_000 });

      const filters = page.getByRole('search', { name: 'Filter audit entries' });
      await filters.getByRole('combobox', { name: 'Action' }).click();
      await page.getByRole('option', { name: 'create', exact: true }).click();
      await filters.getByRole('combobox', { name: 'Entity type' }).click();
      await page.getByRole('option', { name: 'item', exact: true }).click();
      await filters.getByRole('button', { name: 'Apply' }).click();

      // The newest entry is the item of this run: the entity ID is the item ID.
      const row = page.getByRole('row').filter({
        has: page.getByRole('cell', { name: String(itemId), exact: true }),
      });
      await expect(row).toBeVisible({ timeout: 10_000 });
      await expect(row.getByRole('cell', { name: 'create', exact: true })).toBeVisible();
      await expect(row.getByRole('cell', { name: 'item', exact: true })).toBeVisible();
      await expect(row.getByRole('cell', { name: 'admin', exact: false })).toBeVisible();
    } finally {
      if (token && itemId !== null) {
        await api.delete(`${BACKEND_API}/items/${itemId}`, { headers: bearer(token) });
      }
      if (token) await apiLogout(api, token);
      await api.dispose();
    }
  });
});

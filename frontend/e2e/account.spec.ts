import { randomBytes } from 'node:crypto';
import { expect, request, test, type APIRequestContext, type APIResponse } from '@playwright/test';
import {
  ADMIN_PASSWORD,
  ADMIN_USERNAME,
  AUTH_RUN,
  BACKEND_API,
  BACKEND_URL,
  BROWSER_API,
  signIn,
} from './authEnv';

// The account page (`/account`) with the backend at AUTH_ENABLED=true
// (`make test-e2e-auth`): the profile, the client checks, a wrong current
// password and the password change, then a sign-in with the new password.
//
// The spec makes its own non-admin account (a new name each run, deleted again
// in afterAll) instead of using e2e-user. A password change ends every other
// session of its account and a wrong current password counts toward the
// 5-failure lock of the username, and the other specs run in parallel with
// this one and sign in as e2e-user. The credentials of every shared account
// are therefore left as they were found, and nothing of this spec stays behind.

const SETUP_HEADERS = { 'X-Requested-With': 'XMLHttpRequest', 'Content-Type': 'application/json' };
const RUN_ID = `${Date.now()}${randomBytes(2).toString('hex')}`;
const USERNAME = `e2e-account-${RUN_ID}`;
const DISPLAY_NAME = 'E2E Account';
const INITIAL_PASSWORD = randomBytes(18).toString('base64url');
const NEW_PASSWORD = randomBytes(18).toString('base64url');

async function failure(what: string, res: APIResponse): Promise<Error> {
  return new Error(`${what}: ${res.status()} ${(await res.text()).slice(0, 200)}`);
}

/** Sign in as the administrator on the backend itself and return its token. */
async function adminToken(ctx: APIRequestContext): Promise<string> {
  const res = await ctx.post(`${BACKEND_API}/auth/login`, {
    headers: SETUP_HEADERS,
    data: { username: ADMIN_USERNAME, password: ADMIN_PASSWORD },
  });
  if (!res.ok()) throw await failure('login as the administrator', res);
  const body = (await res.json()) as { token?: unknown };
  if (typeof body.token !== 'string' || !body.token) throw new Error('administrator login: the answer has no token');
  return body.token;
}

test.describe('Account page (AUTH_ENABLED=true)', () => {
  test.skip(!AUTH_RUN, 'needs the backend with AUTH_ENABLED=true: run make test-e2e-auth (E2E_AUTH=1)');
  // One account, one password: the steps run in order.
  test.describe.configure({ mode: 'serial' });

  let userId: string | null = null;

  test.beforeAll(async () => {
    const ctx = await request.newContext({ baseURL: BACKEND_URL });
    try {
      const token = await adminToken(ctx);
      const created = await ctx.post(`${BACKEND_API}/users`, {
        headers: { Authorization: `Bearer ${token}` },
        data: { username: USERNAME, password: INITIAL_PASSWORD, display_name: DISPLAY_NAME, email: '', role: 'user' },
      });
      if (created.status() !== 201) throw await failure(`create the user ${USERNAME}`, created);
      userId = ((await created.json()) as { id: string }).id;
    } finally {
      await ctx.dispose();
    }
  });

  test.afterAll(async () => {
    if (!userId) return;
    const ctx = await request.newContext({ baseURL: BACKEND_URL });
    try {
      const token = await adminToken(ctx);
      const res = await ctx.delete(`${BACKEND_API}/users/${userId}`, { headers: { Authorization: `Bearer ${token}` } });
      if (!res.ok()) throw await failure(`delete the user ${USERNAME}`, res);
    } finally {
      await ctx.dispose();
    }
  });

  test('shows the profile, checks the form, and changes the password', async ({ page }) => {
    const changeRequests: string[] = [];
    page.on('request', (req) => {
      if (req.method() === 'PUT' && new URL(req.url()).pathname === `${BROWSER_API}/auth/me/password`) {
        changeRequests.push(req.url());
      }
    });

    await signIn(page, USERNAME, INITIAL_PASSWORD, '/account');
    await expect(page.getByRole('heading', { name: 'Account', level: 1 })).toBeVisible({ timeout: 10_000 });

    // The profile, from the session.
    await expect(page.getByText(DISPLAY_NAME, { exact: true }).first()).toBeVisible();
    await expect(page.getByText(USERNAME, { exact: true })).toBeVisible();
    await expect(page.getByText('Password', { exact: true })).toBeVisible();
    await expect(page.getByRole('link', { name: 'Manage your API keys' })).toHaveAttribute('href', '/account/api-keys');
    // The user name in the header leads here.
    await expect(page.getByTestId('current-user')).toHaveAttribute('href', '/account');

    const current = page.getByLabel('Current password', { exact: false });
    const next = page.getByLabel('New password', { exact: false }).first();
    const confirm = page.getByLabel('Confirm new password', { exact: false });
    const submit = page.getByRole('button', { name: 'Change password' });

    // A client check sends nothing.
    await current.fill(INITIAL_PASSWORD);
    await next.fill(NEW_PASSWORD);
    await confirm.fill(`${NEW_PASSWORD}-different`);
    await submit.click();
    await expect(page.getByText('The passwords do not match.')).toBeVisible();
    expect(changeRequests).toHaveLength(0);

    // A wrong current password: one request, a field error, the new password kept.
    await current.fill(`${INITIAL_PASSWORD}-wrong`);
    await confirm.fill(NEW_PASSWORD);
    await submit.click();
    await expect(page.getByText('Current password is incorrect')).toBeVisible({ timeout: 10_000 });
    expect(changeRequests).toHaveLength(1);
    await expect(current).toHaveValue('');
    await expect(current).toBeFocused();
    await expect(next).toHaveValue(NEW_PASSWORD);

    // The right one: the notice has focus and the fields are empty.
    await current.fill(INITIAL_PASSWORD);
    await submit.click();
    const notice = page.getByRole('status').filter({ hasText: 'Your password was changed' });
    await expect(notice).toBeVisible({ timeout: 10_000 });
    await expect(notice).toBeFocused();
    expect(changeRequests).toHaveLength(2);
    await expect(current).toHaveValue('');
    await expect(next).toHaveValue('');
    await expect(confirm).toHaveValue('');

    // This session stays signed in, and the new password works for a new sign-in.
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Account', level: 1 })).toBeVisible({ timeout: 10_000 });
    await page.getByRole('button', { name: /log out/i }).click();
    await signIn(page, USERNAME, NEW_PASSWORD, '/account');
    await expect(page.getByRole('heading', { name: 'Account', level: 1 })).toBeVisible({ timeout: 10_000 });
  });
});

test.describe('Account page (AUTH_ENABLED=false)', () => {
  test.skip(AUTH_RUN, 'runs with the backend at AUTH_ENABLED=false: run make test-e2e');

  test('the page does not exist when auth is off', async ({ page }) => {
    await page.goto('/account');
    await expect(page.getByText('Page not found')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByRole('heading', { name: 'Account', level: 1 })).toHaveCount(0);
  });
});

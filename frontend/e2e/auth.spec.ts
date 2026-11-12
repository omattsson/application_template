import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import {
  ADMIN_PASSWORD,
  ADMIN_USERNAME,
  AUTH_RUN,
  BROWSER_API,
  REFRESH_COOKIE,
  USER_DISPLAY_NAME,
  USER_USERNAME,
  isRefreshCall,
  signIn,
  userPassword,
} from './authEnv';

// Sign-in, session restore and roles with the backend at AUTH_ENABLED=true
// (`make test-e2e-auth`, which sets E2E_AUTH=1 and starts the backend with a
// test JWT_SECRET and ADMIN_PASSWORD). global-setup.ts creates the non-admin
// account and one item. Password login only: OIDC is not configured in the
// e2e backend.

if (!AUTH_RUN) {
  console.warn(
    '[auth.spec] SKIPPED: these tests need the backend with AUTH_ENABLED=true. Run `make test-e2e-auth` (it sets E2E_AUTH=1).',
  );
}

/** Display name of the admin that the backend creates from ADMIN_PASSWORD (handlers.AuthHandler.EnsureAdminUser). */
const ADMIN_DISPLAY_NAME = 'Administrator';

/** How long the two-tab test holds each refresh call in the browser. */
const REFRESH_HOLD_MS = 750;

const itemsHeading = (page: Page) => page.getByRole('heading', { name: 'Items', level: 1 });
const signInHeading = (page: Page) => page.getByRole('heading', { name: 'Sign in', level: 1 });
const navLink = (page: Page, name: string) =>
  page.getByRole('banner').getByRole('link', { name, exact: true });

/** Record the status of every refresh call of the context. */
function recordRefreshes(context: BrowserContext): number[] {
  const statuses: number[] = [];
  context.on('response', (res) => {
    if (isRefreshCall(res.url(), res.request().method())) statuses.push(res.status());
  });
  return statuses;
}

/**
 * Signed in as the administrator, on the Items page with its data. The
 * bootstrap admin is shown by its display name (from the token), and only an
 * admin gets the Users link.
 */
async function expectAdminOnItems(page: Page): Promise<void> {
  await expect(page).toHaveURL(/\/items$/, { timeout: 10_000 });
  await expect(itemsHeading(page)).toBeVisible({ timeout: 10_000 });
  await expect(page.getByTestId('current-user')).toHaveText(ADMIN_DISPLAY_NAME, { timeout: 10_000 });
  await expect(navLink(page, 'Users')).toBeVisible();
  await expect(page.getByRole('table')).toBeVisible({ timeout: 10_000 });
}

test.describe('Authentication (AUTH_ENABLED=true)', () => {
  test.skip(!AUTH_RUN, 'needs the backend with AUTH_ENABLED=true: run make test-e2e-auth (E2E_AUTH=1)');

  test('an anonymous visit to Items redirects to the login page', async ({ page }) => {
    await page.goto('/items');

    await expect(page).toHaveURL(/\/login\?next=%2Fitems$/, { timeout: 10_000 });
    await expect(signInHeading(page)).toBeVisible({ timeout: 10_000 });
    await expect(itemsHeading(page)).toHaveCount(0);
    // No navigation for an anonymous visitor.
    await expect(navLink(page, 'Items')).toHaveCount(0);
  });

  test('a wrong password shows the generic error and does not sign in', async ({ page, context }) => {
    const refreshes = recordRefreshes(context);
    // The non-admin account: a failure counts towards the per-name login
    // throttle, which must never lock out the administrator of the run.
    await signIn(page, USER_USERNAME, `${userPassword()}-wrong`, '/items');

    const alert = page.getByRole('alert');
    await expect(alert).toHaveText('Invalid username or password', { timeout: 10_000 });
    await expect(page).toHaveURL(/\/login\?next=%2Fitems$/);
    await expect(signInHeading(page)).toBeVisible();
    // The password field is emptied, the username is kept.
    await expect(page.getByLabel('Password', { exact: false })).toHaveValue('');
    await expect(page.getByLabel('Username', { exact: false })).toHaveValue(USER_USERNAME);
    // No session: no refresh cookie, and the page start refresh was refused.
    const cookies = await context.cookies();
    expect(cookies.filter((c) => c.name === REFRESH_COOKIE)).toEqual([]);
    expect(refreshes.every((s) => s === 401), `refresh statuses ${refreshes.join(',')}`).toBe(true);
  });

  test('the administrator signs in, sees Items and the Users link', async ({ page, context }) => {
    await signIn(page, ADMIN_USERNAME, ADMIN_PASSWORD, '/items');

    await expectAdminOnItems(page);
    await expect(navLink(page, 'Users')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Log out' })).toBeVisible();

    // The refresh cookie: httpOnly, SameSite=Strict, Path=/, host-only, and
    // Secure exactly when the backend runs with SECURE_COOKIES=true.
    const cookie = (await context.cookies()).find((c) => c.name === REFRESH_COOKIE);
    expect(cookie, `${REFRESH_COOKIE} cookie after the login`).toBeDefined();
    expect(cookie).toMatchObject({ httpOnly: true, sameSite: 'Strict', path: '/', domain: 'localhost' });
    expect(cookie?.secure).toBe(REFRESH_COOKIE.startsWith('__Host-'));
    // The access token is in memory only, never in web storage.
    const stored = await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }));
    expect(stored).not.toMatch(/eyJ[\w-]+\.[\w-]+\./);
  });

  test('a page reload keeps the session through the refresh cookie', async ({ page, context }) => {
    await signIn(page, ADMIN_USERNAME, ADMIN_PASSWORD, '/items');
    await expectAdminOnItems(page);

    const refreshes = recordRefreshes(context);
    // The reload forgets the in-memory token; the refresh call must carry the cookie.
    const refreshRequest = page.waitForRequest((req) => isRefreshCall(req.url(), req.method()));
    await page.reload();
    const sent = await refreshRequest;
    expect(await sent.headerValue('cookie'), 'the refresh call sends the refresh cookie').toContain(`${REFRESH_COOKIE}=`);

    await expectAdminOnItems(page);
    expect(refreshes).toEqual([200]);
  });

  test('two tabs that reload at the same time both stay signed in', async ({ page, context }) => {
    await signIn(page, ADMIN_USERNAME, ADMIN_PASSWORD, '/items');
    await expectAdminOnItems(page);
    const second = await context.newPage();
    await second.goto('/items');
    await expectAdminOnItems(second);

    // The refresh calls of both tabs, in the order the browser sent and finished them.
    const events: string[] = [];
    const statuses = recordRefreshes(context);
    context.on('request', (req) => {
      if (isRefreshCall(req.url(), req.method())) events.push('start');
    });
    context.on('requestfinished', (req) => {
      if (isRefreshCall(req.url(), req.method())) events.push('end');
    });
    // Hold every refresh call for a moment, so that the other tab surely
    // reaches its own refresh while the first one is still in flight. Without
    // the cross-tab lock the two calls would then overlap.
    await context.route(`**${BROWSER_API}/auth/refresh`, async (route) => {
      await new Promise((resolve) => setTimeout(resolve, REFRESH_HOLD_MS));
      await route.continue();
    });

    await Promise.all([page.reload(), second.reload()]);
    await expectAdminOnItems(page);
    await expectAdminOnItems(second);

    // A tab either adopts the token the other tab shared or refreshes after it
    // (Web Lock `auth-refresh`): the calls never overlap, and none is refused.
    expect(statuses.length, 'refresh calls of the two reloads').toBeGreaterThanOrEqual(1);
    expect(statuses.every((s) => s === 200), `refresh statuses ${statuses.join(',')}`).toBe(true);
    let open = 0;
    for (const e of events) {
      open += e === 'start' ? 1 : -1;
      expect(open, `refresh calls overlapped: ${events.join(' ')}`).toBeLessThanOrEqual(1);
    }

    // The session is still valid on the server: a third reload refreshes again.
    await page.reload();
    await expectAdminOnItems(page);
  });

  test('log out returns to the login page, and Back does not show Items', async ({ page, context }) => {
    await signIn(page, ADMIN_USERNAME, ADMIN_PASSWORD);
    await expect(page.getByTestId('current-user')).toHaveText(ADMIN_DISPLAY_NAME, { timeout: 10_000 });
    // History: ..., /items, /health. The logout replaces /health with /login,
    // so Back goes to /items.
    await navLink(page, 'Items').click();
    await expectAdminOnItems(page);
    await navLink(page, 'Health').click();
    await expect(page.getByRole('heading', { name: 'System Health', level: 1 })).toBeVisible({ timeout: 10_000 });

    await page.getByRole('button', { name: 'Log out' }).click();
    await expect(page).toHaveURL(/\/login$/, { timeout: 10_000 });
    await expect(signInHeading(page)).toBeVisible();
    await expect((await context.cookies()).filter((c) => c.name === REFRESH_COOKIE)).toEqual([]);

    await page.goBack();
    // Back reaches /items, which sends the visitor without a session to the login page.
    await expect(page).toHaveURL(/\/login\?next=%2Fitems$/, { timeout: 10_000 });
    await expect(signInHeading(page)).toBeVisible({ timeout: 10_000 });
    await expect(itemsHeading(page)).toHaveCount(0);
    await expect(page.getByRole('table')).toHaveCount(0);
    await expect(page.getByTestId('current-user')).toHaveCount(0);

    // A reload does not bring the session back either: the refresh is refused.
    const refreshes = recordRefreshes(context);
    await page.reload();
    await expect(signInHeading(page)).toBeVisible({ timeout: 10_000 });
    await expect(page).toHaveURL(/\/login\?next=%2Fitems$/);
    await expect(itemsHeading(page)).toHaveCount(0);
    expect(refreshes.every((s) => s === 401), `refresh statuses ${refreshes.join(',')}`).toBe(true);
  });

  test('a non-admin user cannot open the user administration', async ({ page }) => {
    await signIn(page, USER_USERNAME, userPassword(), '/items');
    await expect(itemsHeading(page)).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('current-user')).toHaveText(USER_DISPLAY_NAME, { timeout: 10_000 });

    // The navigation is shown, without the Users link.
    await expect(navLink(page, 'Items')).toBeVisible();
    await expect(navLink(page, 'Users')).toHaveCount(0);

    await page.goto('/admin/users');
    await expect(page.getByRole('heading', { name: 'Access denied', level: 1 })).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText('You do not have permission to see this page.')).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Users', level: 1 })).toHaveCount(0);
    await expect(page.getByRole('table')).toHaveCount(0);
  });
});

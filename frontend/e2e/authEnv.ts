import { expect, type Page } from '@playwright/test';

// Environment of an e2e run, shared by global-setup.ts and the specs.
//
//   E2E_AUTH=1           The backend runs with AUTH_ENABLED=true (`make test-e2e-auth`).
//                        Without it the backend runs with auth off (`make test-e2e`).
//                        global-setup.ts fails the run when the backend says otherwise.
//   E2E_BACKEND_URL      The backend itself, for the setup requests (default
//                        http://localhost:8081). The browser never calls it: the
//                        pages call the dev server, which proxies /api and /ws.
//   ADMIN_USERNAME,      The local administrator; the same variables the backend
//   ADMIN_PASSWORD       reads. The password is required with E2E_AUTH=1.
//   E2E_SECURE_COOKIES   "true" when the backend runs with SECURE_COOKIES=true: the
//                        refresh cookie is then __Host-refresh_token, else refresh_token.
//   E2E_USER_USERNAME    The non-admin account that global-setup.ts creates (default
//                        e2e-user). Its password is random per run: global-setup.ts
//                        puts it in E2E_USER_PASSWORD for the workers.
//
// No credential has a default here: the Makefile passes the test values.

export const AUTH_RUN = process.env.E2E_AUTH === '1';
export const BACKEND_URL = process.env.E2E_BACKEND_URL || 'http://localhost:8081';
export const ADMIN_USERNAME = process.env.ADMIN_USERNAME || 'admin';
export const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD ?? '';
export const USER_USERNAME = process.env.E2E_USER_USERNAME || 'e2e-user';
/** Display name that global-setup.ts gives the non-admin account. */
export const USER_DISPLAY_NAME = 'E2E User';
export const SECURE_COOKIES = process.env.E2E_SECURE_COOKIES === 'true';
/** Name of the httpOnly refresh cookie (backend handlers.refreshCookieName). */
export const REFRESH_COOKIE = SECURE_COOKIES ? '__Host-refresh_token' : 'refresh_token';

/**
 * API path prefix as the browser calls it. The app is same-origin: it calls
 * `/api` + `/api/v1/...` on the dev server (or nginx), which strips the first
 * `/api` before it forwards the request to the backend (src/api/config.ts).
 */
export const BROWSER_API = '/api/api/v1';
/** API path prefix on the backend itself (setup requests to BACKEND_URL). */
export const BACKEND_API = '/api/v1';

/** Password of the non-admin account; global-setup.ts sets it for the run. */
export function userPassword(): string {
  const password = process.env.E2E_USER_PASSWORD;
  if (!password) throw new Error('E2E_USER_PASSWORD is not set: global-setup.ts creates the non-admin user in auth runs');
  return password;
}

/** Whether a response is the browser's refresh call (POST /api/api/v1/auth/refresh). */
export function isRefreshCall(url: string, method: string): boolean {
  return method === 'POST' && new URL(url).pathname === `${BROWSER_API}/auth/refresh`;
}

/**
 * Sign in through the login form. The access token lives in memory only, so
 * the form is the only way in (a later `page.goto` restores the session from
 * the refresh cookie).
 * @param page - Playwright page
 * @param username - Login name
 * @param password - Password
 * @param next - Page to land on (default: home)
 */
export async function signIn(page: Page, username: string, password: string, next?: string): Promise<void> {
  await page.goto(next ? `/login?next=${encodeURIComponent(next)}` : '/login');
  await expect(page.getByRole('heading', { name: 'Sign in', level: 1 })).toBeVisible({ timeout: 10_000 });
  await page.getByLabel('Username', { exact: false }).fill(username);
  await page.getByLabel('Password', { exact: false }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
}

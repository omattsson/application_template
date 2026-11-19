import { randomBytes } from 'node:crypto';
import { expect, test, type APIRequestContext } from '@playwright/test';
import { AUTH_RUN, BROWSER_API, signIn } from './authEnv';

// Happy path of the password reset (issue #34) with the mail catcher Mailpit:
// request a reset for a local account that has an email address, read the mail
// through the Mailpit HTTP API, open the link (the token is in the fragment),
// set a new password and sign in with it.
//
// Runs only when all of these hold, otherwise the test is skipped:
//   E2E_AUTH=1                 the backend runs with AUTH_ENABLED=true (make test-e2e-auth)
//   MAILPIT_URL                the Mailpit HTTP API, for example http://127.0.0.1:8025
//   PASSWORD_RESET_ENABLED     the backend answers password_reset_enabled: true on /ui-config
//
// scripts/e2e.sh seeds the account and exports the values (the defaults below are the same):
//   E2E_RESET_USERNAME         the local account with an email (default e2e-reset-user)
//   E2E_RESET_EMAIL            that account's address; the reset mail goes there (default e2e-reset-user@example.test)
//   E2E_RESET_PASSWORD         the account's initial password (set by the harness; the reset replaces it)
//   MAILPIT_URL                default http://127.0.0.1:18025 in the harness
// The inbox is cleared before the run; the mail is plain text. The run sets PASSWORD_RESET_MIN_INTERVAL=1s.
// The test fails, not skips, when the account or the mail is missing.

const MAILPIT_URL = process.env.MAILPIT_URL ?? '';
const RESET_USERNAME = process.env.E2E_RESET_USERNAME || 'e2e-reset-user';
const RESET_EMAIL = process.env.E2E_RESET_EMAIL || 'e2e-reset-user@example.test';
/** How long to wait for the mail to arrive in Mailpit. */
const MAIL_TIMEOUT_MS = 20_000;
const MAIL_POLL_MS = 500;

/** The link of the mail: <PASSWORD_RESET_URL>/reset-password#token=<43 URL-safe characters>. */
const LINK_PATTERN = /https?:\/\/[^\s"<>]+\/reset-password#token=([A-Za-z0-9_-]{43})(?![A-Za-z0-9_-])/;

interface MailpitSummary {
  ID: string;
  To: { Address: string }[];
}

interface MailpitMessage {
  Text: string;
}

async function mailpitGet<T>(request: APIRequestContext, path: string): Promise<T> {
  const res = await request.get(`${MAILPIT_URL}${path}`);
  if (!res.ok()) throw new Error(`Mailpit ${path} answered ${res.status()}`);
  return (await res.json()) as T;
}

/** IDs of the messages that Mailpit holds now (the baseline before the request). */
async function messageIds(request: APIRequestContext): Promise<Set<string>> {
  const list = await mailpitGet<{ messages?: MailpitSummary[] }>(request, '/api/v1/messages');
  return new Set((list.messages ?? []).map((m) => m.ID));
}

/** Wait for a new mail to RESET_EMAIL and return its link token. */
async function waitForResetToken(request: APIRequestContext, before: Set<string>): Promise<string> {
  const deadline = Date.now() + MAIL_TIMEOUT_MS;
  for (;;) {
    const list = await mailpitGet<{ messages?: MailpitSummary[] }>(request, '/api/v1/messages');
    const fresh = (list.messages ?? []).filter(
      (m) => !before.has(m.ID) && m.To.some((to) => to.Address.toLowerCase() === RESET_EMAIL.toLowerCase()),
    );
    for (const summary of fresh) {
      const message = await mailpitGet<MailpitMessage>(request, `/api/v1/message/${encodeURIComponent(summary.ID)}`);
      const match = LINK_PATTERN.exec(message.Text ?? '');
      if (match) return match[1];
    }
    if (Date.now() > deadline) {
      throw new Error(`No reset mail for ${RESET_EMAIL} arrived in Mailpit within ${MAIL_TIMEOUT_MS} ms`);
    }
    await new Promise((resolve) => setTimeout(resolve, MAIL_POLL_MS));
  }
}

/** Whether the backend offers the password reset (ui-config password_reset_enabled). */
async function resetEnabled(request: APIRequestContext): Promise<boolean> {
  const res = await request.get(`${BROWSER_API}/ui-config`);
  if (!res.ok()) return false;
  const body = (await res.json()) as { password_reset_enabled?: unknown };
  return body.password_reset_enabled === true;
}

test.describe('Password reset (happy path, Mailpit)', () => {
  test.skip(!AUTH_RUN, 'needs the backend with AUTH_ENABLED=true (make test-e2e-auth)');
  test.skip(MAILPIT_URL === '', 'MAILPIT_URL is not set: no mail catcher for this run');

  test.beforeEach(async ({ request }) => {
    test.skip(!(await resetEnabled(request)), 'PASSWORD_RESET_ENABLED is off on the backend');
  });

  test('a reset mail sets a new password, and the new password signs in', async ({ page, request }) => {
    expect(RESET_EMAIL, 'E2E_RESET_EMAIL must be the email of the reset account').not.toBe('');
    const before = await messageIds(request);
    const newPassword = randomBytes(18).toString('base64url');

    // 1. Ask for the mail. The answer is the same whatever the account.
    await page.goto('/login');
    await page.getByRole('link', { name: 'Forgot password?' }).click();
    await expect(page.getByRole('heading', { name: 'Forgot password', level: 1 })).toBeVisible();
    await page.getByLabel(/username/i).fill(RESET_USERNAME);
    await page.getByRole('button', { name: 'Send reset link' }).click();
    await expect(page.getByRole('status')).toContainText('If the account exists and can receive mail');

    // 2. Open the link from the mail. The fragment carries the token.
    const token = await waitForResetToken(request, before);
    await page.goto(`/reset-password#token=${token}`);
    await expect(page.getByRole('heading', { name: 'Set a new password', level: 1 })).toBeVisible();
    await expect.poll(() => page.url()).not.toContain('token=');

    // 3. Set the new password.
    await page.getByLabel(/^new password/i).fill(newPassword);
    await page.getByLabel(/^confirm new password/i).fill(newPassword);
    await page.getByRole('button', { name: 'Reset password' }).click();
    await expect(page).toHaveURL(/\/login$/);
    await expect(page.getByRole('status')).toContainText('Your password was reset');

    // 4. Sign in with the new password.
    await signIn(page, RESET_USERNAME, newPassword);
    await expect(page).not.toHaveURL(/\/login/);
  });
});

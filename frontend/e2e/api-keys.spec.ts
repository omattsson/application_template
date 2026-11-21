import { test, expect } from '@playwright/test';
import { AUTH_RUN, USER_USERNAME, signIn, userPassword } from './authEnv';

// The API keys page (`/account/api-keys`) with the backend at AUTH_ENABLED=true
// (`make test-e2e-auth`). A signed-in user creates a key, sees the secret once,
// and revokes the key again. The key name is unique per run, so a key left by a
// failed run does not clash with the next one.

/** The raw key of a new key: sk_ and 64 lowercase hex characters. */
const RAW_KEY_PATTERN = /^sk_[0-9a-f]{64}$/;

test.describe('API keys (AUTH_ENABLED=true)', () => {
  test.skip(!AUTH_RUN, 'needs the backend with AUTH_ENABLED=true: run make test-e2e-auth (E2E_AUTH=1)');

  test('a user creates a key, sees the secret once, and revokes the key', async ({ page }) => {
    const name = `e2e-key-${Date.now()}`;
    await signIn(page, USER_USERNAME, userPassword(), '/account/api-keys');
    await expect(page.getByRole('heading', { name: 'API keys', level: 1 })).toBeVisible({ timeout: 10_000 });

    await page.getByRole('button', { name: 'Create API key' }).click();
    const form = page.getByRole('dialog', { name: 'Create API key' });
    await form.getByRole('textbox', { name: /^name/i }).fill(name);
    await form.getByRole('button', { name: 'Create', exact: true }).click();

    // The secret is shown once, in the dialog.
    const secret = page.getByRole('textbox', { name: 'Your API key' });
    await expect(secret).toHaveValue(RAW_KEY_PATTERN, { timeout: 10_000 });
    const rawKey = await secret.inputValue();
    // Never in the URL or in web storage.
    expect(page.url()).not.toContain(rawKey);
    const stored = await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }));
    expect(stored).not.toContain(rawKey);

    // Escape does not close the dialog before the user has seen the key; Done does.
    await page.keyboard.press('Escape');
    await expect(page.getByRole('dialog', { name: 'API key created' })).toBeVisible();
    await page.getByRole('button', { name: 'Done' }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(page.getByText(rawKey)).toHaveCount(0);

    // The list shows the new key, without the secret.
    const row = page.getByRole('row', { name: new RegExp(`^${name}\\b`) });
    await expect(row).toBeVisible({ timeout: 10_000 });
    await expect(row.getByText('Never')).toBeVisible();

    // Revoke: confirm, then the row is gone and the notice says so.
    await page.getByRole('button', { name: `Revoke ${name}` }).click();
    await page
      .getByRole('dialog', { name: 'Revoke API key' })
      .getByRole('button', { name: 'Revoke', exact: true })
      .click();
    await expect(page.getByText(`Key ${name} revoked`)).toBeVisible({ timeout: 10_000 });
    await expect(page.getByRole('row', { name: new RegExp(`^${name}\\b`) })).toHaveCount(0);
  });
});

test.describe('API keys (AUTH_ENABLED=false)', () => {
  test.skip(AUTH_RUN, 'runs with the backend at AUTH_ENABLED=false: run make test-e2e');

  test('the page does not exist when auth is off', async ({ page }) => {
    await page.goto('/account/api-keys');
    await expect(page.getByText('Page not found')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByRole('heading', { name: 'API keys', level: 1 })).toHaveCount(0);
  });
});

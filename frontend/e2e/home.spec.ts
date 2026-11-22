import { test, expect } from '@playwright/test';

test.describe('Home Page', () => {
  test('renders the welcome heading', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Welcome to Application Template');
  });

  test('sets the document title from the UI config', async ({ page }) => {
    await page.goto('/');
    await expect(page).toHaveTitle('Application Template');
  });

  test('renders introductory text', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByText('This is the starting point for your application')).toBeVisible();
  });

  test('lists where to start and links to Items', async ({ page }) => {
    await page.goto('/');
    const start = page.getByRole('region', { name: 'Where to start' });
    await expect(start.getByRole('link', { name: /Health/ })).toBeVisible();
    await start.getByRole('link', { name: /Items/ }).click();
    await expect(page).toHaveURL('/items');
  });

  test('renders the "Make this your own" section', async ({ page }) => {
    await page.goto('/');
    const own = page.getByRole('region', { name: 'Make this your own' });
    await expect(own.getByText('EXTENDING.md')).toBeVisible();
    await expect(own.getByText('APP_TITLE')).toBeVisible();
  });
});

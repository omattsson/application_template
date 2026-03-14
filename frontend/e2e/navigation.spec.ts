import { test, expect } from '@playwright/test';

test.describe('Navigation', () => {
  test('navigates from Home to Health via nav link', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'Welcome to the Full Stack Application'
    );

    await page.getByRole('link', { name: /health/i }).click();
    await expect(page).toHaveURL('/health');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('System Health', {
      timeout: 10_000,
    });
  });

  test('navigates from Health back to Home via nav link', async ({ page }) => {
    await page.goto('/health');

    // Wait for the page to settle
    await expect(page.getByText('System Health')).toBeVisible({ timeout: 10_000 });

    await page.getByRole('link', { name: /home/i }).click();
    await expect(page).toHaveURL('/');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'Welcome to the Full Stack Application'
    );
  });

  test('renders the app bar with title and nav links', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByText('Full Stack App', { exact: true })).toBeVisible();
    await expect(page.getByRole('link', { name: /home/i })).toBeVisible();
    await expect(page.getByRole('link', { name: /health/i })).toBeVisible();
    await expect(page.getByRole('link', { name: /items/i })).toBeVisible();
  });

  test('navigates from Home to Items via nav link', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'Welcome to the Full Stack Application'
    );

    await page.getByRole('link', { name: /items/i }).click();
    await expect(page).toHaveURL('/items');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Items', {
      timeout: 10_000,
    });
  });

  test('renders the footer with current year', async ({ page }) => {
    await page.goto('/');
    const year = new Date().getFullYear().toString();
    await expect(page.getByText(new RegExp(`© ${year}`))).toBeVisible();
  });
});

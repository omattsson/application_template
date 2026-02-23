import { test, expect } from '@playwright/test';

test.describe('Health Page', () => {
  test('displays live and ready status when backend is healthy', async ({ page }) => {
    await page.goto('/health');

    // Wait for health checks to complete
    await expect(page.getByText('System is live')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText('System is ready to handle requests')).toBeVisible();
  });

  test('renders the page heading', async ({ page }) => {
    await page.goto('/health');

    await expect(page.getByRole('heading', { level: 1 })).toHaveText('System Health', {
      timeout: 10_000,
    });
  });

  test('shows Liveness Check and Readiness Check sections', async ({ page }) => {
    await page.goto('/health');

    await expect(page.getByText('Liveness Check')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText('Readiness Check')).toBeVisible();
  });
});

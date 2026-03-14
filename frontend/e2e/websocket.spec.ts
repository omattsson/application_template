import { test, expect } from '@playwright/test';

/**
 * WebSocket integration e2e tests.
 *
 * These tests verify that the app remains fully functional regardless of the
 * WebSocket connection state. The WebSocket provider connects in the background,
 * so we assert on observable UI behaviour rather than on the socket itself.
 */
test.describe('WebSocket Integration', () => {
  test('app loads and renders correctly with WebSocket running in background', async ({ page }) => {
    const wsErrors: string[] = [];
    page.on('pageerror', (error) => {
      if (
        error.message.toLowerCase().includes('websocket') ||
        error.message.includes('ws://')
      ) {
        wsErrors.push(error.message);
      }
    });

    await page.goto('/');

    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'Welcome to the Full Stack Application',
      { timeout: 10_000 }
    );

    // No uncaught WebSocket-related JS errors should surface to the page
    expect(wsErrors).toHaveLength(0);
  });

  test('navigation works correctly while WebSocket is active', async ({ page }) => {
    await page.goto('/');
    await expect(
      page.getByRole('heading', { level: 1 })
    ).toHaveText('Welcome to the Full Stack Application', { timeout: 10_000 });

    // Navigate away and back — the WebSocket provider should survive route changes
    await page.getByRole('link', { name: /health/i }).click();
    await expect(page).toHaveURL('/health');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'System Health',
      { timeout: 10_000 }
    );

    await page.getByRole('link', { name: /home/i }).click();
    await expect(page).toHaveURL('/');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'Welcome to the Full Stack Application'
    );
  });

  test('app recovers gracefully when WebSocket connection is interrupted', async ({
    page,
    context,
  }) => {
    await page.goto('/');
    await expect(
      page.getByRole('heading', { level: 1 })
    ).toHaveText('Welcome to the Full Stack Application', { timeout: 10_000 });

    // Simulate a network interruption by going offline then online
    await context.setOffline(true);
    await context.setOffline(false);

    // App UI must still be functional after the interruption
    await expect(
      page.getByRole('link', { name: /health/i })
    ).toBeVisible({ timeout: 10_000 });

    await page.getByRole('link', { name: /health/i }).click();
    await expect(page).toHaveURL('/health');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'System Health',
      { timeout: 10_000 }
    );
  });
});

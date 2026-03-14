import { test, expect } from '@playwright/test';

test.describe('Items Page', () => {
  test('renders the page heading', async ({ page }) => {
    await page.goto('/items');

    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Items', {
      timeout: 10_000,
    });
  });

  test('displays the items table with data from the API', async ({ page }) => {
    await page.goto('/items');

    // Wait for loading spinner to disappear, indicating data has loaded
    await expect(page.getByRole('progressbar')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByRole('progressbar')).not.toBeVisible({ timeout: 10_000 });

    // Table should be visible with expected column headers
    await expect(page.getByRole('table')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByRole('columnheader', { name: 'Name' })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Price' })).toBeVisible();
  });

  test('navigates to Items page via nav link and back', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'Welcome to the Full Stack Application',
      { timeout: 10_000 }
    );

    await page.getByRole('link', { name: /items/i }).click();
    await expect(page).toHaveURL('/items');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Items', {
      timeout: 10_000,
    });

    await page.getByRole('link', { name: /home/i }).click();
    await expect(page).toHaveURL('/');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'Welcome to the Full Stack Application'
    );
  });

  test('renders table column headers', async ({ page }) => {
    await page.goto('/items');

    await expect(page.getByRole('columnheader', { name: 'ID' })).toBeVisible({
      timeout: 10_000,
    });
    await expect(page.getByRole('columnheader', { name: 'Name' })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Price' })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Description' })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Created At' })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Updated At' })).toBeVisible();
  });
});

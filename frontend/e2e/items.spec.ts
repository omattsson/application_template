import { test, expect, type Page } from '@playwright/test';
import { AUTH_RUN, BROWSER_API } from './authEnv';

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
      'Welcome to Application Template',
      { timeout: 10_000 }
    );

    await page.getByRole('banner').getByRole('link', { name: /items/i }).click();
    await expect(page).toHaveURL('/items');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Items', {
      timeout: 10_000,
    });

    await page.getByRole('banner').getByRole('link', { name: /home/i }).click();
    await expect(page).toHaveURL('/');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'Welcome to Application Template'
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
    await expect(page.getByRole('columnheader', { name: 'Actions' })).toBeVisible();
  });

  test('asks for confirmation before deleting an item', async ({ page }) => {
    await page.goto('/items');

    const deleteButton = page.getByRole('button', { name: /^Delete / }).first();
    await expect(deleteButton).toBeVisible({ timeout: 10_000 });
    await deleteButton.click();

    // The delete does not happen until the dialog is confirmed.
    const dialog = page.getByRole('dialog', { name: 'Delete item' });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole('button', { name: 'Delete' })).toBeVisible();

    // Cancelling closes the dialog and leaves the row in place.
    await dialog.getByRole('button', { name: 'Cancel' }).click();
    await expect(dialog).not.toBeVisible();
    await expect(deleteButton).toBeVisible();
  });
});

/** Create `count` items named `<prefix> 01` ... through the API of the run; returns their ids. */
async function createItems(page: Page, prefix: string, count: number): Promise<number[]> {
  const ids: number[] = [];
  for (let index = 1; index <= count; index += 1) {
    const response = await page.request.post(`${BROWSER_API}/items`, {
      data: { name: `${prefix} ${String(index).padStart(2, '0')}`, price: index },
    });
    expect(response.ok()).toBeTruthy();
    const body = (await response.json()) as { id: number };
    ids.push(body.id);
  }
  return ids;
}

test.describe('Items list: sort, filter and page in the URL', () => {
  // The Makefile runs this spec with auth off. With auth on the page needs a sign-in first.
  test.skip(AUTH_RUN, 'auth on: the list needs a sign-in, this spec runs with auth off');

  test('keeps the sort, the filter and the page after a reload', async ({ page }) => {
    const prefix = `E2E-list-${Date.now()}`;
    const ids = await createItems(page, prefix, 12);
    try {
      await page.goto('/items?size=10');
      const filter = page.getByRole('searchbox', { name: 'Filter by name' });
      await filter.fill(prefix);
      await expect(page).toHaveURL(new RegExp(`[?&]q=${prefix}`), { timeout: 10_000 });
      await expect(page.getByText(/1.10 of 12/)).toBeVisible({ timeout: 10_000 });

      // Sort by name: ascending first, then descending.
      await page.getByRole('button', { name: 'Name' }).click();
      await expect(page).toHaveURL(/[?&]sort=name/);
      await expect(page.getByRole('row').nth(1).getByRole('cell').nth(1)).toHaveText(`${prefix} 01`);
      await page.getByRole('button', { name: 'Name' }).click();
      await expect(page).toHaveURL(/[?&]order=desc/);
      await expect(page.getByRole('columnheader', { name: 'Name' })).toHaveAttribute('aria-sort', 'descending');
      await expect(page.getByRole('row').nth(1).getByRole('cell').nth(1)).toHaveText(`${prefix} 12`);

      // Page 2 of the descending list holds the last two names.
      await page.getByRole('button', { name: 'Go to next page' }).click();
      await expect(page).toHaveURL(/[?&]page=2/);
      await expect(page.getByText(/11.12 of 12/)).toBeVisible({ timeout: 10_000 });
      await expect(page.getByRole('row').nth(1).getByRole('cell').nth(1)).toHaveText(`${prefix} 02`);

      // A reload restores the same state from the URL.
      const urlBeforeReload = page.url();
      await page.reload();
      await expect(page).toHaveURL(urlBeforeReload);
      await expect(filter).toHaveValue(prefix);
      await expect(page.getByRole('columnheader', { name: 'Name' })).toHaveAttribute('aria-sort', 'descending');
      await expect(page.getByText(/11.12 of 12/)).toBeVisible({ timeout: 10_000 });
      await expect(page.getByRole('row').nth(1).getByRole('cell').nth(1)).toHaveText(`${prefix} 02`);
    } finally {
      for (const id of ids) {
        await page.request.delete(`${BROWSER_API}/items/${id}`);
      }
    }
  });
});

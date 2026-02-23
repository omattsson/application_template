import { test, expect } from '@playwright/test';

test.describe('Home Page', () => {
  test('renders the welcome heading', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText(
      'Welcome to the Full Stack Application'
    );
  });

  test('renders the technology list', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByText('Go backend with Gin framework')).toBeVisible();
    await expect(page.getByText('React frontend with Material-UI')).toBeVisible();
    await expect(page.getByText('RESTful API architecture')).toBeVisible();
    await expect(page.getByText('Health monitoring')).toBeVisible();
    await expect(page.getByText('Swagger documentation')).toBeVisible();
  });

  test('renders introductory text', async ({ page }) => {
    await page.goto('/');
    await expect(
      page.getByText('This is a modern full-stack application built with:')
    ).toBeVisible();
  });
});

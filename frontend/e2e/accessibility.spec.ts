import { test } from '@playwright/test';
import { ADMIN_PASSWORD, MODES, checkPage, isAuthEnabled, type PageCheck } from './a11y';

// Colour contrast and structure (axe), in the light and the dark colour mode, at
// 1400x1000. See a11y.ts for the rules.
//
// - Items: with auth off it opens directly; with auth on it needs the local
//   administrator (ADMIN_USERNAME, ADMIN_PASSWORD) and is skipped without it.
// - Login: only exists with auth on (with auth off it redirects home).
// - Admin Users: needs auth on and the local administrator.
//
// A page that did not load must not pass: before axe runs, each test checks the
// heading, the URL, the colour mode, that no spinner is left and that no API
// call answered 5xx.

interface PageCase {
  name: string;
  spec: PageCheck;
  /** The page exists only with auth on. */
  needsAuth: boolean;
  /** The page needs the administrator password (it is for signed-in users). */
  needsSignIn: boolean;
}

const PAGES: PageCase[] = [
  { name: 'items', spec: { path: '/items', heading: 'Items' }, needsAuth: false, needsSignIn: false },
  { name: 'login', spec: { path: '/login', heading: 'Sign in' }, needsAuth: true, needsSignIn: false },
  {
    name: 'admin users',
    spec: { path: '/admin/users', heading: 'Users', signIn: true },
    needsAuth: true,
    needsSignIn: true,
  },
];

test.describe('Accessibility: colour contrast and structure', () => {
  test.use({ viewport: { width: 1400, height: 1000 } });

  for (const mode of MODES) {
    for (const { name, spec, needsAuth, needsSignIn } of PAGES) {
      test(`${name} (${mode})`, async ({ page }) => {
        const authEnabled = await isAuthEnabled(page);
        test.skip(needsAuth && !authEnabled, `${name} needs the backend with AUTH_ENABLED=true`);
        test.skip(
          authEnabled && (needsSignIn || spec.path === '/items') && ADMIN_PASSWORD === '',
          `${name} needs ADMIN_PASSWORD (the local administrator) with auth on`,
        );
        // With auth on, Items is for signed-in users too.
        await checkPage(page, mode, { ...spec, signIn: spec.signIn || (authEnabled && spec.path === '/items') });
      });
    }
  }
});

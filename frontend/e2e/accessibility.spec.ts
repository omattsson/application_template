import { test } from '@playwright/test';
import { MODES, checkPage, prepareRun, type PageCheck } from './a11y';
import type { DecisionInput } from './skipDecision';

// Colour contrast and structure (axe), in the light and the dark colour mode, at
// 1400x1000. See a11y.ts for the rules and the environment (ADMIN_USERNAME,
// ADMIN_PASSWORD, and A11Y_REQUIRE_AUTH=1 which turns a skip into a failure).
//
// - Items: with auth off it opens directly; with auth on it needs the local
//   administrator.
// - Login: only exists with auth on (with auth off it redirects home).
// - Admin Users: needs auth on and the local administrator.
// - Account API keys: needs auth on and the local administrator (a signed-in user).
// - Forgot password and Reset password: need auth on and PASSWORD_RESET_ENABLED.
//
// A page that did not load must not pass: before axe runs, each test checks the
// heading, the URL, the colour mode, that no spinner is left and that no API
// call answered 5xx.

interface PageCase {
  name: string;
  spec: PageCheck;
  needs: Pick<DecisionInput, 'needsAuth' | 'needsSignIn' | 'signInWhenAuthOn'>;
}

const PAGES: PageCase[] = [
  {
    name: 'items',
    spec: { path: '/items', heading: 'Items' },
    needs: { needsAuth: false, needsSignIn: false, signInWhenAuthOn: true },
  },
  {
    name: 'login',
    spec: { path: '/login', heading: 'Sign in' },
    needs: { needsAuth: true, needsSignIn: false, signInWhenAuthOn: false },
  },
  // The two reset pages exist only with PASSWORD_RESET_ENABLED on the backend and
  // a visitor who is not signed in (`make test-e2e-auth` turns the module on). The
  // reset page without a token shows its invalid-link state, which is checked here.
  {
    name: 'forgot password',
    spec: { path: '/forgot-password', heading: 'Forgot password' },
    needs: { needsAuth: true, needsSignIn: false, signInWhenAuthOn: false },
  },
  {
    name: 'reset password',
    spec: { path: '/reset-password', heading: 'Reset password' },
    needs: { needsAuth: true, needsSignIn: false, signInWhenAuthOn: false },
  },
  {
    name: 'admin users',
    spec: { path: '/admin/users', heading: 'Users' },
    needs: { needsAuth: true, needsSignIn: true, signInWhenAuthOn: true },
  },
  // The signed-in user's API keys: exists only with auth on, and needs a sign-in.
  {
    name: 'account api keys',
    spec: { path: '/account/api-keys', heading: 'API keys' },
    needs: { needsAuth: true, needsSignIn: true, signInWhenAuthOn: true },
  },
];

test.describe('Accessibility: colour contrast and structure', () => {
  test.use({ viewport: { width: 1400, height: 1000 } });

  for (const mode of MODES) {
    for (const { name, spec, needs } of PAGES) {
      test(`${name} (${mode})`, async ({ page }) => {
        const { signIn } = await prepareRun(page, needs);
        await checkPage(page, mode, { ...spec, signIn });
      });
    }
  }
});

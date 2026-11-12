import { defineConfig, devices } from '@playwright/test';

// `make test-e2e` (auth off) and `make test-e2e-auth` (AUTH_ENABLED=true) run
// this config through scripts/e2e.sh, which starts MySQL and the backend and
// sets the environment (see e2e/authEnv.ts):
//   E2E_FRONTEND_PORT  Port of the Vite dev server (default 3000).
//   VITE_API_URL       Backend that the dev server proxies /api and /ws to
//                      (default http://localhost:8081, vite.config.ts).
//   E2E_REUSE_SERVER=0 Never reuse a server that already listens on the port
//                      (it could proxy to another backend).
//   E2E_RETRIES        Retries of a failed test. Default: 2 on CI (CI=true), 0
//                      elsewhere. The CI auth runs set 0 so that a flaky sign-in
//                      spec fails the run instead of passing on the second try.
const port = Number(process.env.E2E_FRONTEND_PORT || 3000);
const baseURL = `http://localhost:${port}`;
const retriesEnv = process.env.E2E_RETRIES;
const retries = retriesEnv !== undefined && retriesEnv !== '' ? Number(retriesEnv) : process.env.CI ? 2 : 0;
if (!Number.isInteger(retries) || retries < 0) {
  throw new Error(`E2E_RETRIES must be a whole number >= 0, got "${retriesEnv}"`);
}

export default defineConfig({
  testDir: './e2e',
  globalSetup: './e2e/global-setup.ts',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries,
  workers: process.env.CI ? 1 : undefined,
  // The HTML report goes to PLAYWRIGHT_HTML_OUTPUT_DIR when it is set (one per mode).
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    baseURL,
    // With retries off a trace on the first retry never exists: keep the trace of a failure.
    trace: retries > 0 ? 'on-first-retry' : 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: {
    command: `npm run dev -- --port ${port} --strictPort`,
    url: baseURL,
    reuseExistingServer: !process.env.CI && process.env.E2E_REUSE_SERVER !== '0',
    timeout: 30_000,
  },
});

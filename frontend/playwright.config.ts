import { defineConfig, devices } from '@playwright/test';

// `make test-e2e` (auth off) and `make test-e2e-auth` (AUTH_ENABLED=true) run
// this config through scripts/e2e.sh, which starts MySQL and the backend and
// sets the environment (see e2e/authEnv.ts):
//   E2E_FRONTEND_PORT  Port of the Vite dev server (default 3000).
//   VITE_API_URL       Backend that the dev server proxies /api and /ws to
//                      (default http://localhost:8081, vite.config.ts).
//   E2E_REUSE_SERVER=0 Never reuse a server that already listens on the port
//                      (it could proxy to another backend).
const port = Number(process.env.E2E_FRONTEND_PORT || 3000);
const baseURL = `http://localhost:${port}`;

export default defineConfig({
  testDir: './e2e',
  globalSetup: './e2e/global-setup.ts',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  // The HTML report goes to PLAYWRIGHT_HTML_OUTPUT_DIR when it is set (one per mode).
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    baseURL,
    trace: 'on-first-retry',
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

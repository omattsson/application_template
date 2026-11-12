import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Backend that the dev server proxies /api and /ws to. The browser always
// calls the dev server (same-origin, so the httpOnly refresh cookie works);
// Compose sets VITE_API_URL=http://backend:8081, a backend on the host is the
// default.
const backendUrl = process.env.VITE_API_URL || 'http://localhost:8081';

export default defineConfig({
  plugins: [react()],
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: './src/test/setup.ts',
    css: true,
    exclude: ['e2e/**', 'node_modules/**'],
  },
  server: {
    port: 3000,
    host: true,
    // xfwd adds X-Forwarded-For/-Proto/-Host with the client address, so
    // the backend (which trusts this proxy through TRUSTED_PROXIES) rate
    // limits each client, not the dev server.
    proxy: {
      '/api': {
        target: backendUrl,
        changeOrigin: true,
        xfwd: true,
        rewrite: (path) => path.replace(/^\/api/, '')
      },
      '/ws': {
        target: backendUrl,
        ws: true,
        xfwd: true
      }
    }
  }
});

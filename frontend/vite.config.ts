import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react-swc';

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
        target: 'http://backend:8081',
        changeOrigin: true,
        xfwd: true,
        rewrite: (path) => path.replace(/^\/api/, '')
      },
      '/ws': {
        target: 'http://backend:8081',
        ws: true,
        xfwd: true
      }
    }
  }
});

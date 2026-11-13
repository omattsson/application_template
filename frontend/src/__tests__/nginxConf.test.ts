import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

/** Body of `location <path> { ... }` in the bundled nginx config (no nested blocks there). */
function locationBlock(conf: string, path: string): string {
  const start = conf.indexOf(`location ${path} {`);
  expect(start, `location ${path} exists`).toBeGreaterThanOrEqual(0);
  const end = conf.indexOf('}', start);
  return conf.slice(start, end);
}

describe('frontend/nginx.conf', () => {
  const conf = readFileSync(resolve(__dirname, '../../nginx.conf'), 'utf8');

  it('does not write the /ws access log (the browser sends the access token as ?token=)', () => {
    const ws = locationBlock(conf, '/ws');
    expect(ws).toMatch(/^\s*access_log\s+off;/m);
    // Errors are still logged (error_log is not turned off).
    expect(ws).not.toMatch(/error_log\s+off/);
  });

  it('keeps the /ws error log at crit (an upstream error logs the request line, token included)', () => {
    const ws = locationBlock(conf, '/ws');
    expect(ws).toMatch(/^\s*error_log\s+\/dev\/stderr\s+crit;/m);
  });

  // The framing and sniffing headers of the SPA. The backend sends the same
  // four on its own answers (SecurityHeaders middleware).
  describe('security headers', () => {
    const headers: Array<[string, string]> = [
      ['X-Content-Type-Options', 'nosniff'],
      ['X-Frame-Options', 'DENY'],
      ['Referrer-Policy', 'strict-origin-when-cross-origin'],
      ['Permissions-Policy', 'camera=(), microphone=(), geolocation=()'],
    ];
    const escape = (v: string) => v.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    /** The `server { ... }` body without its location blocks. */
    const serverLevel = conf.replace(/location\s+\S+\s*\{[^}]*\}/g, '');
    const locations = ['/', '/api/', '/ws'];

    it.each(headers)('sends %s at server level with "always"', (name, value) => {
      expect(serverLevel).toMatch(
        new RegExp(`^\\s*add_header\\s+${name}\\s+"${escape(value)}"\\s+always;`, 'm'),
      );
    });

    it('does not set add_header in a location: it would drop the server-level headers there', () => {
      for (const path of locations) {
        expect(locationBlock(conf, path), `location ${path}`).not.toMatch(/\badd_header\b/);
      }
    });

    it('sends the headers once on proxied answers (the backend sets them too)', () => {
      for (const path of ['/api/', '/ws']) {
        const block = locationBlock(conf, path);
        for (const [name] of headers) {
          expect(block, `location ${path} hides the backend ${name}`).toMatch(
            new RegExp(`^\\s*proxy_hide_header\\s+${name};`, 'm'),
          );
        }
      }
    });

    it('does not add a Content-Security-Policy yet (needs its own review)', () => {
      expect(conf).not.toMatch(/Content-Security-Policy/i);
    });
  });
});

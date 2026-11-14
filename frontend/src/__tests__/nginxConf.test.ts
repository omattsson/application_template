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
    const addHeaderRe = (name: string, value: string) =>
      new RegExp(`^\\s*add_header\\s+${name}\\s+"${escape(value)}"\\s+always;`, 'm');
    // Every kind of answer the server gives: the SPA shell, the SPA fallback,
    // hashed assets, the two proxied locations.
    const locations = ['/', '/index.html', '/assets/', '/api/', '/ws'];

    /**
     * The add_header lines in force for a location: nginx inherits the
     * server-level ones only when the location sets none of its own.
     */
    function effectiveHeaders(path: string): string {
      // /index.html has no location of its own: the longest matching prefix serves it.
      const served = [...conf.matchAll(/location\s+(\S+)\s*\{/g)]
        .map((m) => m[1])
        .filter((p) => path.startsWith(p))
        .sort((a, b) => b.length - a.length)[0];
      const own = locationBlock(conf, served ?? path);
      return /\badd_header\b/.test(own) ? own : serverLevel;
    }

    it.each(headers)('sends %s at server level with "always"', (name, value) => {
      expect(serverLevel).toMatch(addHeaderRe(name, value));
    });

    it.each(locations)(
      'location %s carries all four security headers (own add_header drops the server-level ones)',
      (path) => {
        const effective = effectiveHeaders(path);
        for (const [name, value] of headers) {
          expect(effective, `${name} on ${path}`).toMatch(addHeaderRe(name, value));
        }
      },
    );

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

  // Browser caching. A stale index.html after a redeploy points at hashed
  // files that no longer exist; with nosniff the fallback index.html served
  // for the missing script is refused: a blank page. So the shell is always
  // revalidated and only the hashed /assets/ files are cached for good.
  describe('caching', () => {
    /** The `map $uri $spa_cache_control { ... }` entries as [pattern, value]. */
    const mapMatch = /map\s+\$uri\s+\$spa_cache_control\s*\{([^}]*)\}/.exec(conf);
    const entries: Array<[string, string]> = (mapMatch?.[1] ?? '')
      .split(';')
      .map((l) => l.trim())
      .filter(Boolean)
      .map((l) => {
        const m = /^(\S+)\s+"([^"]*)"$/.exec(l);
        expect(m, `map entry "${l}"`).not.toBeNull();
        return [m![1], m![2]] as [string, string];
      });
    /** Cache-Control nginx would send for a $uri (first matching regex, else default). */
    function cacheControl(uri: string): string {
      for (const [pattern, value] of entries) {
        if (pattern !== 'default' && new RegExp(pattern.replace(/^~\*?/, '')).test(uri)) return value;
      }
      return entries.find(([p]) => p === 'default')?.[1] ?? '';
    }

    it('maps $uri to a Cache-Control value and sends it from the server level', () => {
      expect(mapMatch, 'map $uri $spa_cache_control exists').not.toBeNull();
      // No "always": error answers (404, 502) carry no cache header at all.
      expect(conf).toMatch(/^\s*add_header\s+Cache-Control\s+\$spa_cache_control;/m);
    });

    // "/" and SPA routes are served as /index.html after try_files, so $uri
    // is /index.html there.
    it.each(['/index.html', '/favicon.ico', '/manifest.json'])('revalidates %s (no-cache)', (uri) => {
      expect(cacheControl(uri)).toBe('no-cache');
    });

    it('caches the hashed /assets/ files for a year, immutable', () => {
      expect(cacheControl('/assets/index-Cv3Y5WwQ.js')).toBe('public, max-age=31536000, immutable');
    });

    it('adds no Cache-Control of its own on proxied answers (the backend decides)', () => {
      expect(cacheControl('/api/v1/items')).toBe('');
      expect(cacheControl('/ws')).toBe('');
    });

    it('answers a missing hashed asset with 404, not the SPA index.html', () => {
      expect(locationBlock(conf, '/assets/')).toMatch(/try_files\s+\$uri\s+=404;/);
    });
  });
});

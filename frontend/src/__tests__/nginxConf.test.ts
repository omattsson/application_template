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
});

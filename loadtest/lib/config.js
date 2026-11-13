// Shared settings of the k6 load tests: target URL, the safety guard and the
// thresholds. Every value comes from the environment (k6 exposes the process
// environment as __ENV; `make loadtest` sets the Make variables).
//
//   API_URL               backend base URL (default http://localhost:8081)
//   LOADTEST_ALLOW_REMOTE 1 to allow a target that is not on this machine
//   LOADTEST_VUS          virtual users of the load profile (default 10)
//   LOADTEST_DURATION     length of the steady phase, a k6 duration (default 30s)
//   LOADTEST_P95_MS       p95 latency threshold in ms (default 500)
//   LOADTEST_P99_MS       p99 latency threshold in ms (default 1500)
//   LOADTEST_ERROR_RATE   maximum error rate, 0 to 1 (default 0.01)

export const BASE_URL = (__ENV.API_URL || 'http://localhost:8081').replace(/\/+$/, '');
export const WS_URL = BASE_URL.replace(/^http/, 'ws') + '/ws';

export const VUS = positiveInt('LOADTEST_VUS', 10);
export const DURATION = __ENV.LOADTEST_DURATION || '30s';
export const P95_MS = positiveInt('LOADTEST_P95_MS', 500);
export const P99_MS = positiveInt('LOADTEST_P99_MS', 1500);
export const ERROR_RATE = rate('LOADTEST_ERROR_RATE', 0.01);

// Every item the tests create has this name prefix, so the teardown (and a
// human) can tell it from real data.
export const ITEM_PREFIX = 'k6-loadtest-';

function positiveInt(name, fallback) {
  const raw = __ENV[name];
  if (raw === undefined || raw === '') return fallback;
  const n = Number(raw);
  if (!Number.isInteger(n) || n <= 0) {
    throw new Error(`${name} must be a positive integer, got "${raw}"`);
  }
  return n;
}

function rate(name, fallback) {
  const raw = __ENV[name];
  if (raw === undefined || raw === '') return fallback;
  const n = Number(raw);
  if (!(n >= 0 && n <= 1)) {
    throw new Error(`${name} must be a number from 0 to 1, got "${raw}"`);
  }
  return n;
}

// isLocalTarget reports whether url points at this machine.
export function isLocalTarget(url) {
  const m = /^https?:\/\/(\[[0-9a-fA-F:]+\]|[^/:?#@]+)(?::\d+)?(?:[/?#]|$)/.exec(url);
  if (!m) return false;
  const host = m[1].toLowerCase();
  return (
    host === 'localhost' ||
    host.endsWith('.localhost') ||
    host === '[::1]' ||
    /^127\.\d+\.\d+\.\d+$/.test(host)
  );
}

// Safety guard, run when the script loads: a load test must never hit a shared
// or production backend by accident. A non-local URL needs an explicit
// LOADTEST_ALLOW_REMOTE=1. Throwing here makes k6 exit non-zero before it
// sends a request.
if (!isLocalTarget(BASE_URL) && __ENV.LOADTEST_ALLOW_REMOTE !== '1') {
  throw new Error(
    `refusing to load test ${BASE_URL}: it is not a local address. ` +
      'Set LOADTEST_ALLOW_REMOTE=1 only for a backend you own and may overload.',
  );
}

// Thresholds shared by the HTTP profiles. k6 exits non-zero when one fails.
export function httpThresholds() {
  return {
    errors: [`rate<${ERROR_RATE}`],
    checks: [`rate>${1 - ERROR_RATE}`],
    http_req_duration: [`p(95)<${P95_MS}`, `p(99)<${P99_MS}`],
  };
}

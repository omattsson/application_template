// k6 STRESS profile of the Items API: ramps the load up in steps to find where
// latency or errors start to climb. It is opt-in (`make loadtest-stress`), never
// part of `make loadtest`, because it writes a lot of items and can overload a
// small machine. Every item it creates is deleted again.
//
// The backend must run with a high RATE_LIMIT (the default is 100 requests per
// minute per IP), for example RATE_LIMIT=1000000.
//
// Knobs: LOADTEST_STRESS_VUS (peak, default 100), LOADTEST_STRESS_STEP (length
// of each step, default 30s) and the thresholds of lib/config.js, which are
// looser here: p95 and p99 are three times LOADTEST_P95_MS and LOADTEST_P99_MS.

import http from 'k6/http';
import { check } from 'k6';
import { Rate } from 'k6/metrics';
import { BASE_URL, ITEM_PREFIX, ERROR_RATE, P95_MS, P99_MS } from './lib/config.js';
import { setupAuth, headers, teardownItems } from './lib/auth.js';

const errors = new Rate('errors');

function positiveInt(name, fallback) {
  const raw = __ENV[name];
  if (raw === undefined || raw === '') return fallback;
  const n = Number(raw);
  if (!Number.isInteger(n) || n <= 0) throw new Error(`${name} must be a positive integer, got "${raw}"`);
  return n;
}

const PEAK = positiveInt('LOADTEST_STRESS_VUS', 100);
const STEP = __ENV.LOADTEST_STRESS_STEP || '30s';

export const options = {
  scenarios: {
    stress: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: STEP, target: Math.ceil(PEAK / 4) },
        { duration: STEP, target: Math.ceil(PEAK / 2) },
        { duration: STEP, target: PEAK },
        { duration: STEP, target: PEAK },
        { duration: STEP, target: 0 },
      ],
      gracefulRampDown: '10s',
    },
  },
  thresholds: {
    errors: [`rate<${ERROR_RATE * 5}`],
    http_req_duration: [`p(95)<${P95_MS * 3}`, `p(99)<${P99_MS * 3}`],
  },
};

export function setup() {
  return setupAuth();
}

export function teardown(auth) {
  teardownItems(auth);
}

export default function (auth) {
  const p = (name) => ({ headers: headers(auth), tags: { name } });

  // 80% reads, 20% a create and delete pair.
  if (Math.random() < 0.8) {
    const res = http.get(`${BASE_URL}/api/v1/items?limit=50`, p('GET /items'));
    errors.add(!check(res, { 'list items: 200': (r) => r.status === 200 }));
    return;
  }
  const create = http.post(
    `${BASE_URL}/api/v1/items`,
    JSON.stringify({ name: `${ITEM_PREFIX}stress-${__VU}-${__ITER}-${Date.now()}`, price: 5 }),
    p('POST /items'),
  );
  const created = check(create, { 'create item: 201': (r) => r.status === 201 });
  errors.add(!created);
  if (created) {
    const del = http.del(`${BASE_URL}/api/v1/items/${create.json('id')}`, null, p('DELETE /items/:id'));
    errors.add(!check(del, { 'delete item: 204': (r) => r.status === 204 }));
  }
}

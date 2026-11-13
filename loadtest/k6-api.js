// k6 load test of the Items API.
//
// Run it with `make loadtest` (see docs/claude/operations.md, "Load tests").
// By hand:  k6 run loadtest/k6-api.js
//           k6 run --env SCENARIO=crud loadtest/k6-api.js
//
// Scenarios (SCENARIO=browse|crud, default both):
//   browse  read-only: list (paged and filtered) and get one item
//   crud    create, read, update, delete one item per iteration
//
// Thresholds (k6 exits non-zero when one is crossed): error rate, check
// pass rate, p95 and p99 latency; see lib/config.js for the knobs. Auth and the
// safety guard (local targets only) are in lib/auth.js and lib/config.js.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Counter } from 'k6/metrics';
import { BASE_URL, VUS, DURATION, ITEM_PREFIX, httpThresholds } from './lib/config.js';
import { setupAuth, headers, teardownItems } from './lib/auth.js';

const errors = new Rate('errors');
const itemsCreated = new Counter('items_created');

const SCENARIO = __ENV.SCENARIO || 'all';
const crudVUs = Math.max(1, Math.floor(VUS / 2));

const scenarios = {
  browse: { executor: 'constant-vus', exec: 'browse', vus: VUS, duration: DURATION },
  crud: { executor: 'constant-vus', exec: 'crud', vus: crudVUs, duration: DURATION },
};
if (SCENARIO !== 'all' && !(SCENARIO in scenarios)) {
  throw new Error(`SCENARIO must be all, browse or crud, got "${SCENARIO}"`);
}
if (SCENARIO !== 'all') {
  for (const name of Object.keys(scenarios)) if (name !== SCENARIO) delete scenarios[name];
}

export const options = {
  scenarios,
  thresholds: httpThresholds(),
};

export function setup() {
  return setupAuth();
}

export function teardown(auth) {
  teardownItems(auth);
}

// ok records one request against the error rate. 'expected' is the status.
function ok(res, label, expected) {
  const pass = check(res, { [`${label}: status ${expected}`]: (r) => r.status === expected });
  errors.add(!pass);
  return pass;
}

function params(auth, name) {
  return { headers: headers(auth), tags: { name } };
}

export function browse(auth) {
  const list = http.get(`${BASE_URL}/api/v1/items?limit=20`, params(auth, 'GET /items'));
  if (!ok(list, 'list items', 200)) {
    sleep(1);
    return;
  }
  const items = list.json();
  check(list, { 'list is an array': () => Array.isArray(items) });

  const filtered = http.get(
    `${BASE_URL}/api/v1/items?limit=20&offset=0&min_price=0&max_price=1000000&name=${ITEM_PREFIX}`,
    params(auth, 'GET /items (filtered)'),
  );
  ok(filtered, 'filtered list', 200);

  if (items.length > 0) {
    const item = items[Math.floor(Math.random() * items.length)];
    const one = http.get(`${BASE_URL}/api/v1/items/${item.id}`, params(auth, 'GET /items/:id'));
    ok(one, 'get item', 200);
  }
  sleep(0.2 + Math.random() * 0.3);
}

export function crud(auth) {
  const name = `${ITEM_PREFIX}${__VU}-${__ITER}-${Date.now()}`;
  const create = http.post(
    `${BASE_URL}/api/v1/items`,
    JSON.stringify({ name, price: 9.99 }),
    params(auth, 'POST /items'),
  );
  if (!ok(create, 'create item', 201)) {
    sleep(1);
    return;
  }
  itemsCreated.add(1);
  const item = create.json();

  const read = http.get(`${BASE_URL}/api/v1/items/${item.id}`, params(auth, 'GET /items/:id'));
  ok(read, 'read item', 200);

  const update = http.put(
    `${BASE_URL}/api/v1/items/${item.id}`,
    JSON.stringify({ name: `${name}-updated`, price: 19.99, version: item.version }),
    params(auth, 'PUT /items/:id'),
  );
  ok(update, 'update item', 200);

  const del = http.del(`${BASE_URL}/api/v1/items/${item.id}`, null, params(auth, 'DELETE /items/:id'));
  ok(del, 'delete item', 204);

  // A 404 is the expected answer here: do not count it in http_req_failed.
  const gone = http.get(`${BASE_URL}/api/v1/items/${item.id}`, {
    ...params(auth, 'GET /items/:id (deleted)'),
    responseCallback: http.expectedStatuses(404),
  });
  const pass = check(gone, { 'deleted item is gone (404)': (r) => r.status === 404 });
  errors.add(!pass);
  sleep(0.2 + Math.random() * 0.3);
}

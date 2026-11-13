// Authentication of the load tests. The backend has auth off by default
// (AUTH_ENABLED=false): then no credential is needed. With auth on, give ONE of
//
//   API_TOKEN       a JWT access token (bearer). It expires after
//                   ACCESS_TOKEN_EXPIRATION (15m by default): fine for a short
//                   run; for a longer one use API_KEY or the login below.
//   API_KEY         an API key (sk_...), sent as X-API-Key. It does not
//                   expire with the login session.
//   ADMIN_PASSWORD  log in once in setup() (user ADMIN_USERNAME, default
//                   admin) and reuse the token for every VU, so bcrypt and the
//                   login rate limit are not part of the measurement.
//
// No credential has a default and none is ever printed.

import http from 'k6/http';
import { BASE_URL, ITEM_PREFIX } from './config.js';

const JSON_HEADERS = { 'Content-Type': 'application/json' };

// setupAuth checks that the backend answers, finds out whether auth is on and
// returns {token, apiKey, authEnabled}. Call it from setup(); it throws (k6
// aborts the run) with a message that says what to fix.
export function setupAuth() {
  const live = http.get(`${BASE_URL}/health/live`, { tags: { name: 'setup' } });
  if (live.status !== 200) {
    throw new Error(
      `the backend at ${BASE_URL} is not reachable (GET /health/live answered ${live.status}). ` +
        'Start it (make dev-backend) or set API_URL.',
    );
  }

  const cfgRes = http.get(`${BASE_URL}/api/v1/ui-config`, { tags: { name: 'setup' } });
  if (cfgRes.status === 429) {
    throw new Error('the backend rate-limits this client (429): start it with a high RATE_LIMIT, for example RATE_LIMIT=1000000');
  }
  if (cfgRes.status !== 200) {
    throw new Error(`GET /api/v1/ui-config answered ${cfgRes.status}`);
  }
  const authEnabled = cfgRes.json('auth_enabled') === true;

  const apiKey = __ENV.API_KEY || '';
  let token = __ENV.API_TOKEN || '';
  if (authEnabled && !token && !apiKey) {
    const password = __ENV.ADMIN_PASSWORD || '';
    if (!password) {
      throw new Error(
        'the backend has AUTH_ENABLED=true: set API_TOKEN, API_KEY, or ADMIN_PASSWORD (and ADMIN_USERNAME if it is not "admin")',
      );
    }
    const res = http.post(
      `${BASE_URL}/api/v1/auth/login`,
      JSON.stringify({ username: __ENV.ADMIN_USERNAME || 'admin', password }),
      // X-Requested-With is the CSRF header the login needs with refresh
      // sessions on (any value).
      { headers: { ...JSON_HEADERS, 'X-Requested-With': 'k6' }, tags: { name: 'setup' } },
    );
    if (res.status !== 200) {
      throw new Error(`login failed (status ${res.status})`);
    }
    token = res.json('token');
  }
  return { token, apiKey, authEnabled };
}

// headers returns the request headers for the credential of setupAuth().
export function headers(auth, extra) {
  const h = { ...JSON_HEADERS, ...(extra || {}) };
  if (auth && auth.apiKey) h['X-API-Key'] = auth.apiKey;
  else if (auth && auth.token) h.Authorization = `Bearer ${auth.token}`;
  return h;
}

// teardownItems deletes the items that a failed iteration left behind (names
// that start with ITEM_PREFIX). Best effort: it never throws.
export function teardownItems(auth) {
  try {
    const params = { headers: headers(auth), tags: { name: 'teardown' } };
    for (let round = 0; round < 10; round++) {
      const res = http.get(`${BASE_URL}/api/v1/items?name=${ITEM_PREFIX}&limit=100`, params);
      if (res.status !== 200) return;
      const items = res.json().filter((i) => String(i.name).startsWith(ITEM_PREFIX));
      if (items.length === 0) return;
      for (const item of items) http.del(`${BASE_URL}/api/v1/items/${item.id}`, null, params);
    }
  } catch (e) {
    console.warn(`teardown: cleanup skipped (${e})`);
  }
}

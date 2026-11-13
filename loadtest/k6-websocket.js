// k6 load test of the WebSocket endpoint /ws.
//
// Run it with `make loadtest`. By hand: k6 run loadtest/k6-websocket.js
//
// Two scenarios run together:
//   listeners  LOADTEST_VUS connections that subscribe to the items topic and
//              stay open for a few seconds each
//   writer     one VU that creates and deletes an item at a low rate, so the
//              hub broadcasts item.created / item.deleted to every listener
//
// With auth on, the credential of lib/auth.js is used: an access token goes in
// the ?token= query (the way a browser does it), an API key in X-API-Key.
// Thresholds: connect time, connection error rate, and at least one broadcast
// received.

import ws from 'k6/ws';
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Counter, Trend } from 'k6/metrics';
import { BASE_URL, WS_URL, VUS, DURATION, ITEM_PREFIX, ERROR_RATE, P95_MS } from './lib/config.js';
import { setupAuth, headers, teardownItems } from './lib/auth.js';

const wsErrors = new Rate('ws_errors');
const wsConnections = new Counter('ws_connections');
const wsMessages = new Counter('ws_messages_received');
const wsConnectTime = new Trend('ws_connect_duration', true);

const HOLD_MS = 5000;

export const options = {
  scenarios: {
    listeners: { executor: 'constant-vus', exec: 'listener', vus: VUS, duration: DURATION },
    writer: { executor: 'constant-vus', exec: 'writer', vus: 1, duration: DURATION },
  },
  thresholds: {
    ws_errors: [`rate<${ERROR_RATE}`],
    ws_connect_duration: [`p(95)<${P95_MS}`],
    ws_messages_received: ['count>0'],
  },
};

export function setup() {
  return setupAuth();
}

export function teardown(auth) {
  teardownItems(auth);
}

export function listener(auth) {
  let url = WS_URL;
  const params = { headers: {} };
  if (auth.apiKey) {
    // The API key is accepted in the header only.
    params.headers['X-API-Key'] = auth.apiKey;
  } else if (auth.token) {
    url = `${WS_URL}?token=${encodeURIComponent(auth.token)}`;
  }

  const started = Date.now();
  const res = ws.connect(url, params, (socket) => {
    socket.on('open', () => {
      wsConnections.add(1);
      wsConnectTime.add(Date.now() - started);
      socket.send(JSON.stringify({ type: 'subscribe', payload: { topic: 'items' } }));
      socket.setTimeout(() => socket.close(), HOLD_MS);
    });
    socket.on('message', (msg) => {
      wsMessages.add(1);
      let parsed = null;
      try {
        parsed = JSON.parse(msg);
      } catch (e) {
        // not JSON: the check below fails it
      }
      check(parsed, { 'message has a type': (m) => m !== null && typeof m.type === 'string' });
    });
    socket.on('error', () => wsErrors.add(true));
  });

  const pass = check(res, { 'upgrade answered 101': (r) => r && r.status === 101 });
  wsErrors.add(!pass);
}

export function writer(auth) {
  const p = { headers: headers(auth), tags: { name: 'ws writer' } };
  const created = http.post(
    `${BASE_URL}/api/v1/items`,
    JSON.stringify({ name: `${ITEM_PREFIX}ws-${__ITER}-${Date.now()}`, price: 1 }),
    p,
  );
  if (created.status === 201) {
    http.del(`${BASE_URL}/api/v1/items/${created.json('id')}`, null, p);
  }
  sleep(1);
}

/**
 * Base URL of the API. Always same-origin: the dev server (Vite proxy) and
 * nginx in production forward `/api/*` to the backend and strip the `/api`
 * prefix, so `/api` + `/api/v1/items` reaches the backend as `/api/v1/items`.
 * Same-origin calls are required with auth on: the httpOnly refresh cookie
 * (SameSite=Strict, Path=/) is only sent to the origin of the page.
 */
export const API_BASE_URL = '/api';

export const endpoints = {
  health: {
    live: `${API_BASE_URL}/health/live`,
    ready: `${API_BASE_URL}/health/ready`,
  }
};

export const axiosConfig = {
  baseURL: API_BASE_URL,
  headers: {
    'Content-Type': 'application/json',
  },
};

// WebSocket base URL, derived from the page origin. The dev server and nginx
// proxy `/ws` to the backend.
export const WS_BASE_URL = `${window.location.protocol === 'https:' ? 'wss' : 'ws'}://${window.location.host}`;

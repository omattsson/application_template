# API Integration Layer

This directory contains the API client configuration and service implementations for communicating with the backend service.

## Overview

The API layer provides:

- 🔧 Centralized API configuration
- 🌐 Axios client setup with interceptors
- 🚦 Health check service integration
- ⚠️ Global error handling
- 🔄 Request/response interceptors
- 📝 TypeScript types for API responses

## Directory Structure

```text
api/
├── client.ts   # Axios client setup and interceptors
├── config.ts   # API configuration and endpoints
├── errorSummary.ts # summarizeRequestError: the only safe way to log a failed request
├── refresh.ts  # Access token refresh (single flight, cross-tab Web Lock, never aborted)
├── session.ts  # In-memory session (auth mode, access token) shared across tabs
└── README.md   # This documentation
```

## Configuration

The API configuration is managed in `config.ts`. The API is always called
same-origin: the Vite dev proxy and nginx forward `/api/*` to the backend and
strip the `/api` prefix.

```typescript
export const API_BASE_URL = '/api';

export const endpoints = {
  health: {
    live: `${API_BASE_URL}/health/live`,
    ready: `${API_BASE_URL}/health/ready`,
  }
};
```

## API Client Setup

The Axios client is configured in `client.ts` with:

- Base URL configuration
- Default headers
- Request/response interceptors
- Error handling

```typescript
import axios from 'axios';
import { axiosConfig } from './config';

// config.ts: axiosConfig = { baseURL: API_BASE_URL, headers: { 'Content-Type': 'application/json' } }
const api = axios.create(axiosConfig);
export default api; // the services below and your own calls use it
```

## Available Services

### Health Service

Methods for checking backend health status:

```typescript
export const healthService = {
  checkLiveness: async () => {
    try {
      const response = await api.get('/health/live');
      return response.data;
    } catch (error) {
      console.error('Liveness check failed:', summarizeRequestError(error));
      throw error;
    }
  },
  // checkReadiness is the same for /health/ready
};
```

The other service objects (`itemService`, `authService`, `userService`,
`uiConfigService`) live in `client.ts` too.

## Error Handling

Global error handling through an Axios response interceptor in `client.ts`.
A 401 with auth on is refreshed once and retried; every other failure is
logged with `summarizeRequestError` and rejected to the caller.

Never pass the error object itself to `console.error`: an
AxiosError carries the request config, so the log would contain the body (a
login password) and the headers (the `Authorization: Bearer` token).
`summarizeRequestError` keeps only the method, the URL path without its
query, the HTTP status and the axios error code.

```typescript
import { summarizeRequestError } from './errorSummary';

api.interceptors.response.use(
  (response) => response,
  async (error) => {
    // ... 401 refresh and retry (see client.ts) ...
    console.error('API Error:', summarizeRequestError(error));
    // logs e.g. "POST /api/v1/auth/login -> 401 ERR_BAD_REQUEST"
    return Promise.reject(error);
  }
);
```

The same rule applies in a service or a component: log
`summarizeRequestError(error)`, not `error`.

## Usage Example

Using the API client in components:

```typescript
import { healthService } from '@/api/client';
import { summarizeRequestError } from '@/api/errorSummary';

const HealthCheck = () => {
  const [status, setStatus] = useState<string>('');

  const checkHealth = async () => {
    try {
      const result = await healthService.checkLiveness();
      setStatus(result.status);
    } catch (error) {
      console.error('Health check failed:', summarizeRequestError(error));
    }
  };
};
```

## Type Safety

TypeScript interfaces for API responses:

```typescript
interface HealthResponse {
  status: string;
  timestamp: string;
}

interface ErrorResponse {
  error: string;
  message: string;
  status: number;
}
```

## Best Practices

1. Always use the centralized API client for requests
2. Handle errors appropriately at the service level
3. Use TypeScript interfaces for request/response types
4. Keep endpoint definitions in `config.ts`
5. Add interceptors for common operations (auth, logging)

## Adding New Services

When adding a new service:

1. Add endpoint configuration to `config.ts`
2. Create TypeScript interfaces for the data
3. Implement the service methods using `api` and log failures with `summarizeRequestError`
4. Add error handling specific to the service
5. Document the new service in this README

## Environment Configuration

The browser code has no backend address: it always calls `/api` on its own
origin. Only the Vite dev proxy has a target, `VITE_API_URL` (default
`http://localhost:8081`; Compose sets `http://backend:8081`). In production
nginx proxies `/api/` and `/ws`.

## Authentication (optional)

With `AUTH_ENABLED=false` on the backend (the default) nothing changes: no
login, no `Authorization` header, no refresh calls. With auth on:

- The access token lives in memory only (`session.ts`), never in
  localStorage or sessionStorage. The refresh token is an httpOnly cookie
  the SPA cannot read.
- The API client adds `Authorization: Bearer <token>`. A 401 refreshes once
  (concurrent 401s share one refresh) and retries; a refused refresh ends
  the session and the app shows `/login`.
- `refreshAccessToken` (`refresh.ts`) holds the Web Lock `auth-refresh`
  across tabs and adopts a token another tab shared on the `app-template:auth-session:v1`
  BroadcastChannel instead of refreshing again. Refresh requests are never
  aborted (no timeout, no AbortSignal).
- Login, refresh and logout send `X-Requested-With: XMLHttpRequest` and a
  JSON body.

import axios from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import { axiosConfig } from './config';
import { authApi, CSRF_HEADERS, refreshAccessToken } from './refresh';
import { getAccessToken, getSession } from './session';
// Every log line uses this summary: an AxiosError carries the password body and the bearer header.
import { summarizeRequestError } from './errorSummary';
import type { UIConfig } from '../context/UIConfigContext';
import { toRole } from '../utils/jwt';
import type { AuthUser } from '../utils/jwt';

// Marks a request that was already retried after a refresh (no second retry).
declare module 'axios' {
  interface InternalAxiosRequestConfig {
    _authRetry?: boolean;
  }
}

const api = axios.create(axiosConfig);

/** Read the bearer token that a request was sent with, or null. */
function bearerOf(config: InternalAxiosRequestConfig): string | null {
  const header: unknown = config.headers?.Authorization;
  return typeof header === 'string' && header.startsWith('Bearer ') ? header.slice('Bearer '.length) : null;
}

// With auth on, every API request carries the in-memory access token. With
// auth off there is never a token, so no Authorization header is sent.
api.interceptors.request.use((config) => {
  const token = getAccessToken();
  if (token && !config.headers.Authorization) {
    config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});

// A 401 with auth on refreshes the token once (concurrent 401s share one
// refresh, see refreshAccessToken) and retries the request once. A refused
// refresh has ended the session; ProtectedRoute then shows the login page.
api.interceptors.response.use(
  (response) => response,
  async (error) => {
    const config: InternalAxiosRequestConfig | undefined = error?.config;
    if (error?.response?.status === 401 && config && !config._authRetry && getSession().mode === 'enabled') {
      config._authRetry = true;
      let token: string;
      try {
        token = await refreshAccessToken(bearerOf(config));
      } catch {
        return Promise.reject(error);
      }
      config.headers.Authorization = `Bearer ${token}`;
      return api.request(config);
    }
    console.error('API Error:', summarizeRequestError(error));
    return Promise.reject(error);
  }
);

export interface Item {
  id: number;
  name: string;
  price: number;
  description?: string;
  created_at: string;
  updated_at: string;
}

export const itemService = {
  list: async (limit?: number, offset?: number): Promise<Item[]> => {
    try {
      const params = new URLSearchParams();
      if (limit !== undefined) params.set('limit', String(limit));
      if (offset !== undefined) params.set('offset', String(offset));
      const query = params.toString() ? `?${params.toString()}` : '';
      const response = await api.get<Item[]>(`/api/v1/items${query}`);
      return response.data;
    } catch (error) {
      console.error('Failed to fetch items:', summarizeRequestError(error));
      throw error;
    }
  },

  get: async (id: number): Promise<Item> => {
    try {
      const response = await api.get<Item>(`/api/v1/items/${id}`);
      return response.data;
    } catch (error) {
      console.error('Failed to fetch item:', summarizeRequestError(error));
      throw error;
    }
  },

  create: async (item: Omit<Item, 'id' | 'created_at' | 'updated_at'>): Promise<Item> => {
    try {
      const response = await api.post<Item>('/api/v1/items', item);
      return response.data;
    } catch (error) {
      console.error('Failed to create item:', summarizeRequestError(error));
      throw error;
    }
  },

  update: async (id: number, item: Partial<Omit<Item, 'id' | 'created_at' | 'updated_at'>>): Promise<Item> => {
    try {
      const response = await api.put<Item>(`/api/v1/items/${id}`, item);
      return response.data;
    } catch (error) {
      console.error('Failed to update item:', summarizeRequestError(error));
      throw error;
    }
  },

  delete: async (id: number): Promise<void> => {
    try {
      await api.delete(`/api/v1/items/${id}`);
    } catch (error) {
      console.error('Failed to delete item:', summarizeRequestError(error));
      throw error;
    }
  },
};

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

  checkReadiness: async () => {
    try {
      const response = await api.get('/health/ready');
      return response.data;
    } catch (error) {
      console.error('Readiness check failed:', summarizeRequestError(error));
      throw error;
    }
  },
};

/** Answer of POST /api/v1/auth/login. */
export interface LoginResponse {
  token: string;
  /** The user of the answer; null when it is missing or malformed (the UI then decodes the token). */
  user: AuthUser | null;
}

/** Narrow the `user` object of the login answer to the display user. */
function toAuthUser(value: unknown): AuthUser | null {
  if (!value || typeof value !== 'object') return null;
  const u = value as Record<string, unknown>;
  if (typeof u.id !== 'string' || typeof u.username !== 'string' || !u.username) return null;
  const user: AuthUser = {
    id: u.id,
    username: u.username,
    display_name: typeof u.display_name === 'string' && u.display_name ? u.display_name : u.username,
    role: toRole(u.role),
  };
  if (typeof u.email === 'string' && u.email) user.email = u.email;
  if (typeof u.auth_provider === 'string' && u.auth_provider) user.auth_provider = u.auth_provider;
  return user;
}

/**
 * Session endpoints (used with auth on only). They go through the auth
 * client: cookies included, the CSRF header set, never aborted, and no 401
 * interceptor. The refresh itself is `refreshAccessToken` in `./refresh`.
 */
export const authService = {
  /**
   * Sign in with a username and password. Starts a refresh session (httpOnly cookie).
   * @param username - Login name
   * @param password - Password
   * @returns The access token and the user
   * @see POST /api/v1/auth/login
   */
  login: async (username: string, password: string): Promise<LoginResponse> => {
    try {
      const { data } = await authApi.post<{ token?: unknown; user?: unknown } | null>(
        '/api/v1/auth/login',
        { username, password },
        { headers: CSRF_HEADERS },
      );
      if (!data || typeof data !== 'object' || typeof data.token !== 'string' || data.token === '') {
        throw new Error('The login answer has no access token');
      }
      return { token: data.token, user: toAuthUser(data.user) };
    } catch (error) {
      console.error('Login failed:', summarizeRequestError(error));
      throw error;
    }
  },

  /**
   * End the session of the refresh cookie and of the given access token.
   * @param token - The access token of the session, or null
   * @returns Nothing
   * @see POST /api/v1/auth/logout
   */
  logout: async (token: string | null): Promise<void> => {
    try {
      await authApi.post('/api/v1/auth/logout', {}, {
        headers: token ? { ...CSRF_HEADERS, Authorization: `Bearer ${token}` } : { ...CSRF_HEADERS },
      });
    } catch (error) {
      console.error('Logout failed:', summarizeRequestError(error));
      throw error;
    }
  },
};

/** Check the shape of a UI config answer. */
function isUIConfig(value: unknown): value is UIConfig {
  if (!value || typeof value !== 'object') return false;
  const c = value as Record<string, unknown>;
  return (
    typeof c.title === 'string' &&
    typeof c.auth_enabled === 'boolean' &&
    typeof c.oidc_enabled === 'boolean' &&
    typeof c.local_auth_enabled === 'boolean'
  );
}

/** Public display config of the web UI. */
export const uiConfigService = {
  /**
   * Read the UI config (title and auth flags). Public in both auth modes.
   * @returns The UI config
   * @throws When the request fails or the answer is not a UI config (for example index.html from a misrouted proxy)
   * @see GET /api/v1/ui-config
   */
  get: async (): Promise<UIConfig> => {
    try {
      const { data } = await api.get<unknown>('/api/v1/ui-config');
      if (!isUIConfig(data)) throw new Error('The UI config answer is not a UI config');
      return {
        title: data.title,
        auth_enabled: data.auth_enabled,
        oidc_enabled: data.oidc_enabled,
        local_auth_enabled: data.local_auth_enabled,
      };
    } catch (error) {
      console.error('Failed to load the UI config:', summarizeRequestError(error));
      throw error;
    }
  },
};

export default api;

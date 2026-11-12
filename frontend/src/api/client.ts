import axios from 'axios';
import type { InternalAxiosRequestConfig } from 'axios';
import { axiosConfig } from './config';
import { authApi, CSRF_HEADERS, refreshAccessToken } from './refresh';
import { getAccessToken, getSession } from './session';
// Every log line uses this summary: an AxiosError carries the password body and the bearer header.
import { summarizeRequestError } from './errorSummary';
import type { UIConfig } from '../context/UIConfigContext';
import { toRole } from '../utils/jwt';
import type { AuthUser, Role } from '../utils/jwt';

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

/** A user as the user administration returns it (never with a password hash). */
export interface User {
  id: string;
  username: string;
  email: string;
  display_name: string;
  role: Role;
  /** `local` for a password account, otherwise the name of the identity provider. */
  auth_provider: string;
  disabled: boolean;
  service_account: boolean;
  created_at: string;
  updated_at: string;
}

/** One page of users (GET /api/v1/users). `page` starts at 1. */
export interface UserListResponse {
  data: User[];
  total: number;
  page: number;
  pageSize: number;
}

/** Body of POST /api/v1/users. An empty display name defaults to the username on the server. */
export interface CreateUserRequest {
  username: string;
  email: string;
  display_name: string;
  password: string;
  role: Role;
}

/** Answer of PUT /api/v1/users/:id/role. */
export interface ChangeRoleResult {
  id: string;
  old_role: string;
  new_role: string;
  message: string;
  /** False when the user already had the role. */
  changed: boolean;
  /** True for an SSO user: the identity provider may override the role at the next login. */
  idp_managed: boolean;
}

/** Answer of PUT /api/v1/users/:id/disabled. */
export interface SetDisabledResult {
  id: string;
  message: string;
  disabled: boolean;
  /** False when the user already had that state. */
  changed: boolean;
}

/**
 * User administration (admin role and a login session; the API refuses
 * everything else with 403). A change that would leave no enabled admin is
 * answered with 409 and a message.
 */
export const userService = {
  /**
   * List one page of users, ordered by username.
   * @param page - Page number, from 1
   * @param pageSize - Users per page (the server default is 25, the maximum 100)
   * @returns The page and the total number of users
   * @see GET /api/v1/users
   */
  list: async (page: number, pageSize: number): Promise<UserListResponse> => {
    try {
      const query = new URLSearchParams({ page: String(page), pageSize: String(pageSize) });
      const response = await api.get<UserListResponse>(`/api/v1/users?${query.toString()}`);
      return response.data;
    } catch (error) {
      console.error('Failed to list users:', summarizeRequestError(error));
      throw error;
    }
  },

  /**
   * Create a local user.
   * @param user - The new user (the password is sent once and never logged)
   * @returns The created user
   * @see POST /api/v1/users
   */
  create: async (user: CreateUserRequest): Promise<User> => {
    try {
      const response = await api.post<User>('/api/v1/users', user);
      return response.data;
    } catch (error) {
      console.error('Failed to create a user:', summarizeRequestError(error));
      throw error;
    }
  },

  /**
   * Change the role of a user. Ends the sessions of that user.
   * @param id - User ID
   * @param role - The new role
   * @returns The old and the new role
   * @see PUT /api/v1/users/:id/role
   */
  changeRole: async (id: string, role: Role): Promise<ChangeRoleResult> => {
    try {
      const response = await api.put<ChangeRoleResult>(`/api/v1/users/${encodeURIComponent(id)}/role`, { role });
      return response.data;
    } catch (error) {
      console.error('Failed to change the role of a user:', summarizeRequestError(error));
      throw error;
    }
  },

  /**
   * Disable or enable a user. Disabling ends the sessions of that user.
   * @param id - User ID
   * @param disabled - True to disable, false to enable
   * @returns The new state
   * @see PUT /api/v1/users/:id/disabled
   */
  setDisabled: async (id: string, disabled: boolean): Promise<SetDisabledResult> => {
    try {
      const response = await api.put<SetDisabledResult>(`/api/v1/users/${encodeURIComponent(id)}/disabled`, { disabled });
      return response.data;
    } catch (error) {
      console.error('Failed to change the disabled state of a user:', summarizeRequestError(error));
      throw error;
    }
  },

  /**
   * Delete a user permanently.
   * @param id - User ID
   * @returns Nothing
   * @see DELETE /api/v1/users/:id
   */
  delete: async (id: string): Promise<void> => {
    try {
      await api.delete(`/api/v1/users/${encodeURIComponent(id)}`);
    } catch (error) {
      console.error('Failed to delete a user:', summarizeRequestError(error));
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

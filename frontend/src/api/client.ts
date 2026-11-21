import axios, { AxiosHeaders } from 'axios';
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

/**
 * Client of the public password reset routes. It sends no bearer header and
 * no cookie, and it has no 401 interceptor: a refused reset call must never
 * start a token refresh. The routes set no cookie and read none.
 */
export const unauthenticatedApi = axios.create(axiosConfig);

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

/** Columns that the items list can be sorted by (the API whitelist). */
export type ItemSortKey = 'id' | 'name' | 'price' | 'created_at' | 'updated_at';

/** Sort direction of the items list. */
export type SortOrder = 'asc' | 'desc';

/** Query of the items list. Every field is optional; a missing field is not sent. */
export interface ItemListParams {
  /** Rows per page. */
  limit?: number;
  /** Rows to skip. */
  offset?: number;
  /** Column to sort by. */
  sort?: ItemSortKey;
  /** Sort direction. */
  order?: SortOrder;
  /** Name filter: the items whose name contains this text. */
  name?: string;
}

/** One page of the items list. */
export interface ItemListResult {
  /** Rows of the requested page. */
  items: Item[];
  /** Number of items that match the filter (ignoring limit and offset). */
  total: number;
}

/**
 * Read the total of the X-Total-Count header.
 * @param value - The header value
 * @returns The count, or null when the header is missing or is not a count
 */
function totalFromHeader(value: unknown): number | null {
  const text = typeof value === 'string' || typeof value === 'number' ? String(value) : '';
  return /^\d+$/.test(text) && Number.isSafeInteger(Number(text)) ? Number(text) : null;
}

export const itemService = {
  /**
   * List items, one page at a time, with an optional sort and name filter.
   * @param params - Paging, sort and filter; every field is optional
   * @returns The rows of the page and the total of the filtered list
   * @see GET /api/v1/items
   */
  list: async (params: ItemListParams = {}): Promise<ItemListResult> => {
    try {
      const query = new URLSearchParams();
      if (params.limit !== undefined) query.set('limit', String(params.limit));
      if (params.offset !== undefined) query.set('offset', String(params.offset));
      if (params.sort !== undefined) query.set('sort', params.sort);
      if (params.order !== undefined) query.set('order', params.order);
      if (params.name) query.set('name', params.name);
      const suffix = query.toString() ? `?${query.toString()}` : '';
      const response = await api.get<Item[]>(`/api/v1/items${suffix}`);
      const items = response.data;
      // The total is in a header so that the body stays a plain array.
      const total = totalFromHeader(AxiosHeaders.from(response.headers as AxiosHeaders).get('X-Total-Count')) ?? items.length;
      return { items, total };
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

/**
 * Public password reset (only with PASSWORD_RESET_ENABLED on the backend; the
 * pages exist only then). No call of this object carries a token or a cookie.
 */
export const passwordResetService = {
  /**
   * Ask for a reset mail. The server answers 202 for every well-formed username,
   * whether or not the account exists, so the answer never tells. The caller
   * shows the same confirmation for every result except a 400.
   * @param username - Login name (sent once, never logged)
   * @returns Nothing
   * @throws The request error (400 for an empty or over-long username, 429, 413, network)
   * @see POST /api/v1/auth/password-reset
   */
  requestPasswordReset: async (username: string): Promise<void> => {
    try {
      await unauthenticatedApi.post('/api/v1/auth/password-reset', { username });
    } catch (error) {
      console.error('Password reset request failed:', summarizeRequestError(error));
      throw error;
    }
  },

  /**
   * Set a new password with the token of the mail link. The token and the
   * password travel in the JSON body only. The server ends every session of the
   * user on success.
   * @param token - The token read from the link fragment
   * @param newPassword - The new password (sent once, never logged)
   * @returns Nothing (204)
   * @throws The request error: 400 (invalid or expired token, or a password that breaks the rules), 429, 413, 500 (the password stands but the sessions could not be ended)
   * @see POST /api/v1/auth/password-reset/confirm
   */
  confirmPasswordReset: async (token: string, newPassword: string): Promise<void> => {
    try {
      await unauthenticatedApi.post('/api/v1/auth/password-reset/confirm', { token, new_password: newPassword });
    } catch (error) {
      console.error('Password reset confirmation failed:', summarizeRequestError(error));
      throw error;
    }
  },
};

/**
 * An API key of the signed-in user, without its secret (GET /api/v1/api-keys).
 * The API has no update: a key is created or deleted.
 */
export interface ApiKey {
  id: string;
  user_id: string;
  /** The name that the user gave the key. */
  name: string;
  /** The first characters of the key, to tell keys apart. */
  prefix: string;
  created_at: string;
  /** Null (or missing) when the key has never been used. */
  last_used_at?: string | null;
  /** Null (or missing) when the key does not expire. The API requires an expiry on create. */
  expires_at?: string | null;
}

/** One page of the signed-in user's API keys, newest first (GET /api/v1/api-keys). `page` starts at 1. */
export interface ApiKeyListResponse {
  data: ApiKey[];
  total: number;
  page: number;
  pageSize: number;
}

/** Body of POST /api/v1/api-keys. The server takes the expiry in days (at most its maximum, 365 by default). */
export interface CreateApiKeyRequest {
  /** Letters, digits, spaces and . _ - : @ / ( ) +, at most 100 characters. */
  name: string;
  /** Days from now until the key expires. */
  expires_in_days: number;
}

/**
 * Answer of POST /api/v1/api-keys. `raw_key` is the secret: the server returns it
 * once and cannot show it again. The caller keeps it in component state only.
 */
export interface CreatedApiKey {
  id: string;
  name: string;
  prefix: string;
  /** The secret key (sk_ and 64 hex characters). Never log it, store it or put it in a URL. */
  raw_key: string;
  created_at: string;
  expires_at?: string | null;
}

/**
 * The signed-in user's API keys (a login session only: the API refuses a key
 * with 403). The request errors are logged as a summary; the create answer is
 * never logged, because it holds the raw key.
 */
export const apiKeyService = {
  /**
   * List one page of the signed-in user's keys, newest first.
   * @param page - Page number, from 1
   * @param pageSize - Keys per page (the server default is 25, the maximum 100)
   * @returns The page and the total number of keys
   * @see GET /api/v1/api-keys
   */
  list: async (page: number, pageSize: number): Promise<ApiKeyListResponse> => {
    try {
      const query = new URLSearchParams({ page: String(page), pageSize: String(pageSize) });
      const response = await api.get<ApiKeyListResponse>(`/api/v1/api-keys?${query.toString()}`);
      return response.data;
    } catch (error) {
      console.error('Failed to list API keys:', summarizeRequestError(error));
      throw error;
    }
  },

  /**
   * Create a key for the signed-in user. The raw key is in the answer once.
   * @param request - The name and the lifetime in days
   * @returns The new key, with its raw secret (the caller shows it once)
   * @throws The request error: 400 (name or expiry rule), 409 (25 active keys), 429, 413, network
   * @see POST /api/v1/api-keys
   */
  create: async (request: CreateApiKeyRequest): Promise<CreatedApiKey> => {
    try {
      const response = await api.post<CreatedApiKey>('/api/v1/api-keys', request);
      return response.data;
    } catch (error) {
      console.error('Failed to create an API key:', summarizeRequestError(error));
      throw error;
    }
  },

  /**
   * Revoke a key of the signed-in user. The key stops working at once.
   * @param id - Key ID
   * @returns Nothing
   * @throws The request error: 404 when the key does not exist (or is already revoked)
   * @see DELETE /api/v1/api-keys/:id
   */
  delete: async (id: string): Promise<void> => {
    try {
      await api.delete(`/api/v1/api-keys/${encodeURIComponent(id)}`);
    } catch (error) {
      console.error('Failed to revoke an API key:', summarizeRequestError(error));
      throw error;
    }
  },
};

/**
 * One audit entry (GET /api/v1/audit-logs). The server writes `details` as JSON
 * text (method, route, status, client IP, request ID and small values). The UI
 * shows it as text only, never as HTML.
 */
export interface AuditLog {
  id: string;
  /** RFC3339 time of the change. */
  timestamp: string;
  user_id: string;
  username: string;
  /** Lower case word, for example `create` or `change_role`. */
  action: string;
  /** Lower case word, for example `item`, `user` or `api_key`. */
  entity_type: string;
  entity_id: string;
  /** JSON text of the entry, or the raw text when it is not JSON. */
  details: string;
}

/** Query of the audit log list. Every field is optional; an unset field is not sent. */
export interface AuditLogListParams {
  /** Rows per page (the server default is 25, the maximum 100). */
  limit?: number;
  /**
   * Rows to skip. The server clamps an offset above 100000 to 100000 and answers
   * that clamped offset (with the rows of that page), not an error.
   */
  offset?: number;
  /** Only entries with this action. */
  action?: string;
  /** Only entries of this entity type. */
  entity_type?: string;
  /** Only entries of this user ID (the server has no username filter). */
  user_id?: string;
  /** Start of the range, RFC3339 with a zone, inclusive. */
  start_date?: string;
  /** End of the range, RFC3339 with a zone, inclusive. */
  end_date?: string;
}

/** One page of the audit log, newest first, with the exact total. */
export interface AuditLogListResult {
  data: AuditLog[];
  /** Number of entries that match the filters (ignoring limit and offset). */
  total: number;
  limit: number;
  offset: number;
}

/** The query keys of the audit log list, in the order the request sends them. */
const AUDIT_LOG_QUERY_KEYS: readonly (keyof AuditLogListParams)[] = [
  'limit',
  'offset',
  'action',
  'entity_type',
  'user_id',
  'start_date',
  'end_date',
];

/**
 * The audit log (an admin with a login session only: the API refuses everything
 * else with 403). A request error is logged as a summary.
 */
export const auditLogService = {
  /**
   * List one page of audit entries, newest first, with optional filters.
   * @param params - Paging and filters; a missing or empty field is not sent
   * @returns The entries of the page, the total and the paging of the answer. The `offset` of the answer is the one the server read: a larger requested offset comes back clamped to 100000
   * @throws The request error: 400 (a filter or the range is invalid), 403, 401, network
   * @see GET /api/v1/audit-logs
   */
  list: async (params: AuditLogListParams = {}): Promise<AuditLogListResult> => {
    try {
      const query = new URLSearchParams();
      for (const key of AUDIT_LOG_QUERY_KEYS) {
        const value = params[key];
        if (value !== undefined && value !== '') query.set(key, String(value));
      }
      const suffix = query.toString() ? `?${query.toString()}` : '';
      const response = await api.get<AuditLogListResult & { next_cursor?: string }>(`/api/v1/audit-logs${suffix}`);
      const { data, total, limit, offset } = response.data;
      return { data, total, limit, offset };
    } catch (error) {
      console.error('Failed to list audit logs:', summarizeRequestError(error));
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
        password_reset_enabled: data.password_reset_enabled === true,
      };
    } catch (error) {
      console.error('Failed to load the UI config:', summarizeRequestError(error));
      throw error;
    }
  },
};

export default api;

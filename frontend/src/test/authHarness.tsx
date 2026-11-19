import type { ReactNode } from 'react';
import type { InternalAxiosRequestConfig } from 'axios';
import api from '../api/client';
import { authApi } from '../api/refresh';
import { installMockBackend, pathOf } from './mockHttp';
import type { MockReply } from './mockHttp';
import { UIConfigProvider } from '../context/UIConfigContext';
import { AuthProvider } from '../context/AuthContext';
import type { UIConfig } from '../context/UIConfigContext';

/** Answer of GET /api/v1/ui-config for the given auth settings. */
export function uiConfigOf(overrides: Partial<UIConfig> = {}): UIConfig {
  return {
    title: 'Test App',
    auth_enabled: false,
    oidc_enabled: false,
    local_auth_enabled: false,
    password_reset_enabled: false,
    ...overrides,
  };
}

export interface FakeBackendRoutes {
  uiConfig: UIConfig | MockReply;
  refresh?: MockReply | (() => MockReply | Promise<MockReply>);
  login?: MockReply | ((config: InternalAxiosRequestConfig) => MockReply | Promise<MockReply>);
  logout?: MockReply;
  /** Every other request (pages). Default: 200 []. */
  other?: (config: InternalAxiosRequestConfig) => MockReply | Promise<MockReply>;
}

/** Fake the whole backend for the API client and the auth client. */
export function fakeBackend(routes: FakeBackendRoutes) {
  return installMockBackend([api, authApi], (config) => {
    const path = pathOf(config);
    if (path === '/api/v1/ui-config') {
      return 'status' in routes.uiConfig || 'networkError' in routes.uiConfig
        ? (routes.uiConfig as MockReply)
        : { status: 200, data: routes.uiConfig };
    }
    if (path === '/api/v1/auth/refresh') {
      const r = routes.refresh ?? { status: 401, data: { error: 'Invalid session' } };
      return typeof r === 'function' ? r() : r;
    }
    if (path === '/api/v1/auth/login') {
      const r = routes.login ?? { status: 401, data: { error: 'Invalid username or password' } };
      return typeof r === 'function' ? r(config) : r;
    }
    if (path === '/api/v1/auth/logout') {
      return routes.logout ?? { status: 200, data: { message: 'Logged out' } };
    }
    return routes.other ? routes.other(config) : { status: 200, data: [] };
  });
}

/** The real UI config and auth providers. */
export function AuthProviders({ children }: { children: ReactNode }) {
  return (
    <UIConfigProvider>
      <AuthProvider>{children}</AuthProvider>
    </UIConfigProvider>
  );
}

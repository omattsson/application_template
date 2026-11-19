import { createContext, useContext, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { uiConfigService } from '../api/client';

/** Answer of GET /api/v1/ui-config (backend handlers.UIConfigResponse). */
export interface UIConfig {
  title: string;
  /** AUTH_ENABLED: show the login flow and send the access token. */
  auth_enabled: boolean;
  /** Offer "Sign in with SSO" (AUTH_ENABLED and OIDC_ENABLED). */
  oidc_enabled: boolean;
  /** Show the username and password form. */
  local_auth_enabled: boolean;
  /** PASSWORD_RESET_ENABLED with the password form shown: offer "Forgot password?" and the reset pages. */
  password_reset_enabled: boolean;
}

/** Used when the UI config cannot be read: the app as without auth. */
export const FALLBACK_UI_CONFIG: UIConfig = {
  title: 'Application Template',
  auth_enabled: false,
  oidc_enabled: false,
  local_auth_enabled: false,
  password_reset_enabled: false,
};

interface UIConfigContextValue {
  config: UIConfig;
  /** True until the first answer (or failure). */
  loading: boolean;
}

const UIConfigContext = createContext<UIConfigContextValue | null>(null);

/** Reads GET /api/v1/ui-config once and provides it; falls back to {@link FALLBACK_UI_CONFIG} on failure. */
export function UIConfigProvider({ children }: { children: ReactNode }) {
  const [config, setConfig] = useState<UIConfig>(FALLBACK_UI_CONFIG);
  const [loading, setLoading] = useState(true);
  const started = useRef(false);

  useEffect(() => {
    // Once per provider, also under StrictMode's double effects.
    if (started.current) return;
    started.current = true;
    uiConfigService
      .get()
      .then(setConfig, () => setConfig(FALLBACK_UI_CONFIG))
      .finally(() => setLoading(false));
  }, []);

  const value = useMemo(() => ({ config, loading }), [config, loading]);
  return <UIConfigContext.Provider value={value}>{children}</UIConfigContext.Provider>;
}

/**
 * The UI config.
 * @returns The config and whether it is still loading
 */
export function useUIConfig(): UIConfigContextValue {
  const ctx = useContext(UIConfigContext);
  if (!ctx) throw new Error('useUIConfig must be used within <UIConfigProvider>');
  return ctx;
}

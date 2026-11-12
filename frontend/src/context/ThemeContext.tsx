import { createContext, useContext, useState, useMemo, useCallback, useEffect, type ReactNode } from 'react';
import { ThemeProvider, CssBaseline, useMediaQuery } from '@mui/material';
import { createAppTheme } from '../theme';

type ThemeMode = 'light' | 'dark';

interface ThemeModeContextValue {
  mode: ThemeMode;
  toggleMode: () => void;
}

const ThemeModeContext = createContext<ThemeModeContextValue | undefined>(undefined);

const STORAGE_KEY = 'theme-mode';

/**
 * Read the persisted mode. Storage can throw (private browsing, blocked
 * cookies, a sandboxed frame), so every access is guarded.
 * @returns The stored mode, or null when nothing valid is stored or storage is unavailable
 */
function readStoredMode(): ThemeMode | null {
  try {
    const stored = window.localStorage.getItem(STORAGE_KEY);
    return stored === 'light' || stored === 'dark' ? stored : null;
  } catch {
    return null;
  }
}

/**
 * Persist the mode. A failed write is ignored: the choice then lasts for the session only.
 * @param mode - The mode to store
 */
function writeStoredMode(mode: ThemeMode): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, mode);
  } catch {
    // Storage is unavailable or full. The in-memory choice still applies.
  }
}

function getInitialMode(prefersDark: boolean): ThemeMode {
  return readStoredMode() ?? (prefersDark ? 'dark' : 'light');
}

export function ThemeModeProvider({ children }: { children: ReactNode }) {
  const prefersDark = useMediaQuery('(prefers-color-scheme: dark)', { noSsr: true });
  const [mode, setMode] = useState<ThemeMode>(() => getInitialMode(prefersDark));

  const toggleMode = useCallback(() => {
    setMode((prev) => (prev === 'light' ? 'dark' : 'light'));
  }, []);

  useEffect(() => {
    writeStoredMode(mode);
  }, [mode]);

  const theme = useMemo(() => createAppTheme(mode), [mode]);

  const contextValue = useMemo(() => ({ mode, toggleMode }), [mode, toggleMode]);

  return (
    <ThemeModeContext.Provider value={contextValue}>
      <ThemeProvider theme={theme}>
        <CssBaseline />
        {children}
      </ThemeProvider>
    </ThemeModeContext.Provider>
  );
}

export function useThemeMode(): ThemeModeContextValue {
  const context = useContext(ThemeModeContext);
  if (!context) {
    throw new Error('useThemeMode must be used within a ThemeModeProvider');
  }
  return context;
}

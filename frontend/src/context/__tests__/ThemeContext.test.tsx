import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { renderHook, act, render, screen } from '@testing-library/react';
import type { ReactNode } from 'react';
import { ThemeModeProvider, useThemeMode } from '../ThemeContext';

// Mock useMediaQuery to control prefers-color-scheme
let mockPrefersDark = false;
vi.mock('@mui/material', async () => {
  const actual = await vi.importActual('@mui/material');
  return {
    ...actual,
    useMediaQuery: () => mockPrefersDark,
  };
});

const wrapper = ({ children }: { children: ReactNode }) => (
  <ThemeModeProvider>{children}</ThemeModeProvider>
);

describe('ThemeContext', () => {
  beforeEach(() => {
    localStorage.clear();
    mockPrefersDark = false;
  });

  afterEach(() => {
    vi.clearAllMocks();
    vi.restoreAllMocks();
    localStorage.clear();
  });

  describe('useThemeMode outside provider', () => {
    it('throws when used outside ThemeModeProvider', () => {
      vi.spyOn(console, 'error').mockImplementation(() => {});
      expect(() => renderHook(() => useThemeMode())).toThrow(
        'useThemeMode must be used within a ThemeModeProvider'
      );
    });
  });

  describe('default mode', () => {
    it('defaults to light when system prefers light and no stored value', () => {
      mockPrefersDark = false;
      const { result } = renderHook(() => useThemeMode(), { wrapper });
      expect(result.current.mode).toBe('light');
    });

    it('defaults to dark when system prefers dark and no stored value', () => {
      mockPrefersDark = true;
      const { result } = renderHook(() => useThemeMode(), { wrapper });
      expect(result.current.mode).toBe('dark');
    });

    it('uses stored value over system preference', () => {
      mockPrefersDark = false;
      localStorage.setItem('theme-mode', 'dark');
      const { result } = renderHook(() => useThemeMode(), { wrapper });
      expect(result.current.mode).toBe('dark');
    });

    it('uses stored light value over dark system preference', () => {
      mockPrefersDark = true;
      localStorage.setItem('theme-mode', 'light');
      const { result } = renderHook(() => useThemeMode(), { wrapper });
      expect(result.current.mode).toBe('light');
    });

    it('ignores an invalid stored value and follows the system preference', () => {
      mockPrefersDark = true;
      localStorage.setItem('theme-mode', 'sepia');
      const { result } = renderHook(() => useThemeMode(), { wrapper });
      expect(result.current.mode).toBe('dark');
    });
  });

  describe('toggleMode', () => {
    it('toggles from light to dark', () => {
      mockPrefersDark = false;
      const { result } = renderHook(() => useThemeMode(), { wrapper });
      expect(result.current.mode).toBe('light');

      act(() => {
        result.current.toggleMode();
      });

      expect(result.current.mode).toBe('dark');
    });

    it('toggles from dark to light', () => {
      localStorage.setItem('theme-mode', 'dark');
      const { result } = renderHook(() => useThemeMode(), { wrapper });
      expect(result.current.mode).toBe('dark');

      act(() => {
        result.current.toggleMode();
      });

      expect(result.current.mode).toBe('light');
    });

    it('toggles back and forth', () => {
      mockPrefersDark = false;
      const { result } = renderHook(() => useThemeMode(), { wrapper });

      act(() => result.current.toggleMode());
      expect(result.current.mode).toBe('dark');

      act(() => result.current.toggleMode());
      expect(result.current.mode).toBe('light');
    });
  });

  describe('persistence to localStorage', () => {
    it('saves mode to localStorage on toggle', () => {
      mockPrefersDark = false;
      const { result } = renderHook(() => useThemeMode(), { wrapper });

      act(() => {
        result.current.toggleMode();
      });

      expect(localStorage.getItem('theme-mode')).toBe('dark');
    });

    it('saves initial mode to localStorage', () => {
      mockPrefersDark = false;
      renderHook(() => useThemeMode(), { wrapper });

      expect(localStorage.getItem('theme-mode')).toBe('light');
    });

    it('restores the persisted choice in a new provider instance', () => {
      const first = renderHook(() => useThemeMode(), { wrapper });
      act(() => first.result.current.toggleMode());
      first.unmount();

      const second = renderHook(() => useThemeMode(), { wrapper });
      expect(second.result.current.mode).toBe('dark');
    });
  });

  describe('a throwing localStorage', () => {
    it('falls back to the system preference when reading throws', () => {
      mockPrefersDark = true;
      vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
        throw new DOMException('denied', 'SecurityError');
      });
      const { result } = renderHook(() => useThemeMode(), { wrapper });
      expect(result.current.mode).toBe('dark');
    });

    it('falls back to light when reading throws and the system prefers light', () => {
      mockPrefersDark = false;
      vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
        throw new DOMException('denied', 'SecurityError');
      });
      const { result } = renderHook(() => useThemeMode(), { wrapper });
      expect(result.current.mode).toBe('light');
    });

    it('still toggles when writing throws', () => {
      vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
        throw new DOMException('storage is full', 'InvalidStateError');
      });
      const { result } = renderHook(() => useThemeMode(), { wrapper });

      act(() => result.current.toggleMode());
      expect(result.current.mode).toBe('dark');
    });

    it('still renders its children when the storage getter itself throws', () => {
      vi.spyOn(window, 'localStorage', 'get').mockImplementation(() => {
        throw new DOMException('denied', 'SecurityError');
      });
      render(
        <ThemeModeProvider>
          <div>Still here</div>
        </ThemeModeProvider>
      );
      expect(screen.getByText('Still here')).toBeInTheDocument();
    });
  });

  describe('MUI ThemeProvider integration', () => {
    it('provides a MUI theme to children', () => {
      const ThemeConsumer = () => {
        const { mode } = useThemeMode();
        return <div data-testid="mode">{mode}</div>;
      };

      render(
        <ThemeModeProvider>
          <ThemeConsumer />
        </ThemeModeProvider>
      );

      expect(screen.getByTestId('mode')).toHaveTextContent('light');
    });

    it('renders CssBaseline via the provider (no crash)', () => {
      render(
        <ThemeModeProvider>
          <div data-testid="child">Hello</div>
        </ThemeModeProvider>
      );

      expect(screen.getByTestId('child')).toBeInTheDocument();
    });
  });
});

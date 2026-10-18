import { createContext, useContext, useMemo, useState, type ReactNode } from 'react';
import { useColorScheme } from 'react-native';
import { themes, type ThemeMode, type ThemeTokens } from './tokens';

interface ThemeValue {
  tokens: ThemeTokens;
  isDark: boolean;
  /** 用户选的那一档（system / light / dark）；默认跟随系统 */
  mode: ThemeMode;
  setMode: (mode: ThemeMode) => void;
}

const ThemeCtx = createContext<ThemeValue | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  const system = useColorScheme();
  const [mode, setMode] = useState<ThemeMode>('system');
  const isDark = mode === 'system' ? system !== 'light' : mode === 'dark';
  const value = useMemo<ThemeValue>(
    () => ({ tokens: themes[isDark ? 'dark' : 'light'], isDark, mode, setMode }),
    [isDark, mode],
  );
  return <ThemeCtx.Provider value={value}>{children}</ThemeCtx.Provider>;
}

export function useTheme(): ThemeValue {
  const v = useContext(ThemeCtx);
  if (!v) throw new Error('useTheme 只能用在 ThemeProvider 里');
  return v;
}

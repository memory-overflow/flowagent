import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react';

// ============================================================
// 主题：light / dark / system
//
// 实现方式：把解析后的主题写到 <html data-theme>，CSS 侧用
// [data-theme="light"] 覆盖 :root 里那套（原本只有深色）的变量。
// 之所以不动 :root 的默认值：深色是这套配色的事实基线，把它整体搬走会
// 波及 3400 行 CSS，风险与收益不成比例。
//
// 首屏闪烁由 index.html 里的内联脚本兜底 —— 它在 CSS 生效前就把
// data-theme 打上，React 挂载时只是确认一遍。
// ============================================================

export type ThemeMode = 'light' | 'dark' | 'system';
export type ResolvedTheme = 'light' | 'dark';

const STORAGE_KEY = 'flowagent.theme';
const DARK_QUERY = '(prefers-color-scheme: dark)';

function readStoredMode(): ThemeMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (v === 'light' || v === 'dark' || v === 'system') return v;
  } catch {
    /* localStorage 不可用就跟随系统 */
  }
  return 'system';
}

function systemTheme(): ResolvedTheme {
  if (typeof window === 'undefined' || !window.matchMedia) return 'dark';
  return window.matchMedia(DARK_QUERY).matches ? 'dark' : 'light';
}

interface ThemeValue {
  mode: ThemeMode;
  setMode: (mode: ThemeMode) => void;
  /** system 解析后的实际主题，供需要按主题分支渲染的组件使用 */
  resolved: ResolvedTheme;
}

const ThemeContext = createContext<ThemeValue>({
  mode: 'system',
  setMode: () => {},
  resolved: 'dark',
});

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<ThemeMode>(readStoredMode);
  // 跟随系统时，系统偏好本身也是一个状态源，要在 state 里留一份，
  // 否则 matchMedia 变化时组件不会重渲染。
  const [sysTheme, setSysTheme] = useState<ResolvedTheme>(systemTheme);

  useEffect(() => {
    if (!window.matchMedia) return;
    const mq = window.matchMedia(DARK_QUERY);
    const onChange = (e: MediaQueryListEvent) => setSysTheme(e.matches ? 'dark' : 'light');
    // Safari < 14 只有 addListener
    if (mq.addEventListener) {
      mq.addEventListener('change', onChange);
      return () => mq.removeEventListener('change', onChange);
    }
    mq.addListener(onChange);
    return () => mq.removeListener(onChange);
  }, []);

  const resolved: ResolvedTheme = mode === 'system' ? sysTheme : mode;

  useEffect(() => {
    const root = document.documentElement;
    root.setAttribute('data-theme', resolved);
    // 让浏览器原生控件（滚动条、表单、拼写检查弹层）也跟着走
    root.style.colorScheme = resolved;
  }, [resolved]);

  const setMode = useCallback((next: ThemeMode) => {
    setModeState(next);
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch {
      /* 存不下就只在本次会话生效 */
    }
  }, []);

  const value = useMemo<ThemeValue>(() => ({ mode, setMode, resolved }), [mode, setMode, resolved]);

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeValue {
  return useContext(ThemeContext);
}

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react';
import { zh, en } from './locales';

// ============================================================
// 极简 i18n
//
// 为什么自己写而不引 i18next：这里的诉求只有「两种语言 + 点号 key + 少量
// 插值」，引一整套生态（插件/后端加载/复数规则）带来的体积与心智负担不划算。
//
// 两个关键行为：
//   1. 英文缺失自动回落中文 —— 翻译可以分批推进，任何时刻界面都不会出现
//      裸 key 或空白。
//   2. 语言跟随 React state —— 切换后所有组件重渲染，无需刷新页面。
// ============================================================

export type Lang = 'zh' | 'en';

/** 词典节点：任意深度嵌套，叶子是字符串 */
type Dict = { [key: string]: any };

const DICTS: Record<Lang, Dict> = { zh, en };

const STORAGE_KEY = 'flowagent.lang';

function readStoredLang(): Lang {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (v === 'zh' || v === 'en') return v;
  } catch {
    /* localStorage 不可用（隐私模式等）就用默认语言 */
  }
  return 'zh';
}

/** 按点号路径取词，路径不通或叶子不是字符串时返回 undefined */
function lookup(dict: Dict, path: string): string | undefined {
  let cur: any = dict;
  for (const seg of path.split('.')) {
    if (cur == null || typeof cur !== 'object') return undefined;
    cur = cur[seg];
  }
  return typeof cur === 'string' ? cur : undefined;
}

/** 把 {name} 占位符替换成实际值；没有对应值就原样保留，方便发现问题 */
function interpolate(s: string, vars?: Record<string, string | number>): string {
  if (!vars) return s;
  return s.replace(/\{(\w+)\}/g, (m, k) => (k in vars ? String(vars[k]) : m));
}

export type Translate = (key: string, vars?: Record<string, string | number>) => string;

interface I18nValue {
  lang: Lang;
  setLang: (lang: Lang) => void;
  t: Translate;
}

const I18nContext = createContext<I18nValue>({
  lang: 'zh',
  setLang: () => {},
  t: (key) => key,
});

export function I18nProvider({ children }: { children: ReactNode }) {
  const [lang, setLangState] = useState<Lang>(readStoredLang);

  useEffect(() => {
    // 同步 <html lang>：影响浏览器翻译提示、字体回退与无障碍朗读
    document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  }, [lang]);

  const setLang = useCallback((next: Lang) => {
    setLangState(next);
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch {
      /* 存不下就只在本次会话生效 */
    }
  }, []);

  const t = useCallback<Translate>(
    (key, vars) => {
      // 当前语言 → 中文兜底 → 裸 key。裸 key 只在「中文词典也漏了」时出现，
      // 保留原文比显示空白更容易定位问题。
      const raw = lookup(DICTS[lang], key) ?? lookup(zh, key) ?? key;
      return interpolate(raw, vars);
    },
    [lang],
  );

  const value = useMemo<I18nValue>(() => ({ lang, setLang, t }), [lang, setLang, t]);

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n(): I18nValue {
  return useContext(I18nContext);
}

/** 只要 t 的场景（多数组件）用它，省一层解构 */
export function useT(): Translate {
  return useI18n().t;
}

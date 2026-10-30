import { useEffect, useRef, useState } from 'react';
import { useI18n } from '../i18n';
import { useTheme, type ThemeMode } from '../theme';

// ============================================================
// 右上角全局偏好控件：语言 + 亮暗主题
//
// 从设置页独立出来，全局固定右上角，无需进 /settings 即可切换。
// 两项都是纯前端本机偏好（localStorage），切换立即生效、无需刷新。
// ============================================================

const THEME_ICON: Record<ThemeMode, string> = {
  light: '☀️',
  dark: '🌙',
  system: '🖥️',
};

export default function PreferenceControls() {
  const { lang, setLang, t } = useI18n();
  const { mode, setMode } = useTheme();
  const [open, setOpen] = useState<'lang' | 'theme' | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);

  // 点击控件外部时收起下拉
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(null);
      }
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);

  return (
    <div className="pref-controls" ref={rootRef}>
      {/* 语言 */}
      <div className="pref-dropdown">
        <button
          type="button"
          className="pref-trigger"
          onClick={() => setOpen(open === 'lang' ? null : 'lang')}
          title={t('preferences.language')}
          aria-label={t('preferences.language')}
        >
          <span className="pref-emoji">🌐</span>
          <span className="pref-label">{lang === 'zh' ? '简' : 'EN'}</span>
          <span className="pref-caret">▾</span>
        </button>
        {open === 'lang' && (
          <div className="pref-menu">
            <button
              type="button"
              className={`pref-item ${lang === 'zh' ? 'active' : ''}`}
              onClick={() => { setLang('zh'); setOpen(null); }}
            >
              {t('preferences.langZh')}
            </button>
            <button
              type="button"
              className={`pref-item ${lang === 'en' ? 'active' : ''}`}
              onClick={() => { setLang('en'); setOpen(null); }}
            >
              {t('preferences.langEn')}
            </button>
          </div>
        )}
      </div>

      {/* 主题 */}
      <div className="pref-dropdown">
        <button
          type="button"
          className="pref-trigger"
          onClick={() => setOpen(open === 'theme' ? null : 'theme')}
          title={t('preferences.theme')}
          aria-label={t('preferences.theme')}
        >
          <span className="pref-emoji">{THEME_ICON[mode]}</span>
          <span className="pref-caret">▾</span>
        </button>
        {open === 'theme' && (
          <div className="pref-menu">
            <button
              type="button"
              className={`pref-item ${mode === 'light' ? 'active' : ''}`}
              onClick={() => { setMode('light'); setOpen(null); }}
            >
              {t('preferences.themeLight')}
            </button>
            <button
              type="button"
              className={`pref-item ${mode === 'dark' ? 'active' : ''}`}
              onClick={() => { setMode('dark'); setOpen(null); }}
            >
              {t('preferences.themeDark')}
            </button>
            <button
              type="button"
              className={`pref-item ${mode === 'system' ? 'active' : ''}`}
              onClick={() => { setMode('system'); setOpen(null); }}
            >
              {t('preferences.themeSystem')}
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

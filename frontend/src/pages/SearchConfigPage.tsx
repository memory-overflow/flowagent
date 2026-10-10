import { useState, useEffect, useCallback } from 'react';
import {
  getSearchConfig,
  updateSearchConfig,
  testSearch,
} from '../services/api';
import type { SearchConfig, SearchEngine } from '../types/api';
import { SearchEngineKeyURL } from '../types/api';
import { useT } from '../i18n';

// 引擎下拉 / 标签的展示名按枚举映射（types/api.ts 里的 SearchEngineLabel 是中文
// 常量，不进词典），改这里即可同步中英。
const ENGINE_LABEL_KEY: Record<SearchEngine, string> = {
  baidu: 'searchConfig.engineBaidu',
  ddg: 'searchConfig.engineDdg',
  bocha: 'searchConfig.engineBocha',
  tavily: 'searchConfig.engineTavily',
  serper: 'searchConfig.engineSerper',
  tencent_wsa: 'searchConfig.engineTencentWsa',
  baidu_qianfan: 'searchConfig.engineBaiduQianfan',
};

// 已下线引擎（与后端 model.SearchEngine.Hidden 保持一致）：DuckDuckGo 在中国大陆
// 出口 IP 上稳定返回 202，探活与检索都必败，暂不提供选择。历史配置若仍是 ddg，
// 加载时回落到 baidu，避免下拉出现无对应 option 的空选中态。
const HIDDEN_ENGINES: SearchEngine[] = ['ddg'];
const ENGINES: SearchEngine[] = ['baidu', 'bocha', 'tavily', 'serper', 'tencent_wsa', 'baidu_qianfan'];
const KEY_ENGINES: SearchEngine[] = ['bocha', 'tavily', 'serper', 'tencent_wsa', 'baidu_qianfan'];

function needsKey(e: SearchEngine): boolean {
  return KEY_ENGINES.includes(e);
}

export default function SearchConfigPage() {
  const t = useT();
  const [loading, setLoading] = useState(true);
  const [engine, setEngine] = useState<SearchEngine>('baidu');
  const [bochaKey, setBochaKey] = useState('');
  const [tavilyKey, setTavilyKey] = useState('');
  const [serperKey, setSerperKey] = useState('');
  const [tencentWSAKey, setTencentWSAKey] = useState('');
  const [baiduQianfanKey, setBaiduQianfanKey] = useState('');
  const [saving, setSaving] = useState(false);

  const [query, setQuery] = useState('');
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<{ success: boolean; msg: string } | null>(null);

  const fetch = useCallback(async () => {
    try {
      const res = await getSearchConfig();
      const cfg = res.data.data;
      if (cfg) {
        const raw = (cfg.engine || 'baidu') as SearchEngine;
        setEngine(HIDDEN_ENGINES.includes(raw) ? 'baidu' : raw);
        setBochaKey(cfg.bocha_key || '');
        setTavilyKey(cfg.tavily_key || '');
        setSerperKey(cfg.serper_key || '');
        setTencentWSAKey(cfg.tencent_wsa_key || '');
        setBaiduQianfanKey(cfg.baidu_qianfan_key || '');
      }
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetch(); }, [fetch]);

  const keyValue = (e: SearchEngine): string => {
    if (e === 'bocha') return bochaKey;
    if (e === 'tavily') return tavilyKey;
    if (e === 'serper') return serperKey;
    if (e === 'tencent_wsa') return tencentWSAKey;
    return baiduQianfanKey;
  };
  const setKeyValue = (e: SearchEngine, v: string) => {
    if (e === 'bocha') setBochaKey(v);
    else if (e === 'tavily') setTavilyKey(v);
    else if (e === 'serper') setSerperKey(v);
    else if (e === 'tencent_wsa') setTencentWSAKey(v);
    else setBaiduQianfanKey(v);
  };

  const handleSave = async () => {
    // 前端先做一次与后端一致的预校验，给用户即时反馈；后端仍有硬校验兜底。
    if (needsKey(engine) && !keyValue(engine).trim()) {
      alert(t('searchConfig.keyRequired', {
        engine: t(ENGINE_LABEL_KEY[engine]),
        url: SearchEngineKeyURL[engine],
      }));
      return;
    }
    setSaving(true);
    try {
      await updateSearchConfig({
        engine,
        bocha_key: bochaKey.trim(),
        tavily_key: tavilyKey.trim(),
        serper_key: serperKey.trim(),
        tencent_wsa_key: tencentWSAKey.trim(),
        baidu_qianfan_key: baiduQianfanKey.trim(),
      });
      alert(t('searchConfig.saved'));
      await fetch();
    } catch (e: any) {
      alert(t('searchConfig.saveFailed', { msg: e.message }));
    } finally {
      setSaving(false);
    }
  };

  const handleTest = async () => {
    if (!query.trim()) return alert(t('searchConfig.errQueryRequired'));
    setTesting(true);
    setTestResult(null);
    try {
      const res = await testSearch({
        engine,
        query: query.trim(),
        bocha_key: bochaKey.trim(),
        tavily_key: tavilyKey.trim(),
        serper_key: serperKey.trim(),
        tencent_wsa_key: tencentWSAKey.trim(),
        baidu_qianfan_key: baiduQianfanKey.trim(),
      });
      const d = res.data.data;
      if (d.success) {
        setTestResult({ success: true, msg: d.results || t('searchConfig.noResultText') });
      } else {
        setTestResult({ success: false, msg: d.error_msg || t('common.unknown') });
      }
    } catch (e: any) {
      setTestResult({ success: false, msg: t('searchConfig.testFailed', { msg: e.message }) });
    } finally {
      setTesting(false);
    }
  };

  if (loading) {
    return (
      <div className="page-container">
        <div style={{ padding: 20, textAlign: 'center', color: 'var(--text-muted)' }}>{t('common.loading')}</div>
      </div>
    );
  }

  return (
    <div className="page-container">
      <div className="page-header">
        <h2>{t('searchConfig.title')}</h2>
      </div>

      <p style={{ marginTop: 0, fontSize: 13, color: 'var(--text-muted)' }}>
        {t('searchConfig.intro')}
      </p>

      <div className="form-group">
        <label>{t('searchConfig.currentEngine')}</label>
        <select className="form-select" value={engine} onChange={(e) => setEngine(e.target.value as SearchEngine)}>
          {ENGINES.map((e) => (
            <option key={e} value={e}>{t(ENGINE_LABEL_KEY[e])}</option>
          ))}
        </select>
      </div>

      <div className="form-group" style={{ marginTop: 16 }}>
        <label>{t('searchConfig.apiKeyGroup')}</label>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          {KEY_ENGINES.map((e) => {
            const url = SearchEngineKeyURL[e];
            const isCurrent = engine === e;
            return (
              <div key={e} style={{ border: isCurrent ? '1px solid var(--accent)' : '1px solid var(--border)', borderRadius: 6, padding: 12 }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
                  <span style={{ fontWeight: 600, fontSize: 14 }}>{t(ENGINE_LABEL_KEY[e])}</span>
                  {isCurrent && <span className="badge badge-info">{t('searchConfig.currentBadge')}</span>}
                  {url && (
                    <a href={url} target="_blank" rel="noreferrer" style={{ fontSize: 12 }}>
                      {t('searchConfig.applyKey')}
                    </a>
                  )}
                </div>
                <input
                  className="form-input"
                  type="password"
                  value={keyValue(e)}
                  onChange={(ev) => setKeyValue(e, ev.target.value)}
                  placeholder={t('searchConfig.apiKeyPlaceholder', { engine: t(ENGINE_LABEL_KEY[e]) })}
                  autoComplete="off"
                />
              </div>
            );
          })}
        </div>
      </div>

      <div className="form-group" style={{ marginTop: 16 }}>
        <label>{t('searchConfig.connectivityTest')}</label>
        <div style={{ display: 'flex', gap: 8 }}>
          <input
            className="form-input"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={t('searchConfig.queryPlaceholder')}
          />
          <button className="btn btn-ghost" onClick={handleTest} disabled={testing}>
            {testing ? t('searchConfig.testing') : t('searchConfig.test')}
          </button>
        </div>
        {testResult && (
          <div style={{
            marginTop: 8,
            padding: 12,
            borderRadius: 4,
            background: testResult.success ? 'rgba(34,197,94,0.1)' : 'rgba(239,68,68,0.1)',
            fontSize: 13,
            color: 'var(--text-primary)',
          }}>
            <div style={{ fontWeight: 600, marginBottom: 6 }}>
              {testResult.success ? t('searchConfig.searchOk') : t('searchConfig.searchFail')}
            </div>
            <div style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{testResult.msg}</div>
          </div>
        )}
      </div>

      <div style={{ marginTop: 24, display: 'flex', gap: 8 }}>
        <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
          {saving ? t('common.saving') : t('common.save')}
        </button>
      </div>
    </div>
  );
}

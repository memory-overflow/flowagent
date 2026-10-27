import { useState, useEffect, useCallback } from 'react';
import {
  getDBConfig,
  updateDBConfig,
  testDBConfig,
  getAgentSettings,
  updateAgentSettings,
} from '../services/api';
import type { DBConfig, DBDriver, AgentSettings } from '../types/api';
import { useT } from '../i18n';

// 系统设置页：两件事 —— 外观与语言（本机偏好）、数据库连接（服务端配置）。
//
// 数据库连接部分的背景：云上 MySQL 每次查询都要跨公网往返，本地调试时慢到影响手感。
// 这里可以直接切到本机 MySQL 或 sqlite（纯本地文件，零网络往返）。
// 配置落在服务端的 config/db_config.json，不写 conf.yaml，密码也不进代码仓库。
//
// 生效时机：**保存即生效**（热切换）。server 保存后就地换库 + 建表；
// scheduler 靠设置文件 watcher 在 2s 内跟随。在途请求在旧库上收尾
// （旧连接池延迟 30s 关闭），新请求直接落新库。

export default function SettingsPage() {
  const t = useT();
  const [loading, setLoading] = useState(true);
  const [cfg, setCfg] = useState<DBConfig | null>(null);
  const [driver, setDriver] = useState<DBDriver>('mysql');
  const [host, setHost] = useState('127.0.0.1');
  const [port, setPort] = useState(3306);
  const [user, setUser] = useState('root');
  const [password, setPassword] = useState('');
  const [dbname, setDbname] = useState('workflow_agent_test');
  const [charset, setCharset] = useState('utf8mb4');
  const [path, setPath] = useState('./data/flowagent.db');

  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<{ success: boolean; msg: string } | null>(null);
  const [saveMsg, setSaveMsg] = useState<string | null>(null);
  const [agentCfg, setAgentCfg] = useState<AgentSettings | null>(null);
  const [agentSaving, setAgentSaving] = useState(false);
  const [agentMsg, setAgentMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const fetch = useCallback(async () => {
    try {
      const [res, agentRes] = await Promise.all([getDBConfig(), getAgentSettings()]);
      const d = res.data.data;
      setAgentCfg(agentRes.data.data);
      setCfg(d);
      if (d) {
        setDriver(d.driver === 'sqlite' ? 'sqlite' : 'mysql');
        setHost(d.host || '127.0.0.1');
        setPort(d.port || 3306);
        setUser(d.user || 'root');
        setPassword(d.password || '');
        setDbname(d.dbname || 'workflow_agent_test');
        setCharset(d.charset || 'utf8mb4');
        setPath(d.path || './data/flowagent.db');
      }
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetch(); }, [fetch]);

  const payload = () => ({
    driver,
    host,
    port: Number(port) || 3306,
    user,
    password,
    dbname,
    charset,
    path,
  });

  const handleTest = async () => {
    setTesting(true);
    setTestResult(null);
    try {
      const res = await testDBConfig(payload());
      const d = res.data.data;
      setTestResult({
        success: d.success,
        // d.desc / d.error_msg 是后端给的诊断文案，原样透出，不进词典
        msg: d.success
          ? `${t('settings.connectOkLabel')}: ${d.desc || ''}`
          : d.error_msg || t('common.unknown'),
      });
    } catch (e: any) {
      setTestResult({ success: false, msg: t('settings.testFailed', { msg: e.message }) });
    } finally {
      setTesting(false);
    }
  };

  const handleSave = async () => {
    setSaving(true);
    setSaveMsg(null);
    try {
      const res = await updateDBConfig(payload());
      const d = res.data.data;
      // 后端保存前会先试连，连不上直接拒绝；保存成功即热切换完成
      setSaveMsg(d.msg || t('settings.saved'));
      await fetch();
    } catch (e: any) {
      alert(t('settings.saveFailed', { msg: e.message }));
    } finally {
      setSaving(false);
    }
  };

  const handleAgentSave = async () => {
    if (!agentCfg) return;
    const planner = agentCfg.planner_max_tool_call_rounds;
    const single = agentCfg.single_agent_max_tool_call_rounds;
    if (!Number.isInteger(planner) || planner < 1 || planner > 20 || !Number.isInteger(single) || single < 1 || single > 500) {
      setAgentMsg({ ok: false, text: t('settings.agentValidation') });
      return;
    }
    setAgentSaving(true);
    setAgentMsg(null);
    try {
      const res = await updateAgentSettings(agentCfg);
      setAgentCfg(res.data.data);
      setAgentMsg({ ok: true, text: t('settings.agentSaved') });
    } catch (e: any) {
      setAgentMsg({ ok: false, text: t('settings.agentSaveFailed', { msg: e?.response?.data?.msg || e.message }) });
    } finally {
      setAgentSaving(false);
    }
  };

  if (loading) {
    return (
      <div className="page-container">
        <div style={{ padding: 20, textAlign: 'center', color: 'var(--text-muted)' }}>
          {t('common.loading')}
        </div>
      </div>
    );
  }

  return (
    <div className="page-container">
      <div className="page-header">
        <h2>{t('settings.title')}</h2>
      </div>

      {/* ---- Agent 工具调用轮次 ---- */}
      {agentCfg && (
        <div className="task-card">
          <div style={{ fontSize: 14, fontWeight: 600, marginBottom: 2 }}>{t('settings.agentTitle')}</div>
          <div style={{ fontSize: 12, color: 'var(--text-muted)', marginBottom: 12 }}>{t('settings.agentDesc')}</div>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
            <div className="form-group">
              <label>{t('settings.plannerRounds')}</label>
              <input className="form-input" type="number" min={1} max={20} step={1} value={agentCfg.planner_max_tool_call_rounds} onChange={(e) => setAgentCfg({ ...agentCfg, planner_max_tool_call_rounds: Number(e.target.value) })} />
              <div className="form-hint">{t('settings.plannerRoundsHint')}</div>
            </div>
            <div className="form-group">
              <label>{t('settings.singleAgentRounds')}</label>
              <input className="form-input" type="number" min={1} max={500} step={1} value={agentCfg.single_agent_max_tool_call_rounds} onChange={(e) => setAgentCfg({ ...agentCfg, single_agent_max_tool_call_rounds: Number(e.target.value) })} />
              <div className="form-hint">{t('settings.singleAgentRoundsHint')}</div>
            </div>
          </div>
          <div style={{ marginTop: 12, display: 'flex', gap: 10, alignItems: 'center' }}>
            <button className="btn btn-primary" onClick={handleAgentSave} disabled={agentSaving}>{agentSaving ? t('common.saving') : t('common.save')}</button>
            {agentMsg && <span style={{ fontSize: 13, color: agentMsg.ok ? 'var(--success)' : 'var(--danger)' }}>{agentMsg.text}</span>}
          </div>
        </div>
      )}

      {/* ---- 数据库连接 ---- */}
      <p style={{ marginTop: 20, fontSize: 13, color: 'var(--text-muted)' }}>
        {t('settings.introLead')}{' '}
        <code>{cfg?.settings_path || 'config/db_config.json'}</code>
        {t('settings.introTail')}
      </p>

      {cfg?.active_desc && (
        <div style={{
          padding: '10px 12px', borderRadius: 4, marginBottom: 16, marginTop: 12, fontSize: 13,
          background: cfg.from_settings ? 'var(--success-bg)' : 'var(--warning-bg)',
          color: 'var(--text-primary)',
        }}>
          {t('settings.activeDesc')}<b>{cfg.active_desc}</b>
          {cfg.from_settings ? t('settings.matches') : t('settings.mismatch')}
        </div>
      )}

      <div className="form-group">
        <label>{t('settings.dbType')}</label>
        <select className="form-select" value={driver} onChange={(e) => setDriver(e.target.value as DBDriver)}>
          <option value="mysql">MySQL</option>
          <option value="sqlite">{t('settings.sqliteOption')}</option>
        </select>
      </div>

      {driver === 'mysql' ? (
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginTop: 12 }}>
          <div className="form-group">
            <label>{t('settings.host')}</label>
            <input className="form-input" value={host} onChange={(e) => setHost(e.target.value)} placeholder="127.0.0.1" />
          </div>
          <div className="form-group">
            <label>{t('settings.port')}</label>
            <input className="form-input" type="number" value={port} onChange={(e) => setPort(Number(e.target.value))} />
          </div>
          <div className="form-group">
            <label>{t('settings.user')}</label>
            <input className="form-input" value={user} onChange={(e) => setUser(e.target.value)} autoComplete="off" />
          </div>
          <div className="form-group">
            <label>{t('settings.password')}</label>
            <input className="form-input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="off" />
          </div>
          <div className="form-group">
            <label>{t('settings.dbname')}</label>
            <input className="form-input" value={dbname} onChange={(e) => setDbname(e.target.value)} />
          </div>
          <div className="form-group">
            <label>{t('settings.charset')}</label>
            <input className="form-input" value={charset} onChange={(e) => setCharset(e.target.value)} />
          </div>
        </div>
      ) : (
        <div className="form-group" style={{ marginTop: 12 }}>
          <label>{t('settings.dbPath')}</label>
          <input className="form-input" value={path} onChange={(e) => setPath(e.target.value)} placeholder="./data/flowagent.db" />
          <div className="form-hint">{t('settings.pathHint')}</div>
        </div>
      )}

      <div style={{ marginTop: 20, display: 'flex', gap: 8, alignItems: 'center' }}>
        <button className="btn btn-ghost" onClick={handleTest} disabled={testing}>
          {testing ? t('settings.testing') : t('settings.testConn')}
        </button>
        <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
          {saving ? t('common.saving') : t('common.save')}
        </button>
      </div>

      {testResult && (
        <div style={{
          marginTop: 12, padding: 12, borderRadius: 4, fontSize: 13,
          background: testResult.success ? 'var(--success-bg)' : 'var(--danger-bg)',
          color: 'var(--text-primary)',
        }}>
          <div style={{ fontWeight: 600, marginBottom: 6 }}>
            {testResult.success ? t('settings.connectOk') : t('settings.connectFail')}
          </div>
          <div style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{testResult.msg}</div>
        </div>
      )}

      {saveMsg && (
        <div style={{
          marginTop: 12, padding: 12, borderRadius: 4, fontSize: 13,
          background: 'var(--success-bg)', color: 'var(--text-primary)',
          whiteSpace: 'pre-wrap',
        }}>
          {saveMsg}
        </div>
      )}
    </div>
  );
}

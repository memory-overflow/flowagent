import { useState, useEffect, useCallback } from 'react';
import {
  listModels,
  updateModel,
  deleteModel,
  setDefaultModel,
  testModel,
} from '../services/api';
import type { ModelConfig } from '../types/api';
import { ModelProtocolLabel } from '../types/api';
import ModelFormModal from '../components/ModelFormModal';
import { useT } from '../i18n';

export default function ModelsPage() {
  const t = useT();
  const [models, setModels] = useState<ModelConfig[]>([]);
  const [loading, setLoading] = useState(true);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<ModelConfig | null>(null);
  const [testResult, setTestResult] = useState<{
    id: string;
    name: string;
    success: boolean;
    errorMsg?: string;
    reply?: string;
    supportsImage?: boolean;
    visionErrorMsg?: string;
    testing: boolean;
  } | null>(null);

  const fetch = useCallback(async () => {
    try {
      const res = await listModels();
      setModels(res.data.data?.list || []);
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetch(); }, [fetch]);

  const handleCreate = () => {
    setEditing(null);
    setModalOpen(true);
  };

  const handleEdit = (model: ModelConfig) => {
    setEditing(model);
    setModalOpen(true);
  };

  const handleDelete = async (id: string) => {
    if (!confirm(t('models.deleteConfirm'))) return;
    try {
      await deleteModel(id);
      setModels((prev) => prev.filter((m) => m.id !== id));
    } catch (e: any) {
      alert(t('models.deleteFailed', { msg: e.message }));
    }
  };

  const handleSetDefault = async (model: ModelConfig) => {
    try {
      await setDefaultModel(model.id);
      // 设为默认后，其余模型的 is_default 全部清掉，目标模型强制启用
      setModels((prev) =>
        prev.map((m) => ({
          ...m,
          is_default: m.id === model.id,
          enabled: m.id === model.id ? true : m.enabled,
        }))
      );
    } catch (e: any) {
      alert(t('models.setDefaultFailed', { msg: e.message }));
    }
  };

  const handleToggleEnabled = async (model: ModelConfig) => {
    // 默认模型不可停用（后端同样有硬校验兜底）
    if (model.enabled && model.is_default) {
      alert(t('models.defaultLocked'));
      return;
    }
    try {
      const next = !model.enabled;
      const res = await updateModel(model.id, { enabled: next });
      const updated = res.data.data;
      setModels((prev) => prev.map((m) => (m.id === model.id ? updated : m)));
    } catch (e: any) {
      alert(t('models.updateFailed', { msg: e.message }));
    }
  };

  const handleTest = async (model: ModelConfig) => {
    setTestResult({ id: model.id, name: model.name, success: false, testing: true });
    try {
      const res = await testModel({ id: model.id });
      setTestResult({
        id: model.id,
        name: model.name,
        success: res.data.data.success,
        errorMsg: res.data.data.error_msg,
        reply: res.data.data.reply,
        supportsImage: res.data.data.supports_image,
        visionErrorMsg: res.data.data.vision_error_msg,
        testing: false,
      });
    } catch (e: any) {
      setTestResult({
        id: model.id,
        name: model.name,
        success: false,
        errorMsg: e.message,
        testing: false,
      });
    }
  };

  return (
    <div className="page-container">
      <div className="page-header">
        <h2>{t('models.title')}</h2>
        <div style={{ display: 'flex', gap: 8 }}>
          <button className="btn btn-primary" onClick={handleCreate}>{t('models.newModel')}</button>
        </div>
      </div>

      <p style={{ marginTop: 0, fontSize: 13, color: 'var(--text-muted)' }}>
        {t('models.intro')}
      </p>

      {loading && <div style={{ padding: 20, textAlign: 'center', color: 'var(--text-muted)' }}>{t('common.loading')}</div>}

      {!loading && models.length === 0 && (
        <div className="empty-state">
          <div className="empty-icon">🤖</div>
          <div className="empty-title">{t('models.emptyTitle')}</div>
          <div className="empty-desc">{t('models.emptyDesc')}</div>
        </div>
      )}

      {!loading && models.length > 0 && (
        <table className="data-table">
          <thead>
            <tr>
              <th>{t('common.name')}</th>
              <th>{t('models.protocol')}</th>
              <th>{t('models.modelName')}</th>
              <th>{t('models.image')}</th>
              <th>{t('models.context')}</th>
              <th>{t('common.status')}</th>
              <th>{t('models.defaultBadge')}</th>
              <th>{t('models.updated')}</th>
              <th>{t('common.action')}</th>
            </tr>
          </thead>
          <tbody>
            {models.map((m) => (
              <tr key={m.id}>
                <td style={{ fontWeight: 500 }}>{m.name}</td>
                <td>
                  <span className="badge badge-info">{ModelProtocolLabel[m.protocol] || m.protocol}</span>
                </td>
                <td style={{ fontFamily: 'monospace', fontSize: 13 }}>{m.model}</td>
                <td>
                  {m.supports_image === undefined || m.supports_image === null ? (
                    <span className="badge badge-muted" title={t('models.imageUntestedTitle')}>{t('models.imageUntested')}</span>
                  ) : m.supports_image ? (
                    <span className="badge badge-success" title={t('models.imageMultimodalTitle')}>{t('models.imageMultimodal')}</span>
                  ) : (
                    <span className="badge badge-muted" title={t('models.imageTextOnlyTitle')}>{t('models.imageTextOnly')}</span>
                  )}
                </td>
                <td>
                  {m.context_limit ? (
                    <span
                      style={{ fontFamily: 'monospace', fontSize: 13 }}
                      title={t('models.contextTitle', {
                        limit: m.context_limit.toLocaleString(),
                        threshold: Math.round(m.context_limit * 0.8).toLocaleString(),
                      })}
                    >
                      {fmtContextLimit(m.context_limit)}
                    </span>
                  ) : (
                    <span
                      className="badge badge-warning"
                      title={t('models.contextMissingTitle')}
                    >
                      {t('models.contextMissing')}
                    </span>
                  )}
                </td>
                <td>
                  {(() => {
                    const lockDefault = m.enabled && m.is_default;
                    return (
                      <button
                        className={`badge ${m.enabled ? 'badge-success' : 'badge-muted'}`}
                        style={{
                          border: 'none',
                          cursor: lockDefault ? 'not-allowed' : 'pointer',
                          opacity: lockDefault ? 0.55 : 1,
                        }}
                        onClick={() => handleToggleEnabled(m)}
                        disabled={lockDefault}
                        title={
                          lockDefault
                            ? t('models.defaultLocked')
                            : m.enabled
                              ? t('models.clickDisable')
                              : t('models.clickEnable')
                        }
                      >
                        {m.enabled ? t('common.enabled') : t('common.disabled')}
                      </button>
                    );
                  })()}
                </td>
                <td>
                  {m.is_default ? (
                    <span className="badge badge-success">{t('models.defaultBadge')}</span>
                  ) : (
                    <button className="btn btn-ghost btn-sm" onClick={() => handleSetDefault(m)} title={t('models.setDefault')}>
                      {t('models.setDefault')}
                    </button>
                  )}
                </td>
                <td style={{ fontSize: 12, color: 'var(--text-muted)' }}>
                  {m.update_ts ? new Date(m.update_ts).toLocaleString('zh-CN') : '-'}
                </td>
                <td>
                  <div style={{ display: 'flex', gap: 4 }}>
                    <button className="btn btn-ghost btn-sm" onClick={() => handleTest(m)}>{t('models.test')}</button>
                    <button className="btn btn-ghost btn-sm" onClick={() => handleEdit(m)}>{t('common.edit')}</button>
                    <button className="btn btn-danger btn-sm" onClick={() => handleDelete(m.id)}>{t('common.delete')}</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {modalOpen && (
        <ModelFormModal
          model={editing}
          onClose={() => setModalOpen(false)}
          onSaved={(saved) => {
            if (editing) {
              setModels((prev) => prev.map((m) => (m.id === saved.id ? saved : m)));
            } else {
              setModels((prev) => [...prev, saved]);
            }
            setModalOpen(false);
          }}
        />
      )}

      {testResult && (
        <div className="modal-overlay" onClick={() => setTestResult(null)}>
          <div className="modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 480 }}>
            <div className="modal-header">
              <h3>
                {testResult.testing
                  ? t('models.testing')
                  : testResult.success
                    ? t('models.connectOk')
                    : t('models.connectFail')}
              </h3>
              <button className="modal-close" onClick={() => setTestResult(null)}>×</button>
            </div>
            <div className="modal-body">
              <p style={{ marginTop: 0, color: 'var(--text-muted)' }}>
                {t('models.testTarget', { name: testResult.name })}
              </p>
              {testResult.testing ? (
                <div style={{ padding: 12, textAlign: 'center', color: 'var(--text-muted)' }}>
                  {t('models.testingDesc')}
                </div>
              ) : testResult.success ? (
                <>
                  <div style={{ padding: 12, background: 'rgba(34,197,94,0.1)', borderRadius: 4 }}>
                    <div style={{ fontWeight: 600, marginBottom: 6 }}>{t('models.testReply')}</div>
                    <div style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                      {testResult.reply || t('common.empty')}
                    </div>
                  </div>
                  <div style={{
                    padding: 12,
                    marginTop: 8,
                    borderRadius: 4,
                    background: testResult.supportsImage ? 'rgba(34,197,94,0.1)' : 'rgba(245,158,11,0.12)',
                  }}>
                    <div style={{ fontWeight: 600, marginBottom: 6 }}>
                      {t('models.imageSupport', {
                        v: testResult.supportsImage
                          ? t('models.imageSupported')
                          : t('models.imageUnsupported'),
                      })}
                    </div>
                    {testResult.visionErrorMsg && (
                      <div style={{ fontSize: 12, color: 'var(--text-muted)', wordBreak: 'break-word' }}>
                        {testResult.visionErrorMsg}
                      </div>
                    )}
                  </div>
                </>
              ) : (
                <div style={{ padding: 12, background: 'rgba(239,68,68,0.1)', borderRadius: 4, color: 'var(--text-primary)' }}>
                  {testResult.errorMsg}
                </div>
              )}
            </div>
            <div className="modal-footer">
              <button className="btn btn-primary" onClick={() => setTestResult(null)}>{t('common.close')}</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

/**
 * 紧凑显示上下文上限：1048576 → "1M"、922000 → "922K"。
 *
 * 列表里只需一眼看出量级，精确值放在 title 里。
 */
function fmtContextLimit(n: number): string {
  if (n >= 1024 * 1024) {
    const m = n / (1024 * 1024);
    return Number.isInteger(m) ? `${m}M` : `${m.toFixed(1)}M`;
  }
  if (n >= 1000) return `${Math.round(n / 1000)}K`;
  return String(n);
}

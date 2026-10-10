import { useState } from 'react';
import { createModel, updateModel, testModel } from '../services/api';
import type { ModelConfig, ModelProtocol } from '../types/api';
import { useT } from '../i18n';

/**
 * 模型配置表单（新建 / 编辑共用）。
 *
 * 从 ModelsPage 抽出来是为了让「首次配置引导」也能复用同一份表单：
 * 校验规则、协议分支、连通性测试与图像探测回填逻辑只有一处实现，
 * 不会出现「引导里配出来的模型和管理页配的不一样」。
 */
export default function ModelFormModal({
  model,
  onClose,
  onSaved,
}: {
  model: ModelConfig | null;
  onClose: () => void;
  onSaved: (m: ModelConfig) => void;
}) {
  const t = useT();
  const [name, setName] = useState(model?.name || '');
  const [protocol, setProtocol] = useState<ModelProtocol>(model?.protocol || 'openai');
  const [baseUrl, setBaseUrl] = useState(model?.base_url || '');
  const [apiKey, setApiKey] = useState(model?.api_key || '');
  const [modelName, setModelName] = useState(model?.model || '');
  const [apiVersion, setApiVersion] = useState(model?.api_version || '');
  const [supportsImage, setSupportsImage] = useState(model?.supports_image || false);
  // 用字符串承接而非 number：受控 number input 在清空时会得到 NaN，
  // 存字符串才能表达「留空」这个状态（提交时转 0 = 未配置）
  const [contextLimit, setContextLimit] = useState(
    model?.context_limit ? String(model.context_limit) : '',
  );
  const [formTest, setFormTest] = useState<{ testing: boolean; result?: string } | null>(null);
  const [saving, setSaving] = useState(false);

  const handleSave = async () => {
    if (!name.trim()) return alert(t('models.errNameRequired'));
    if (!baseUrl.trim()) return alert(t('models.errBaseUrlRequired'));
    if (!modelName.trim()) return alert(t('models.errModelRequired'));
    setSaving(true);
    try {
      const payload = {
        name: name.trim(),
        protocol,
        base_url: baseUrl.trim(),
        api_key: apiKey.trim(),
        model: modelName.trim(),
        api_version: apiVersion.trim() || undefined,
        supports_image: supportsImage,
        context_limit: Number(contextLimit) || 0,
      };

      if (model?.id) {
        const res = await updateModel(model.id, payload);
        onSaved(res.data.data);
      } else {
        const res = await createModel(payload);
        onSaved(res.data.data);
      }
    } catch (e: any) {
      alert(t('models.saveFailed', { msg: e.message }));
    } finally {
      setSaving(false);
    }
  };

  // 用当前表单字段直连测试：连通性 + 图像理解探测，探测结果自动回填「支持图像理解」开关。
  const handleFormTest = async () => {
    if (!baseUrl.trim()) return alert(t('models.errTestBaseUrl'));
    if (!modelName.trim()) return alert(t('models.errTestModelName'));
    setFormTest({ testing: true });
    try {
      const res = await testModel({
        protocol,
        base_url: baseUrl.trim(),
        api_key: apiKey.trim(),
        model: modelName.trim(),
        api_version: apiVersion.trim() || undefined,
      });
      const d = res.data.data;
      if (!d.success) {
        setFormTest({
          testing: false,
          result: t('models.testFailResult', { msg: d.error_msg || t('common.unknown') }),
        });
        return;
      }
      setSupportsImage(!!d.supports_image);
      // vision_error_msg 是后端给的探测诊断，原样透出，不进词典
      const vision = d.supports_image ? t('models.visionSupported') : t('models.visionUnsupported');
      setFormTest({
        testing: false,
        result: d.vision_error_msg
          ? t('models.testOkResultErr', { v: vision, err: d.vision_error_msg })
          : t('models.testOkResult', { v: vision }),
      });
    } catch (e: any) {
      setFormTest({ testing: false, result: t('models.testFailed', { msg: e.message }) });
    }
  };

  // 表单是否被改动过（与初始值比对）。用于关闭前保护未保存内容。
  const isDirty =
    name !== (model?.name || '') ||
    protocol !== (model?.protocol || 'openai') ||
    baseUrl !== (model?.base_url || '') ||
    apiKey !== (model?.api_key || '') ||
    modelName !== (model?.model || '') ||
    apiVersion !== (model?.api_version || '') ||
    supportsImage !== (model?.supports_image || false) ||
    contextLimit !== (model?.context_limit ? String(model.context_limit) : '');

  const requestClose = () => {
    if (isDirty && !confirm(t('models.unsavedConfirm'))) return;
    onClose();
  };

  return (
    <div className="modal-overlay" onClick={requestClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>{model ? t('models.formEditTitle') : t('models.formCreateTitle')}</h3>

        <div className="form-group">
          <label>{t('models.nameRequired')}</label>
          <input className="form-input" value={name} onChange={(e) => setName(e.target.value)} placeholder={t('models.namePlaceholder')} />
        </div>

        <div className="form-group">
          <label>{t('models.protocol')}</label>
          <select className="form-select" value={protocol} onChange={(e) => setProtocol(e.target.value as ModelProtocol)}>
            <option value="openai">{t('models.protocolOpenai')}</option>
            <option value="gemini">{t('models.protocolGemini')}</option>
            <option value="gpt">{t('models.protocolGpt')}</option>
            <option value="claude">{t('models.protocolClaude')}</option>
          </select>
        </div>

        <div className="form-group">
          <label>{t('models.baseUrlRequired')}</label>
          <input
            className="form-input"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
            placeholder={
              protocol === 'gpt'
                ? 'https://<resource-name>.cognitiveservices.azure.com/openai'
                : protocol === 'gemini'
                  ? 'https://<gateway-host>/llm-service/gcs/public/publishers/google/models'
                  : protocol === 'claude'
                    ? 'https://api.anthropic.com'
                    : 'https://<openai-compatible-endpoint>/v1'
            }
          />
        </div>

        <div className="form-group">
          <label>{t('models.apiKey')}</label>
          <input
            className="form-input"
            type="password"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            placeholder={
              protocol === 'claude'
                ? t('models.apiKeyClaudePlaceholder')
                : 'sk-...'
            }
            autoComplete="off"
          />
        </div>

        <div className="form-group">
          <label>{t('models.modelNameRequired')}</label>
          <input
            className="form-input"
            value={modelName}
            onChange={(e) => setModelName(e.target.value)}
            placeholder={
              protocol === 'gpt'
                ? 'gpt-5.6-sol'
                : protocol === 'gemini'
                  ? 'gemini-3.1-pro-preview'
                  : protocol === 'claude'
                    ? 'hy4-preview'
                    : 'hy3'
            }
          />
        </div>

        <div className="form-group">
          <label>{t('models.contextLimitField')}</label>
          <input
            className="form-input"
            type="number"
            min={0}
            value={contextLimit}
            onChange={(e) => setContextLimit(e.target.value)}
            placeholder={t('models.contextLimitPlaceholder')}
          />
          <div className="form-hint">
            {t('models.contextLimitHint')}
          </div>
        </div>

        {protocol === 'gpt' && (
          <div className="form-group">
            <label>{t('models.apiVersion')}</label>
            <input
              className="form-input"
              value={apiVersion}
              onChange={(e) => setApiVersion(e.target.value)}
              placeholder={t('models.apiVersionPlaceholder')}
            />
          </div>
        )}

        <div className="form-group">
          <label>{t('models.imageSupportLabel')}</label>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
            <label style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
              <input
                type="checkbox"
                checked={supportsImage}
                onChange={(e) => setSupportsImage(e.target.checked)}
              />
              {t('models.supportsImage')}
            </label>
            <button
              type="button"
              className="btn btn-ghost btn-sm"
              onClick={handleFormTest}
              disabled={formTest?.testing}
            >
              {formTest?.testing ? t('models.testing') : t('models.test')}
            </button>
          </div>
          {formTest?.result && (
            <div style={{ marginTop: 8, fontSize: 12, color: 'var(--text-muted)', wordBreak: 'break-word' }}>
              {formTest.result}
            </div>
          )}
        </div>

        <div className="modal-footer">
          <button className="btn btn-ghost" onClick={requestClose}>{t('common.cancel')}</button>
          <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
            {saving ? t('common.saving') : t('common.save')}
          </button>
        </div>
      </div>
    </div>
  );
}

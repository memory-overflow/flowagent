import { useState, useEffect, useCallback, useRef } from 'react';
import {
  listSkills,
  createSkill,
  updateSkill,
  deleteSkill,
  listBuiltinSkill,
  importBuiltinSkill,
  importSkillZip,
  importSkillMD,
  type BuiltinSkillItem,
} from '../services/api';
import type { SkillConfig, SkillType } from '../types/api';
import { useT } from '../i18n';

// 后端 args/env 存储为 JSON 字符串，解析时兼容两种形态
function parseJsonArray(v: unknown): string[] {
  if (Array.isArray(v)) return v as string[];
  if (typeof v === 'string' && v.trim()) {
    try {
      const arr = JSON.parse(v);
      return Array.isArray(arr) ? (arr as string[]) : [];
    } catch {
      return [];
    }
  }
  return [];
}

function parseJsonObject(v: unknown): Record<string, string> {
  if (v && typeof v === 'object' && !Array.isArray(v)) {
    return v as Record<string, string>;
  }
  if (typeof v === 'string' && v.trim()) {
    try {
      const obj = JSON.parse(v);
      return obj && typeof obj === 'object' && !Array.isArray(obj) ? (obj as Record<string, string>) : {};
    } catch {
      return {};
    }
  }
  return {};
}

export default function SkillsPage() {
  const t = useT();
  const [skills, setSkills] = useState<SkillConfig[]>([]);
  const [loading, setLoading] = useState(true);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<SkillConfig | null>(null);
  const [builtinsOpen, setBuiltinsOpen] = useState(false);
  const [importOpen, setImportOpen] = useState(false);

  const fetch = useCallback(async () => {
    try {
      const res = await listSkills();
      setSkills(res.data.data?.list || []);
    } catch (e) {
      console.error(e);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetch(); }, [fetch]);

  const handleEdit = (skill: SkillConfig) => {
    setEditing(skill);
    setModalOpen(true);
  };

  const handleDelete = async (id: string) => {
    if (!confirm(t('skills.deleteConfirm'))) return;
    try {
      await deleteSkill(id);
      setSkills((prev) => prev.filter((s) => s.id !== id));
    } catch (e: any) {
      alert(t('skills.deleteFailed', { msg: e.message }));
    }
  };

  const typeBadge: Record<number, { cls: string; label: string }> = {
    1: { cls: 'badge-info', label: t('skills.typeInstruction') },
    2: { cls: 'badge-success', label: t('skills.typeExecutable') },
  };

  return (
    <div className="page-container">
      <div className="page-header">
        <h2>{t('skills.title')}</h2>
        <div style={{ display: 'flex', gap: 8 }}>
          <button className="btn btn-ghost" onClick={() => setBuiltinsOpen(true)}>
            {t('skills.importBuiltin')}
          </button>
          <button className="btn btn-primary" onClick={() => setImportOpen(true)}>{t('skills.importSkill')}</button>
        </div>
      </div>

      {loading && <div style={{ padding: 20, textAlign: 'center', color: 'var(--text-muted)' }}>{t('common.loading')}</div>}

      {!loading && skills.length === 0 && (
        <div className="empty-state">
          <div className="empty-icon">🧩</div>
          <div className="empty-title">{t('skills.emptyTitle')}</div>
          <div className="empty-desc">{t('skills.emptyDesc')}</div>
        </div>
      )}

      {!loading && skills.length > 0 && (
        <table className="data-table">
          <thead>
            <tr>
              <th>{t('common.name')}</th>
              <th>{t('common.type')}</th>
              <th>{t('common.desc')}</th>
              <th>{t('skills.updatedAt')}</th>
              <th>{t('common.action')}</th>
            </tr>
          </thead>
          <tbody>
            {skills.map((s) => (
              <tr key={s.id}>
                <td style={{ fontWeight: 500 }}>{s.name_display || s.name}</td>
                <td>
                  <span className={`badge ${typeBadge[s.type]?.cls || 'badge-muted'}`}>
                    {typeBadge[s.type]?.label || t('common.unknown')}
                  </span>
                </td>
                <td style={{ maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {s.description_display || s.description || '-'}
                </td>
                <td style={{ fontSize: 12, color: 'var(--text-muted)' }}>
                  {s.update_ts ? new Date(s.update_ts).toLocaleString('zh-CN') : '-'}
                </td>
                <td>
                  <div style={{ display: 'flex', gap: 4 }}>
                    <button className="btn btn-ghost btn-sm" onClick={() => handleEdit(s)}>{t('common.edit')}</button>
                    <button className="btn btn-danger btn-sm" onClick={() => handleDelete(s.id!)}>{t('common.delete')}</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {modalOpen && (
        <SkillForm
          skill={editing}
          onClose={() => setModalOpen(false)}
          onSaved={(saved) => {
            if (editing) {
              setSkills((prev) => prev.map((s) => (s.id === saved.id ? saved : s)));
            } else {
              setSkills((prev) => [...prev, saved]);
            }
            setModalOpen(false);
          }}
        />
      )}

      {importOpen && (
        <ImportSkillModal
          onClose={() => setImportOpen(false)}
          onImported={() => fetch()}
          onManualCreate={() => {
            setImportOpen(false);
            setEditing(null);
            setModalOpen(true);
          }}
        />
      )}

      {builtinsOpen && (
        <BuiltinSkillModal
          onClose={() => setBuiltinsOpen(false)}
          onImported={() => {
            setBuiltinsOpen(false);
            fetch();
          }}
        />
      )}
    </div>
  );
}

// ============================================================
// Import Skill Modal — 拖拽 zip 导入 / 手动新建入口
// ============================================================

type ZipImportResult = {
  skills: Array<{ skill?: { name?: string } | null; key: string; action: string }>;
  total: number;
  total_files: number;
};

function ImportSkillModal({
  onClose,
  onImported,
  onManualCreate,
}: {
  onClose: () => void;
  onImported: () => void;
  onManualCreate: () => void;
}) {
  const t = useT();
  const [dragging, setDragging] = useState(false);
  const [importing, setImporting] = useState(false);
  const [overwrite, setOverwrite] = useState(true);
  const [result, setResult] = useState<ZipImportResult | null>(null);
  const [error, setError] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const dragDepth = useRef(0);

  const handleFile = async (file: File | undefined | null) => {
    if (!file || importing) return;
    const isZip = file.name.toLowerCase().endsWith('.zip');
    const isMD = file.name.toLowerCase().endsWith('.md');
    if (!isZip && !isMD) {
      setError(t('skills.import.needZipOrMd'));
      return;
    }
    setError('');
    setResult(null);
    setImporting(true);
    try {
      const res = isZip ? await importSkillZip(file, overwrite) : await importSkillMD(file, overwrite);
      setResult(res.data.data);
      onImported();
    } catch (e: any) {
      setError(t('skills.import.failed', { msg: e?.response?.data?.msg || e.message }));
    } finally {
      setImporting(false);
    }
  };

  const onDrop = (e: React.DragEvent) => {
    e.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    handleFile(e.dataTransfer.files?.[0]);
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 560 }}>
        <div className="modal-header">
          <h3>{t('skills.import.title')}</h3>
          <button className="modal-close" onClick={onClose}>×</button>
        </div>
        <div className="modal-body">
          <div
            className={`import-dropzone${dragging ? ' dragging' : ''}`}
            onClick={() => !importing && inputRef.current?.click()}
            onDragEnter={(e) => { e.preventDefault(); dragDepth.current++; setDragging(true); }}
            onDragOver={(e) => e.preventDefault()}
            onDragLeave={() => { dragDepth.current--; if (dragDepth.current <= 0) { dragDepth.current = 0; setDragging(false); } }}
            onDrop={onDrop}
          >
            <div className="import-dropzone-icon">{importing ? '⏳' : '📥'}</div>
            <div className="import-dropzone-text">
              {importing ? t('skills.import.importing') : t('skills.import.dropzone')}
            </div>
            <input
              ref={inputRef}
              type="file"
              accept=".zip,.md"
              style={{ display: 'none' }}
              onChange={(e) => { const f = e.target.files?.[0]; e.target.value = ''; handleFile(f); }}
            />
          </div>

          <label style={{ display: 'flex', gap: 8, alignItems: 'center', marginTop: 12, fontSize: 13 }}>
            <input type="checkbox" checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />
            {t('skills.import.overwrite')}
          </label>

          <div className="import-requirements">
            <div className="import-requirements-title">{t('skills.import.reqTitle')}</div>
            <ul>
              <li>{t('skills.import.req1Lead')}<code>skill.json</code>{t('skills.import.req1Mid')}<code>SKILL.md</code>{t('skills.import.req1Tail')}</li>
              <li><code>SKILL.md</code>{t('skills.import.req2Tail')}</li>
              <li>{t('skills.import.req3')}</li>
            </ul>
          </div>

          {error && (
            <div style={{ marginTop: 12, padding: 10, background: 'rgba(239,68,68,0.08)', borderRadius: 4, fontSize: 13, color: 'var(--danger, #ef4444)' }}>
              {error}
            </div>
          )}

          {result && (
            <div style={{ marginTop: 12, padding: 10, background: 'rgba(34,197,94,0.08)', borderRadius: 4, fontSize: 13 }}>
              <div>
                {t('skills.import.complete', {
                  total: result.total,
                  created: result.skills.filter((s) => s.action === 'created').length,
                  updated: result.skills.filter((s) => s.action === 'updated').length,
                  skipped: result.skills.filter((s) => s.action === 'skipped').length,
                  files: result.total_files,
                })}
              </div>
              {result.skills
                .filter((s) => s.action !== 'skipped')
                .map((s) => (
                  <div key={s.key} style={{ marginTop: 4 }}>
                    • {s.skill?.name || s.key}
                    <span style={{ color: 'var(--text-muted)' }}>{s.action === 'created' ? t('skills.import.tagCreated') : t('skills.import.tagUpdated')}</span>
                  </div>
                ))}
            </div>
          )}
        </div>
        <div className="modal-footer">
          <button className="btn btn-ghost" onClick={onManualCreate}>{t('skills.import.manualCreate')}</button>
          <button className="btn btn-primary" onClick={onClose}>{t('skills.done')}</button>
        </div>
      </div>
    </div>
  );
}

// ============================================================
// Skill Form Modal
// ============================================================

function SkillForm({
  skill,
  onClose,
  onSaved,
}: {
  skill: SkillConfig | null;
  onClose: () => void;
  onSaved: (s: SkillConfig) => void;
}) {
  const t = useT();
  const [name, setName] = useState(skill?.name || '');
  const [type, setType] = useState<SkillType>(skill?.type ?? 1);
  const [description, setDescription] = useState(skill?.description || '');
  const [content, setContent] = useState(skill?.content || '');
  const [command, setCommand] = useState(skill?.command || '');
  const [args, setArgs] = useState(parseJsonArray(skill?.args).join(' '));
  const [env, setEnv] = useState(
    Object.entries(parseJsonObject(skill?.env)).map(([k, v]) => `${k}=${v}`).join('\n')
  );
  const [saving, setSaving] = useState(false);

  const handleSave = async () => {
    if (!name.trim()) return alert(t('skills.form.nameRequired'));
    setSaving(true);
    try {
      const envObj: Record<string, string> = {};
      if (env.trim()) {
        env.split('\n').forEach((line) => {
          const idx = line.indexOf('=');
          if (idx > 0) envObj[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
        });
      }

      const payload: any = {
        name: name.trim(),
        description: description.trim(),
        type,
        content: content,
      };
      if (type === 2) {
        payload.command = command.trim() || undefined;
        payload.args = args.trim() ? args.trim().split(/\s+/) : undefined;
        payload.env = Object.keys(envObj).length > 0 ? envObj : undefined;
      }

      if (skill?.id) {
        const res = await updateSkill(skill.id, payload);
        onSaved(res.data.data);
      } else {
        const res = await createSkill(payload);
        onSaved(res.data.data);
      }
    } catch (e: any) {
      alert(t('skills.form.saveFailed', { msg: e.message }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>{skill ? t('skills.form.editTitle') : t('skills.form.createTitle')}</h3>

        <div className="form-group">
          <label>{t('skills.form.name')}</label>
          <input className="form-input" value={name} onChange={(e) => setName(e.target.value)} placeholder={t('skills.form.namePlaceholder')} />
        </div>

        <div className="form-group">
          <label>{t('common.type')}</label>
          <select className="form-select" value={type} onChange={(e) => setType(Number(e.target.value) as SkillType)}>
            <option value={1}>{t('skills.form.typeInstructionOption')}</option>
            <option value={2}>{t('skills.form.typeExecutableOption')}</option>
          </select>
        </div>

        <div className="form-group">
          <label>{t('skills.form.content')}</label>
          <textarea
            className="form-textarea"
            value={content}
            onChange={(e) => setContent(e.target.value)}
            placeholder={type === 1
              ? t('skills.form.contentPlaceholderInstruction')
              : t('skills.form.contentPlaceholderExecutable')}
            style={{ minHeight: 100 }}
          />
        </div>

        {type === 2 && (
          <>
            <div className="form-group">
              <label>{t('skills.form.command')}</label>
              <input className="form-input" value={command} onChange={(e) => setCommand(e.target.value)} placeholder={t('skills.form.commandPlaceholder')} />
            </div>
            <div className="form-group">
              <label>{t('skills.form.args')}</label>
              <input className="form-input" value={args} onChange={(e) => setArgs(e.target.value)} placeholder={t('skills.form.argsPlaceholder')} />
            </div>
            <div className="form-group">
              <label>{t('skills.form.env')}</label>
              <textarea className="form-textarea" value={env} onChange={(e) => setEnv(e.target.value)} placeholder={t('skills.form.envPlaceholder')} />
            </div>
          </>
        )}

        <div className="form-group">
          <label>{t('common.desc')}</label>
          <input className="form-input" value={description} onChange={(e) => setDescription(e.target.value)} placeholder={t('skills.form.descPlaceholder')} />
        </div>

        <div className="modal-footer">
          <button className="btn btn-ghost" onClick={onClose}>{t('common.cancel')}</button>
          <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
            {saving ? t('common.saving') : t('common.save')}
          </button>
        </div>
      </div>
    </div>
  );
}

// ============================================================
// Builtin Skill Modal — 一键导入内置 Skill
// ============================================================

function BuiltinSkillModal({
  onClose,
  onImported,
}: {
  onClose: () => void;
  onImported: () => void;
}) {
  const t = useT();
  const [items, setItems] = useState<BuiltinSkillItem[]>([]);
  const [dir, setDir] = useState('');
  const [loading, setLoading] = useState(true);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [overwrite, setOverwrite] = useState(false);
  const [importing, setImporting] = useState(false);
  const [result, setResult] = useState<{ imported: string[]; updated: string[]; skipped: string[] } | null>(null);

  useEffect(() => {
    (async () => {
      try {
        const res = await listBuiltinSkill();
        const list = res.data.data?.list || [];
        setItems(list);
        setDir(res.data.data?.dir || '');
        setSelected(new Set(list.filter((i) => !i.already_imported).map((i) => i.key)));
      } catch (e) {
        console.error(e);
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  const toggle = (key: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const handleImport = async () => {
    if (selected.size === 0) return alert(t('skills.builtin.selectAtLeastOne'));
    setImporting(true);
    try {
      const res = await importBuiltinSkill(Array.from(selected), overwrite);
      setResult(res.data.data);
    } catch (e: any) {
      alert(t('skills.builtin.importFailed', { msg: e.message }));
    } finally {
      setImporting(false);
    }
  };

  const typeLabel: Record<number, string> = {
    1: t('skills.typeInstruction'),
    2: t('skills.typeExecutable'),
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 640 }}>
        <div className="modal-header">
          <h3>{t('skills.importBuiltin')}</h3>
          <button className="modal-close" onClick={onClose}>×</button>
        </div>
        <div className="modal-body">
          {dir && (
            <p style={{ marginTop: 0, fontSize: 12, color: 'var(--text-muted)' }}>
              {t('skills.builtin.source')}<code>{dir}</code>
            </p>
          )}

          {loading && <div style={{ padding: 20, textAlign: 'center' }}>{t('common.loading')}</div>}

          {!loading && items.length === 0 && (
            <div className="empty-state">
              <div className="empty-title">{t('skills.builtin.emptyTitle')}</div>
              <div className="empty-desc">
                {t('skills.builtin.emptyDescLead')}<code>skills/builtins/</code>{t('skills.builtin.emptyDescMid')}<code>*.json</code>{t('skills.builtin.emptyDescTail')}
              </div>
            </div>
          )}

          {!loading && items.length > 0 && (
            <>
              <div style={{ maxHeight: 360, overflowY: 'auto', border: '1px solid var(--border)', borderRadius: 4 }}>
                {items.map((it) => (
                  <label
                    key={it.key}
                    style={{
                      display: 'flex',
                      gap: 10,
                      padding: '10px 12px',
                      borderBottom: '1px solid var(--border)',
                      cursor: 'pointer',
                      alignItems: 'flex-start',
                    }}
                  >
                    <input
                      type="checkbox"
                      style={{ marginTop: 4 }}
                      checked={selected.has(it.key)}
                      onChange={() => toggle(it.key)}
                    />
                    <div style={{ flex: 1 }}>
                      <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                        <span style={{ fontWeight: 600 }}>{it.name_display || it.name}</span>
                        <span className="badge badge-info" style={{ fontSize: 11 }}>{typeLabel[it.type] || t('common.unknown')}</span>
                        {it.already_imported && (
                          <span className="badge badge-muted" style={{ fontSize: 11 }}>{t('skills.builtin.exists')}</span>
                        )}
                      </div>
                      {(it.description_display || it.description) && (
                        <div style={{ fontSize: 12, color: 'var(--text-muted)', marginTop: 2 }}>
                          {it.description_display || it.description}
                        </div>
                      )}
                      {it.command && (
                        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4, fontFamily: 'monospace' }}>
                          {it.command} {(it.args || []).join(' ')}
                        </div>
                      )}
                    </div>
                  </label>
                ))}
              </div>

              <label style={{ display: 'flex', gap: 8, alignItems: 'center', marginTop: 12, fontSize: 13 }}>
                <input type="checkbox" checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />
                {t('skills.builtin.overwrite')}
              </label>

              {result && (
                <div style={{ marginTop: 12, padding: 10, background: 'rgba(34,197,94,0.08)', borderRadius: 4, fontSize: 13 }}>
                  <div>{t('skills.builtin.resultCreated')}{(result.imported && result.imported.length) ? result.imported.join(t('skills.builtin.listSep')) : t('skills.builtin.resultNone')}</div>
                  <div>{t('skills.builtin.resultUpdated')}{(result.updated && result.updated.length) ? result.updated.join(t('skills.builtin.listSep')) : t('skills.builtin.resultNone')}</div>
                  <div>{t('skills.builtin.resultSkipped')}{(result.skipped && result.skipped.length) ? result.skipped.join(t('skills.builtin.listSep')) : t('skills.builtin.resultNone')}</div>
                </div>
              )}
            </>
          )}
        </div>
        <div className="modal-footer">
          <button className="btn btn-ghost" onClick={onClose}>{t('common.cancel')}</button>
          {result ? (
            <button className="btn btn-primary" onClick={onImported}>{t('skills.done')}</button>
          ) : (
            <button className="btn btn-primary" disabled={importing || loading || items.length === 0} onClick={handleImport}>
              {importing ? t('skills.builtin.importing') : t('skills.builtin.import', { n: selected.size })}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

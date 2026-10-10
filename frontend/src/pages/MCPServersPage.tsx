import { useState, useEffect, useCallback } from 'react';
import {
  listMCPServers,
  createMCPServer,
  updateMCPServer,
  deleteMCPServer,
  testMCPServer,
  listBuiltinMCP,
  importBuiltinMCP,
  type BuiltinMCPItem,
} from '../services/api';
import type { MCPServerConfig, MCPTransport } from '../types/api';
import { useT } from '../i18n';

// 后端 args/env/headers 存储为 JSON 字符串, 解析时兼容两种形态
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

export default function MCPServersPage() {
  const t = useT();
  const [servers, setServers] = useState<MCPServerConfig[]>([]);
  const [loading, setLoading] = useState(true);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<MCPServerConfig | null>(null);
  const [builtinsOpen, setBuiltinsOpen] = useState(false);

  const fetch = useCallback(async () => {
    try {
      const res = await listMCPServers();
      setServers(res.data.data?.list || []);
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

  const handleEdit = (server: MCPServerConfig) => {
    setEditing(server);
    setModalOpen(true);
  };

  const handleDelete = async (id: string) => {
    if (!confirm(t('mcp.deleteConfirm'))) return;
    try {
      await deleteMCPServer(id);
      setServers((prev) => prev.filter((s) => s.id !== id));
    } catch (e: any) {
      alert(t('mcp.deleteFailed', { msg: e.message }));
    }
  };

  const [testResult, setTestResult] = useState<{
    success: boolean;
    errorMsg?: string;
    tools?: { name: string; description: string }[];
  } | null>(null);

  const handleTest = async (server: MCPServerConfig) => {
    try {
      const res = await testMCPServer(server.id!);
      setTestResult({
        success: res.data.data.success,
        errorMsg: res.data.data.error_msg,
        tools: res.data.data.tools,
      });
    } catch (e: any) {
      setTestResult({ success: false, errorMsg: e.message });
    }
  };

  // 传输方式的显示文案走词典，值仍是 'stdio' / 'sse' / 'streamable_http'
  const transportLabel: Record<string, string> = {
    stdio: t('mcp.transportStdio'),
    sse: t('mcp.transportSse'),
    streamable_http: t('mcp.transportStreamableHttp'),
  };

  const statusBadge: Record<number, { cls: string; label: string }> = {
    1: { cls: 'badge-success', label: t('mcp.statusActive') },
    2: { cls: 'badge-muted', label: t('mcp.statusInactive') },
    3: { cls: 'badge-danger', label: t('mcp.statusError') },
  };

  return (
    <div className="page-container">
      <div className="page-header">
        <h2>{t('mcp.title')}</h2>
        <div style={{ display: 'flex', gap: 8 }}>
          <button className="btn btn-ghost" onClick={() => setBuiltinsOpen(true)}>
            {t('mcp.importBuiltin')}
          </button>
          <button className="btn btn-primary" onClick={handleCreate}>{t('mcp.newServer')}</button>
        </div>
      </div>

      {loading && <div style={{ padding: 20, textAlign: 'center', color: 'var(--text-muted)' }}>{t('common.loading')}</div>}

      {!loading && servers.length === 0 && (
        <div className="empty-state">
          <div className="empty-icon">🔌</div>
          <div className="empty-title">{t('mcp.emptyTitle')}</div>
          <div className="empty-desc">{t('mcp.emptyDesc')}</div>
        </div>
      )}

      {!loading && servers.length > 0 && (
        <table className="data-table">
          <thead>
            <tr>
              <th>{t('common.name')}</th>
              <th>{t('mcp.transport')}</th>
              <th>{t('common.desc')}</th>
              <th>{t('common.status')}</th>
              <th>{t('mcp.updatedAt')}</th>
              <th>{t('common.action')}</th>
            </tr>
          </thead>
          <tbody>
            {servers.map((s) => (
              <tr key={s.id}>
                <td style={{ fontWeight: 500 }}>{s.name_display || s.name}</td>
                <td><span className="badge badge-info">{transportLabel[s.transport] || s.transport}</span></td>
                <td style={{ maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {s.description_display || s.description || '-'}
                </td>
                <td>
                  <span className={`badge ${statusBadge[s.status]?.cls || 'badge-muted'}`}>
                    {statusBadge[s.status]?.label || t('common.unknown')}
                  </span>
                </td>
                <td style={{ fontSize: 12, color: 'var(--text-muted)' }}>
                  {s.update_ts ? new Date(s.update_ts).toLocaleString('zh-CN') : '-'}
                </td>
                <td>
                  <div style={{ display: 'flex', gap: 4 }}>
                    <button className="btn btn-ghost btn-sm" onClick={() => handleTest(s)}>{t('mcp.test')}</button>
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
        <MCPServerForm
          server={editing}
          onClose={() => setModalOpen(false)}
          onSaved={(saved) => {
            if (editing) {
              setServers((prev) => prev.map((s) => (s.id === saved.id ? saved : s)));
            } else {
              setServers((prev) => [...prev, saved]);
            }
            setModalOpen(false);
          }}
        />
      )}

      {testResult && (
        <div className="modal-overlay" onClick={() => setTestResult(null)}>
          <div className="modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 520 }}>
            <div className="modal-header">
              <h3>{testResult.success ? t('mcp.connectOk') : t('mcp.connectFail')}</h3>
              <button className="modal-close" onClick={() => setTestResult(null)}>×</button>
            </div>
            <div className="modal-body">
              {testResult.success ? (
                <>
                  <p style={{ color: 'var(--text-muted)', marginTop: 0 }}>
                    {t('mcp.toolsFound', { n: testResult.tools?.length || 0 })}
                  </p>
                  <div style={{ maxHeight: 320, overflowY: 'auto', border: '1px solid var(--border)', borderRadius: 4 }}>
                    {(testResult.tools || []).map((t) => (
                      <div key={t.name} style={{ padding: '10px 12px', borderBottom: '1px solid var(--border)' }}>
                        <div style={{ fontWeight: 600, color: 'var(--text-primary)' }}>{t.name}</div>
                        {t.description && (
                          <div style={{ fontSize: 12, color: 'var(--text-muted)', marginTop: 2 }}>{t.description}</div>
                        )}
                      </div>
                    ))}
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

      {builtinsOpen && (
        <BuiltinMCPModal
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
// MCP Server Form Modal
// ============================================================

function MCPServerForm({
  server,
  onClose,
  onSaved,
}: {
  server: MCPServerConfig | null;
  onClose: () => void;
  onSaved: (s: MCPServerConfig) => void;
}) {
  const t = useT();
  const [name, setName] = useState(server?.name || '');
  const [transport, setTransport] = useState<MCPTransport>(server?.transport || 'stdio');
  const [command, setCommand] = useState(server?.command || '');
  // 后端 args/env/headers 都是 JSON 字符串, 这里统一解析为数组/对象
  const [args, setArgs] = useState(parseJsonArray(server?.args).join(' '));
  const [env, setEnv] = useState(
    Object.entries(parseJsonObject(server?.env)).map(([k, v]) => `${k}=${v}`).join('\n')
  );
  const [url, setUrl] = useState(server?.url || '');
  const [headers, setHeaders] = useState(
    Object.entries(parseJsonObject(server?.headers)).map(([k, v]) => `${k}: ${v}`).join('\n')
  );
  const [description, setDescription] = useState(server?.description || '');
  const [saving, setSaving] = useState(false);

  const handleSave = async () => {
    if (!name.trim()) return alert(t('mcp.form.nameRequired'));
    setSaving(true);
    try {
      const envObj: Record<string, string> = {};
      if (env.trim()) {
        env.split('\n').forEach((line) => {
          const idx = line.indexOf('=');
          if (idx > 0) envObj[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
        });
      }
      const headersObj: Record<string, string> = {};
      if (headers.trim()) {
        headers.split('\n').forEach((line) => {
          const idx = line.indexOf(':');
          if (idx > 0) headersObj[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
        });
      }

      const payload = {
        name: name.trim(),
        transport,
        command: command.trim() || undefined,
        args: args.trim() ? args.trim().split(/\s+/) : undefined,
        env: Object.keys(envObj).length > 0 ? envObj : undefined,
        url: url.trim() || undefined,
        headers: Object.keys(headersObj).length > 0 ? headersObj : undefined,
        description: description.trim(),
      };

      if (server?.id) {
        const res = await updateMCPServer(server.id, payload);
        onSaved(res.data.data);
      } else {
        const res = await createMCPServer(payload as any);
        onSaved(res.data.data);
      }
    } catch (e: any) {
      alert(t('mcp.form.saveFailed', { msg: e.message }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>{server ? t('mcp.form.editTitle') : t('mcp.form.createTitle')}</h3>

        <div className="form-group">
          <label>{t('mcp.form.name')}</label>
          <input className="form-input" value={name} onChange={(e) => setName(e.target.value)} placeholder={t('mcp.form.namePlaceholder')} />
        </div>

        <div className="form-group">
          <label>{t('mcp.transport')}</label>
          <select className="form-select" value={transport} onChange={(e) => setTransport(e.target.value as MCPTransport)}>
            <option value="stdio">{t('mcp.transportStdioOption')}</option>
            <option value="sse">{t('mcp.transportSseOption')}</option>
            <option value="streamable_http">{t('mcp.transportStreamableHttpOption')}</option>
          </select>
        </div>

        {transport === 'stdio' && (
          <>
            <div className="form-group">
              <label>{t('mcp.form.command')}</label>
              <input className="form-input" value={command} onChange={(e) => setCommand(e.target.value)} placeholder={t('mcp.form.commandPlaceholder')} />
            </div>
            <div className="form-group">
              <label>{t('mcp.form.args')}</label>
              <input className="form-input" value={args} onChange={(e) => setArgs(e.target.value)} placeholder={t('mcp.form.argsPlaceholder')} />
            </div>
            <div className="form-group">
              <label>{t('mcp.form.env')}</label>
              <textarea className="form-textarea" value={env} onChange={(e) => setEnv(e.target.value)} placeholder={t('mcp.form.envPlaceholder')} />
            </div>
          </>
        )}

        {(transport === 'sse' || transport === 'streamable_http') && (
          <>
            <div className="form-group">
              <label>{t('mcp.form.url')}</label>
              <input className="form-input" value={url} onChange={(e) => setUrl(e.target.value)} placeholder={t('mcp.form.urlPlaceholder')} />
            </div>
            <div className="form-group">
              <label>{t('mcp.form.headers')}</label>
              <textarea className="form-textarea" value={headers} onChange={(e) => setHeaders(e.target.value)} placeholder={t('mcp.form.headersPlaceholder')} />
            </div>
          </>
        )}

        <div className="form-group">
          <label>{t('common.desc')}</label>
          <input className="form-input" value={description} onChange={(e) => setDescription(e.target.value)} placeholder={t('mcp.form.descPlaceholder')} />
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
// Builtin MCP Modal — 一键导入内置 MCP
// ============================================================

function BuiltinMCPModal({
  onClose,
  onImported,
}: {
  onClose: () => void;
  onImported: () => void;
}) {
  const t = useT();
  const [items, setItems] = useState<BuiltinMCPItem[]>([]);
  const [dir, setDir] = useState('');
  const [loading, setLoading] = useState(true);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [overwrite, setOverwrite] = useState(false);
  const [importing, setImporting] = useState(false);
  const [result, setResult] = useState<{ imported: string[]; updated: string[]; skipped: string[] } | null>(null);

  useEffect(() => {
    (async () => {
      try {
        const res = await listBuiltinMCP();
        const list = res.data.data?.list || [];
        setItems(list);
        setDir(res.data.data?.dir || '');
        // 默认勾选所有还未导入的
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
    if (selected.size === 0) return alert(t('mcp.builtin.selectAtLeastOne'));
    setImporting(true);
    try {
      const res = await importBuiltinMCP(Array.from(selected), overwrite);
      setResult(res.data.data);
    } catch (e: any) {
      alert(t('mcp.builtin.importFailed', { msg: e.message }));
    } finally {
      setImporting(false);
    }
  };

  // 传输方式的显示文案走词典，值仍是 'stdio' / 'sse' / 'streamable_http'
  const transportLabel: Record<string, string> = {
    stdio: t('mcp.transportStdio'),
    sse: t('mcp.transportSse'),
    streamable_http: t('mcp.transportStreamableHttp'),
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 640 }}>
        <div className="modal-header">
          <h3>{t('mcp.importBuiltin')}</h3>
          <button className="modal-close" onClick={onClose}>×</button>
        </div>
        <div className="modal-body">
          {dir && (
            <p style={{ marginTop: 0, fontSize: 12, color: 'var(--text-muted)' }}>
              {t('mcp.builtin.source')}<code>{dir}</code>
            </p>
          )}

          {loading && <div style={{ padding: 20, textAlign: 'center' }}>{t('common.loading')}</div>}

          {!loading && items.length === 0 && (
            <div className="empty-state">
              <div className="empty-title">{t('mcp.builtin.emptyTitle')}</div>
              <div className="empty-desc">
                {t('mcp.builtin.emptyDescLead')}<code>mcp/builtins/</code>{t('mcp.builtin.emptyDescMid')}<code>*.json</code>{t('mcp.builtin.emptyDescTail')}
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
                        <span className="badge badge-info" style={{ fontSize: 11 }}>{transportLabel[it.transport] || it.transport}</span>
                        {it.already_imported && (
                          <span className="badge badge-muted" style={{ fontSize: 11 }}>{t('mcp.builtin.exists')}</span>
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
                {t('mcp.builtin.overwrite')}
              </label>

              {result && (
                <div style={{ marginTop: 12, padding: 10, background: 'rgba(34,197,94,0.08)', borderRadius: 4, fontSize: 13 }}>
                  <div>{t('mcp.builtin.resultCreated')}{(result.imported && result.imported.length) ? result.imported.join(t('mcp.builtin.listSep')) : t('mcp.builtin.resultNone')}</div>
                  <div>{t('mcp.builtin.resultUpdated')}{(result.updated && result.updated.length) ? result.updated.join(t('mcp.builtin.listSep')) : t('mcp.builtin.resultNone')}</div>
                  <div>{t('mcp.builtin.resultSkipped')}{(result.skipped && result.skipped.length) ? result.skipped.join(t('mcp.builtin.listSep')) : t('mcp.builtin.resultNone')}</div>
                </div>
              )}
            </>
          )}
        </div>
        <div className="modal-footer">
          <button className="btn btn-ghost" onClick={onClose}>{t('common.cancel')}</button>
          {result ? (
            <button className="btn btn-primary" onClick={onImported}>{t('mcp.builtin.done')}</button>
          ) : (
            <button className="btn btn-primary" disabled={importing || loading || items.length === 0} onClick={handleImport}>
              {importing ? t('mcp.builtin.importing') : t('mcp.builtin.import', { n: selected.size })}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

import { useState } from 'react';
import type { FileInfo } from '../types/api';
import { openLocalFile } from '../services/api';
import FilePreview, { previewKindFor } from './FilePreview';
import { useT } from '../i18n';

// ============================================================
// 文件列表（任务产物 / 用户附件通用）
//
// 从「纯缩略图网格」改为「图标 + 文件名 + 大小 + 操作」的卡片列表：
// 产物大多是 .md / .go / .csv 这类没有视觉预览的文件，只给一个 📄 图标
// 用户根本不知道那是什么、也无法取用。文件名与操作入口才是核心信息。
//
// 本地 agent 场景下，文件就躺在后端工作区里：
//   - 点击卡片 → 浏览器内直接预览（pdf / 图片 / 视频 / 音频 / 文本 / markdown）
//   - 「打开」按钮 → 用系统默认应用在本地打开（替代「下载」）
// ============================================================

interface Props {
  files: FileInfo[];
  /** 标题，如「产物文件」；不传则不显示标题行 */
  label?: string;
  /** 紧凑模式：用于消息气泡内嵌，去掉标题与外边距 */
  compact?: boolean;
}

/** 超过该数量时默认折叠，只展示前 collapseLimit 个，其余点「展开全部」再看 */
const COLLAPSE_LIMIT = 6;

const FILE_TYPE_ICONS: Record<number, string> = {
  1: '🖼',
  2: '🎬',
  3: '📄',
  4: '📦',
};

/** 按扩展名给出更具体的图标，比 file_type 粗分类更有辨识度 */
function iconFor(f: FileInfo): string {
  const ext = (f.name.split('.').pop() || '').toLowerCase();
  const byExt: Record<string, string> = {
    md: '📝', markdown: '📝', txt: '📄',
    pdf: '📕', doc: '📘', docx: '📘',
    xls: '📊', xlsx: '📊', csv: '📊', tsv: '📊',
    ppt: '📙', pptx: '📙',
    json: '🧾', yaml: '🧾', yml: '🧾', toml: '🧾', xml: '🧾',
    go: '🐹', py: '🐍', js: '📜', jsx: '📜', ts: '📜', tsx: '📜',
    html: '🌐', htm: '🌐', css: '🎨',
    zip: '🗜', tar: '🗜', gz: '🗜', rar: '🗜',
    sh: '⚙️', sql: '🗃',
    mp4: '🎬', webm: '🎬', mov: '🎬', mp3: '🎵', wav: '🎵',
  };
  return byExt[ext] || FILE_TYPE_ICONS[f.file_type || 4] || '📦';
}

/** 字节数格式化成人类可读大小 */
function humanSize(n?: number): string {
  if (!n || n <= 0) return '';
  const units = ['B', 'KB', 'MB', 'GB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`;
}

/** 图片类且有可用 url 时才渲染缩略图 */
function isPreviewableImage(f: FileInfo): boolean {
  return f.file_type === 1 && !!f.url;
}

/** 图片预览用 inline 链接（浏览器内直接显示而非触发下载） */
function inlineURL(url: string): string {
  if (url.includes('inline=1')) return url;
  return `${url}${url.includes('?') ? '&' : '?'}inline=1`;
}

export default function OutputFiles({ files, label, compact }: Props) {
  const t = useT();
  const [preview, setPreview] = useState<FileInfo | null>(null);
  const [opening, setOpening] = useState<string | null>(null);
  // 产物过多时默认折叠：比如 PDF 转图片一次产出 13+ 张，全部平铺会把
  // 对话流撑出好几屏。默认只展示前 COLLAPSE_LIMIT 个，展开是显式动作。
  const overLimit = files.length > COLLAPSE_LIMIT;
  const [expanded, setExpanded] = useState(false);
  const visible = overLimit && !expanded ? files.slice(0, COLLAPSE_LIMIT) : files;

  /** 用系统默认应用在本地打开文件（替代浏览器下载）。 */
  const openFile = async (f: FileInfo) => {
    if (!f.url) return;
    const key = f.path || f.name;
    setOpening(key);
    try {
      await openLocalFile(f.url);
    } catch (e) {
      alert(t('files.openFailed', { msg: (e as Error).message }));
    } finally {
      setOpening(null);
    }
  };

  if (!files || files.length === 0) return null;

  return (
    <div className={`file-list${compact ? ' is-compact' : ''}`}>
      {label && (
        <div className="file-list-label">
          {label}
          <span className="file-list-count">{files.length}</span>
        </div>
      )}

      <div className="file-list-items">
        {visible.map((f, i) => {
          const size = humanSize(f.size);
          const showThumb = isPreviewableImage(f);
          const previewable = previewKindFor(f) !== 'none';
          const open = () => setPreview(f);
          return (
            <div
              key={f.path || `${f.name}-${i}`}
              className={`file-card${previewable ? ' is-clickable' : ''}`}
              title={previewable ? t('files.previewX', { name: f.name }) : f.path || f.name}
              role={previewable ? 'button' : undefined}
              tabIndex={previewable ? 0 : undefined}
              onClick={previewable ? open : undefined}
              onKeyDown={(e) => {
                if (previewable && (e.key === 'Enter' || e.key === ' ')) {
                  e.preventDefault();
                  open();
                }
              }}
            >
              {showThumb ? (
                <span className="file-card-thumb">
                  <img src={inlineURL(f.url!)} alt={f.name} />
                </span>
              ) : (
                <span className="file-card-icon" aria-hidden>
                  {iconFor(f)}
                </span>
              )}

              <div className="file-card-meta">
                <div className="file-card-name">{f.name}</div>
                <div className="file-card-sub">
                  {f.path && <span className="file-card-path">{f.path}</span>}
                  {size && <span className="file-card-size">{size}</span>}
                </div>
              </div>

              {previewable && (
                <button
                  type="button"
                  className="file-card-action"
                  onClick={(e) => {
                    e.stopPropagation();
                    open();
                  }}
                  title={t('files.previewX', { name: f.name })}
                >
                  {t('files.preview')}
                </button>
              )}

              {f.url && (
                <button
                  type="button"
                  className="file-card-action"
                  onClick={(e) => {
                    e.stopPropagation();
                    openFile(f);
                  }}
                  disabled={opening === (f.path || f.name)}
                  title={t('files.openX', { name: f.name })}
                >
                  {opening === (f.path || f.name) ? t('files.opening') : t('files.open')}
                </button>
              )}
            </div>
          );
        })}
      </div>

      {overLimit && (
        <button
          type="button"
          className="file-list-toggle"
          onClick={() => setExpanded((v) => !v)}
        >
          {expanded ? t('files.collapseAll') : t('files.expandAll', { n: files.length })}
        </button>
      )}

      {preview && <FilePreview file={preview} onClose={() => setPreview(null)} />}
    </div>
  );
}

import { useEffect, useState } from 'react';
import type { FileInfo } from '../types/api';
import { FileType } from '../types/api';
import { openLocalFile } from '../services/api';
import Markdown from './Markdown';
import { useT } from '../i18n';

// ============================================================
// 文件预览弹层 —— 点击文件卡片后，在浏览器内直接渲染预览
//
// 本地 agent 场景下文件就躺在后端工作区里，download 端点支持 inline=1
// （Content-Disposition: inline），因此 pdf / 图片 / 视频 / 音频 / 文本
// 都能直接在浏览器内渲染，无需先下载到本地再打开。
// ============================================================

type PreviewKind = 'image' | 'video' | 'audio' | 'pdf' | 'markdown' | 'text' | 'none';

/** 文本类扩展名（与后端 pkg/workspace/artifacts.go 的 textExts 对齐，供前端预览判定） */
const TEXT_EXTS = new Set([
  'txt', 'md', 'markdown', 'json', 'jsonl', 'xml', 'html', 'htm', 'css',
  'csv', 'tsv', 'yaml', 'yml', 'toml', 'ini', 'conf', 'env',
  'go', 'py', 'js', 'jsx', 'ts', 'tsx', 'java', 'c', 'h', 'cpp', 'hpp', 'cc',
  'rs', 'rb', 'php', 'sh', 'bash', 'zsh', 'sql', 'log', 'proto', 'graphql', 'vue', 'svelte',
]);

const IMAGE_EXTS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg', 'bmp', 'ico', 'avif']);
const VIDEO_EXTS = new Set(['mp4', 'webm', 'mov', 'm4v', 'ogv', 'mkv']);
const AUDIO_EXTS = new Set(['mp3', 'wav', 'ogg', 'm4a', 'flac', 'aac']);

function extOf(name: string): string {
  return (name.split('.').pop() || '').toLowerCase();
}

/** 判断该文件能否在浏览器内预览，以及以什么方式预览。 */
export function previewKindFor(f: FileInfo): PreviewKind {
  const ext = extOf(f.name);
  const mime = (f.mime_type || '').toLowerCase();
  const ft = f.file_type;

  if (ft === FileType.IMAGE || mime.startsWith('image/') || IMAGE_EXTS.has(ext)) return 'image';
  if (ft === FileType.VIDEO || mime.startsWith('video/') || VIDEO_EXTS.has(ext)) return 'video';
  if (mime.startsWith('audio/') || AUDIO_EXTS.has(ext)) return 'audio';
  if (mime === 'application/pdf' || ext === 'pdf') return 'pdf';

  const isText =
    ft === FileType.TEXT ||
    mime.startsWith('text/') ||
    mime === 'application/json' ||
    mime === 'application/xml' ||
    mime === 'application/javascript' ||
    TEXT_EXTS.has(ext);
  if (!isText) return 'none';
  return ext === 'md' || ext === 'markdown' ? 'markdown' : 'text';
}

/** inline 链接：让 download 端点返回 Content-Disposition: inline 而非 attachment。 */
function inlineURL(url: string): string {
  if (!url) return url;
  if (url.includes('inline=1')) return url;
  return `${url}${url.includes('?') ? '&' : '?'}inline=1`;
}

/** 文本类预览的内容大小上限（2MB），超出直接提示下载，避免前端渲染卡死。 */
const MAX_TEXT_PREVIEW_BYTES = 2 * 1024 * 1024;

interface Props {
  file: FileInfo;
  onClose: () => void;
}

export default function FilePreview({ file, onClose }: Props) {
  const t = useT();
  const kind = previewKindFor(file);
  const src = inlineURL(file.url || '');
  const isTextKind = kind === 'markdown' || kind === 'text';

  const [text, setText] = useState<string | null>(null);
  const [textState, setTextState] = useState<'loading' | 'ready' | 'error' | 'too-large'>(
    isTextKind ? 'loading' : 'ready',
  );
  const [opening, setOpening] = useState(false);

  /** 用系统默认应用在本地打开文件（替代浏览器下载）。 */
  const openFile = async () => {
    if (!file.url) return;
    setOpening(true);
    try {
      await openLocalFile(file.url);
    } catch (e) {
      alert(t('files.openFailed', { msg: (e as Error).message }));
    } finally {
      setOpening(false);
    }
  };

  // 文本类：拉取内容渲染。用 inline=1 的下载端点即可拿到原始文本。
  useEffect(() => {
    if (!isTextKind) return;
    if (file.size && file.size > MAX_TEXT_PREVIEW_BYTES) {
      setTextState('too-large');
      return;
    }
    if (!src) {
      setTextState('error');
      return;
    }
    let cancelled = false;
    setTextState('loading');
    fetch(src)
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        return res.text();
      })
      .then((txt) => {
        if (cancelled) return;
        setText(txt);
        setTextState('ready');
      })
      .catch(() => {
        if (!cancelled) setTextState('error');
      });
    return () => {
      cancelled = true;
    };
  }, [isTextKind, src, file.size]);

  // ESC 关闭
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  return (
    <div className="file-preview-overlay" onClick={onClose} role="presentation">
      <div
        className="file-preview-dialog"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label={t('files.previewX', { name: file.name })}
      >
        <header className="file-preview-header">
          <div className="file-preview-heading">
            <span className="file-preview-name" title={file.name}>
              {file.name}
            </span>
            {file.path && <span className="file-preview-path">{file.path}</span>}
          </div>
          <div className="file-preview-actions">
            {file.url && (
              <button
                type="button"
                className="file-preview-btn"
                onClick={openFile}
                disabled={opening}
                title={t('files.openX', { name: file.name })}
              >
                {opening ? t('files.opening') : t('files.open')}
              </button>
            )}
            <button
              type="button"
              className="file-preview-close"
              onClick={onClose}
              aria-label={t('files.closePreview')}
              title={t('common.close')}
            >
              ✕
            </button>
          </div>
        </header>

        <div className="file-preview-body">
          {kind === 'image' && <img className="file-preview-img" src={src} alt={file.name} />}
          {kind === 'video' && <video className="file-preview-video" src={src} controls autoPlay />}
          {kind === 'audio' && <audio className="file-preview-audio" src={src} controls autoPlay />}
          {kind === 'pdf' && <iframe className="file-preview-iframe" src={src} title={file.name} />}

          {kind === 'markdown' && textState === 'ready' && (
            <div className="file-preview-markdown">
              <Markdown>{text || ''}</Markdown>
            </div>
          )}
          {kind === 'text' && textState === 'ready' && (
            <pre className="file-preview-pre">{text || ''}</pre>
          )}

          {isTextKind && textState === 'loading' && (
            <div className="file-preview-hint">{t('files.loading')}</div>
          )}
          {isTextKind && textState === 'error' && (
            <div className="file-preview-fallback">{t('files.readFailed')}</div>
          )}
          {isTextKind && textState === 'too-large' && (
            <div className="file-preview-fallback">{t('files.tooLarge')}</div>
          )}

          {kind === 'none' && (
            <div className="file-preview-fallback">
              <div className="file-preview-fallback-icon" aria-hidden>
                📦
              </div>
              <div>{t('files.unsupported')}</div>
              {file.url && (
                <button
                  type="button"
                  className="btn btn-primary"
                  onClick={openFile}
                  disabled={opening}
                  style={{ marginTop: 14 }}
                >
                  {opening ? t('files.opening') : t('files.openLocal')}
                </button>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

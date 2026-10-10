import { createContext, useContext, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { FileInfo } from '../types/api';
import FilePreview from './FilePreview';
import { useT } from '../i18n';

/**
 * 对话工作区上下文：由页面层（ChatPage 等）提供当前对话的 chat_id。
 *
 * Markdown 正文里 LLM 经常输出相对路径文件链接（如 [outputs/report.md](outputs/report.md)），
 * 有了 chat_id 才能把这种链接映射到工作区文件预览（/api/v1/files/download）。
 * 未提供时相对路径链接只拦截导航、不做预览，避免把 SPA 路由带跑（表现为「跳到主页」）。
 */
export const MarkdownWorkspaceContext = createContext<string | undefined>(
  undefined,
);

/**
 * 修复「闭合 ** 紧贴 CJK/全角标点」导致的加粗解析失败。
 *
 * CommonMark 规定加粗闭合分隔符必须是 right-flanking：前一个字符不能是
 * 空白，且（前一个字符不是标点，或后一个字符是空白/标点）。LLM 中文回复
 * 极常输出 `**会议时间：**2026-09-18` 这种写法 —— 闭合 ** 前是全角冒号
 * （Unicode 标点）、后面紧跟数字，两个条件都不满足，无法闭合，渲染成
 * 原样星号。中文没有空格习惯，这类内容几乎必然踩中（GitHub 同样如此）。
 *
 * 修复：把紧贴闭合 ** 的标点移到 ** 外面（`**会议时间：**` →
 * `**会议时间**：`），文本内容不变、显示等价，且能正确加粗。
 * 代码段（``` 与 `）里的 ** 是字面量，先占位保护、处理完再还原。
 */
function normalizeCjkBold(text: string): string {
  const stash: string[] = [];
  let out = text
    .replace(/```[\s\S]*?```/g, (m) => `\u0000${stash.push(m) - 1}\u0000`)
    .replace(/`[^`\n]+`/g, (m) => `\u0000${stash.push(m) - 1}\u0000`);

  out = out.replace(
    /(\*\*[^*\n]+?)((?:[：:，,。．;；!！?？、·～…—）)】》〉」』"”'’.()"\[\]]])+)(\*\*)/g,
    (_m: string, open: string, body: string, punct: string, close: string) =>
      `${open}${body}${close}${punct}`,
  );

  return out.replace(/\u0000(\d+)\u0000/g, (_m: string, i: string) =>
    stash[Number(i)],
  );
}

interface Props {
  children: string;
  /**
   * 紧凑模式：用于 think 等辅助内容。
   * 字号更小、颜色更淡、段落间距压缩。
   */
  compact?: boolean;
  className?: string;
}

/**
 * Markdown 渲染组件。
 *
 * 统一 agent 回复与思考过程的富文本渲染，支持 GFM（表格、删除线、任务列表）。
 *
 * 安全性：react-markdown 默认不渲染 raw HTML（未启用 rehype-raw），
 * 因此 LLM 或工具返回的内容中若含 <script> 等标签只会被当作纯文本，无 XSS 风险。
 *
 * 链接处理分两类：
 *  - 绝对协议链接（http/https/mailto 等）：target=_blank + rel=noreferrer，新 tab 打开。
 *  - 相对路径（如 outputs/report.md）：视为工作区文件链接，拦截默认导航，
 *    在应用内弹 FilePreview 预览（chat_id 缺失时仅拦截，不响应）。
 */
export default function Markdown({ children, compact = false, className }: Props) {
  const t = useT();
  const chatId = useContext(MarkdownWorkspaceContext);
  const [previewFile, setPreviewFile] = useState<FileInfo | null>(null);

  const cls = ['markdown-body', compact ? 'markdown-compact' : '', className]
    .filter(Boolean)
    .join(' ');

  return (
    <div className={cls}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ node, href, children, ...props }) => {
            // 模型经常脑补 sandbox:/、file:// 这类「假协议」前缀（本质是工作区
            // 相对路径前面加了沙箱协议）。剥掉还原成相对路径，否则点击会触发
            // 浏览器协议导航失败 → SPA 路由兜底 → 跳到任务页/主页。
            const normalized = href
              ? href.replace(/^(?:sandbox|mnt):\/+/i, '').replace(/^file:\/+/i, '')
              : href;
            // 协议化链接（http:/https:/mailto:/...）或页内锚点：维持外链行为
            if (!normalized || /^[a-z][a-z0-9+.-]*:/i.test(normalized) || normalized.startsWith('#')) {
              return (
                <a {...props} href={normalized} target="_blank" rel="noreferrer noopener">
                  {children}
                </a>
              );
            }
            // 相对路径：工作区文件链接，点击走应用内预览而非页面导航。
            //
            // 必须先解码：react-markdown 会把链接地址里的非 ASCII 字符百分号编码
            // （「飞书」→「%E9%A3%9E%E4%B9%A6」）。不解码的话下面 encodeURIComponent
            // 会再编码一次（% → %25），后端只解一次，拿到的 path 是字面量
            // 「outputs/%E9%A3%9E...」，文件不存在 → 预览「内容读取失败」。
            // 中文文件名在本项目是常态（LLM 产物大多是中文名），不能漏。
            let decoded = normalized;
            try {
              decoded = decodeURIComponent(normalized);
            } catch {
              // 非法编码（如路径里本身含孤立的 %）保持原样
            }
            const relPath = decoded.replace(/^\.\//, '');
            return (
              <a
                {...props}
                href={relPath}
                title={chatId ? t('files.previewX', { name: relPath }) : relPath}
                onClick={(e) => {
                  e.preventDefault();
                  if (!chatId) return;
                  const name = relPath.split('/').pop() || relPath;
                  setPreviewFile({
                    source: 'local',
                    name,
                    path: relPath,
                    url: `/api/v1/files/download?chat_id=${encodeURIComponent(chatId)}&path=${encodeURIComponent(relPath)}`,
                  });
                }}
              >
                {children}
              </a>
            );
          },
        }}
      >
        {normalizeCjkBold(children)}
      </ReactMarkdown>

      {previewFile && (
        <FilePreview file={previewFile} onClose={() => setPreviewFile(null)} />
      )}
    </div>
  );
}

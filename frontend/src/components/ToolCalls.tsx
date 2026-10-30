import { useState } from 'react';
import type { ToolCallRecord } from '../types/api';
import Markdown from './Markdown';
import { useT } from '../i18n';

interface Props {
  calls: ToolCallRecord[];
}

/** 后端 model.TurnStatus：1-执行中 2-成功 3-失败 */
const STATUS_FAILED = 3;

/** 折叠阈值：超过这个条数只展示前几条，其余收进「展开全部」 */
const COLLAPSED_COUNT = 5;

// 文件类工具 → 操作文案的 i18n key。命中的工具展示为「操作 + 文件名」，
// 替代生硬的 builtin/xxx（read_file 等对用户更有意义）。
const FILE_TOOL_OP: Record<string, string> = {
  read_file: 'readFile',
  write_file: 'writeFile',
  edit_file: 'editFile',
  list_dir: 'listDir',
  create_dir: 'createDir',
  file_exists: 'checkFile',
  delete_path: 'deletePath',
};

// 非文件类内置工具的友好显示名；展开后仍展示真实工具名，便于调试。
const BUILTIN_TOOL_LABEL: Record<string, string> = {
  run_command: 'runCommand',
};

/** 从工具入参 JSON 里提取文件/目录路径（兼容 path / file_path / file 等字段名） */
function extractPath(input: string): string {
  if (!input) return '';
  try {
    const obj = JSON.parse(input);
    const p = obj?.path ?? obj?.file_path ?? obj?.file ?? obj?.dir ?? obj?.filename ?? '';
    if (typeof p === 'string') return p;
  } catch {
    /* 非 JSON 入参：不强求 */
  }
  return '';
}

/** 取路径最后一段作为展示用文件名 */
function basename(p: string): string {
  const s = p.replace(/[\\/]+$/, '');
  const i = Math.max(s.lastIndexOf('/'), s.lastIndexOf('\\'));
  return i >= 0 ? s.slice(i + 1) : s;
}

/** 入参优雅化：较短入参 prettify 成多行 JSON，长入参（如 write_file 的 content）原样保留 */
function formatInput(input: string): string {
  if (!input) return '';
  if (input.length > 2000) return input;
  try {
    return JSON.stringify(JSON.parse(input), null, 2);
  } catch {
    return input;
  }
}

export default function ToolCalls({ calls }: Props) {
  const t = useT();
  const [expanded, setExpanded] = useState<Set<number>>(new Set());
  // 长列表默认只显示前 COLLAPSED_COUNT 条；执行中任务列表会持续增长，
  // 新条目若被折叠会看不到 —— 所以执行中（有状态为「执行中」的调用）不折叠。
  const hasRunning = calls.some((c) => c.status === 1);
  const overflown = calls.length > COLLAPSED_COUNT && !hasRunning;
  const [showAll, setShowAll] = useState(false);
  const visible = overflown && !showAll ? calls.slice(0, COLLAPSED_COUNT) : calls;
  const hiddenCount = calls.length - visible.length;

  const toggle = (i: number) => {
    const next = new Set(expanded);
    if (next.has(i)) next.delete(i);
    else next.add(i);
    setExpanded(next);
  };

  return (
    <div className="tool-calls">
      {visible.map((c, i) => {
        const failed = c.status === STATUS_FAILED;
        const isOpen = expanded.has(i);
        // 优先用模型在工具 schema 的 summary 参数里写的说明；
        // 为空时降级为 server/tool_name 拼接（保持与历史数据兼容）
        const summary = c.summary?.trim();
        const toolName = c.server_name ? `${c.server_name}/${c.tool_name}` : c.tool_name;

        // 文件类工具：折叠态展示「操作 + 文件名」；展开后补全完整路径。
        // tool_name 兼容历史格式（如 fs__read_file / builtin 前缀），按 __ 取末段归一化。
        const bareName = c.tool_name.split('__').pop() || c.tool_name;
        const opKey = FILE_TOOL_OP[bareName];
        const builtinLabelKey = BUILTIN_TOOL_LABEL[bareName];
        const filePath = opKey ? extractPath(c.input) : '';
        const isFileTool = !!opKey && !!filePath;

        let label: string;
        if (isFileTool) {
          label = `${t(`toolCalls.${opKey}`)} ${basename(filePath)}`;
        } else if (builtinLabelKey) {
          const friendlyName = t(`toolCalls.${builtinLabelKey}`);
          label = summary ? `【${friendlyName}】${summary}` : `【${friendlyName}】`;
        } else if (summary) {
          label = `【${toolName}】${summary}`;
        } else {
          label = `【${toolName}】`;
        }
        const usingSummary = !isFileTool && !!summary;

        return (
          <div key={i} className="tool-call-item">
            <div className="tool-name" onClick={() => toggle(i)} style={{ cursor: 'pointer' }}>
              {isOpen ? '▾' : '▸'} {label}
              {failed && <span className="tool-badge failed">{t('status.failed')}</span>}
              {c.time_cost && <span className="tool-cost">{c.time_cost}</span>}
            </div>

            {isOpen && (
              <>
                {/* 文件工具折叠态只显示文件名，展开后补「工具名 + 完整路径」；
                    其它工具沿用 summary 时补工具名，避免展开后不知道是哪个工具 */}
                {isFileTool ? (
                  <div className="tool-meta">
                    {t('toolCalls.tool')}<code>{toolName}</code>
                    <span className="tool-path">{filePath}</span>
                  </div>
                ) : (
                  usingSummary && (
                    <div className="tool-meta">
                      {t('toolCalls.tool')}<code>{toolName}</code>
                    </div>
                  )
                )}
                {c.input && (
                  <div className="tool-args">
                    <div className="tool-detail-label">{t('toolCalls.input')}</div>
                    <pre className="tool-args-pre">{formatInput(c.input)}</pre>
                  </div>
                )}
                {c.output && (
                  // 工具返回常含 markdown（文件内容、页面快照等），按 markdown 渲染
                  <div className={`tool-result${failed ? ' failed' : ''}`}>
                    <div className="tool-detail-label">{t('toolCalls.output')}</div>
                    <Markdown compact>{c.output}</Markdown>
                  </div>
                )}
              </>
            )}
          </div>
        );
      })}

      {overflown && (
        <button className="tool-calls-toggle" onClick={() => setShowAll(!showAll)}>
          {showAll
            ? t('toolCalls.collapseAll')
            : t('toolCalls.expandAll', { n: calls.length, hidden: hiddenCount })}
        </button>
      )}
    </div>
  );
}

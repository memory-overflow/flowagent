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
        const fallback = c.server_name ? `${c.server_name}/${c.tool_name}` : c.tool_name;
        const label = summary || fallback;
        const usingSummary = !!summary;

        return (
          <div key={i} className="tool-call-item">
            <div className="tool-name" onClick={() => toggle(i)} style={{ cursor: 'pointer' }}>
              {isOpen ? '▾' : '▸'} {label}
              {failed && <span className="tool-badge failed">{t('status.failed')}</span>}
              {c.time_cost && <span className="tool-cost">{c.time_cost}</span>}
            </div>

            {isOpen && (
              <>
                {usingSummary && (
                  // 展示 summary 时把 server/tool_name 拼接到折叠态作为补充元数据，
                  // 否则点击展开后用户看不到「这是哪个工具」
                  <div className="tool-meta">
                    {t('toolCalls.tool')}<code>{fallback}</code>
                  </div>
                )}
                {c.input && (
                  <div className="tool-args">
                    {t('toolCalls.args')}<code>{c.input}</code>
                  </div>
                )}
                {c.output && (
                  // 工具返回常含 markdown（文件内容、页面快照等），按 markdown 渲染
                  <div className={`tool-result${failed ? ' failed' : ''}`}>
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

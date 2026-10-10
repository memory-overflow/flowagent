import { useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import type { ConversationWorkflow, WorkflowNode } from '../types/api';
import { TaskStatus } from '../types/api';
import Markdown from './Markdown';
import OutputFiles from './OutputFiles';
import { useT } from '../i18n';

// ============================================================
// 工作流结果分栏（对话区右侧可拖拽/收起的嵌入式面板）
//
// 区别于传统的 fixed 弹出抽屉：本组件按「分栏」形式嵌入到
// ChatPage 的 flex 行布局里，宽度由父组件控制，边界带拖拽条。
//
// 「最终结果」= 叶子节点（没有任何下游依赖的节点）的 output。
// 这些节点承载整段工作流的最终交付物（文章全文、生成的成品说明等），
// 而非中间调研节点的网页抓取全文。
// ============================================================

// 状态 → 展示文案 / 样式 class（与 WorkflowBanner 口径一致）
const STATUS_META: Record<number, { labelKey: string; className: string }> = {
  1: { labelKey: 'pending', className: 'pending' },
  2: { labelKey: 'running', className: 'running' },
  3: { labelKey: 'completed', className: 'completed' },
  4: { labelKey: 'failed', className: 'failed' },
  5: { labelKey: 'cancelled', className: 'cancelled' },
  6: { labelKey: 'partialFailed', className: 'failed' },
};

/** 提取叶子节点：没有任何节点依赖它的节点（即整条 DAG 的终点）。 */
function leafNodes(nodes: WorkflowNode[]): WorkflowNode[] {
  const hasDownstream = new Set<string>();
  for (const n of nodes) {
    for (const d of n.depends_on || []) hasDownstream.add(d);
  }
  const leaves = nodes.filter((n) => !hasDownstream.has(n.node_id));
  // 空 DAG 或单节点自环等退化场景：回退到全部节点，避免结果区一片空白
  return leaves.length > 0 ? leaves : nodes;
}

/** 叶子节点里真正有产出的（已完成且带文本或文件）。 */
function finalNodes(nodes: WorkflowNode[]): WorkflowNode[] {
  return leafNodes(nodes).filter(
    (n) =>
      n.status === TaskStatus.COMPLETED &&
      (!!n.output?.reply_text?.trim() || (n.output?.output_files?.length ?? 0) > 0),
  );
}

interface Props {
  workflow: ConversationWorkflow;
  /** 当前是否展开（false 时只显示一条极窄的折叠条） */
  expanded: boolean;
  /** 当前面板宽度（像素）。仅在 expanded 时生效 */
  width: number;
  /** 切换展开/折叠 */
  onToggleExpanded: () => void;
  chatId: string;
}

export default function WorkflowResultPanel({
  workflow,
  expanded,
  width,
  onToggleExpanded,
  chatId,
}: Props) {
  const navigate = useNavigate();
  const t = useT();
  const nodes = workflow.nodes || [];
  const done = nodes.filter((n) => n.status === TaskStatus.COMPLETED).length;
  const finished = workflow.task_status === TaskStatus.COMPLETED;
  const meta = STATUS_META[workflow.task_status] || STATUS_META[1];

  const finals = useMemo(() => finalNodes(nodes), [nodes]);

  // ---- 折叠态：只露一根细长条 + 中心一个圆形按钮 ----
  // 用户可以一眼知道右侧还有折叠内容，不至于把它完全忘掉。
  if (!expanded) {
    return (
      <aside
        className="workflow-result-rail is-collapsed"
        aria-label={t('workflow.resultAriaCollapsed')}
      >
        <button
          type="button"
          className="workflow-result-expand-btn"
          onClick={onToggleExpanded}
          title={t('workflow.expandResult')}
        >
          <span className="workflow-result-expand-icon">📦</span>
          <span className="workflow-result-expand-label">{t('workflow.result')}</span>
        </button>
      </aside>
    );
  }

  // ---- 展开态：正常的分栏内容 ----
  return (
    <aside
      className="workflow-result-rail"
      style={{ width }}
      aria-label={t('workflow.resultTitle')}
    >
      <header className="workflow-result-header">
        <div className="workflow-result-title">
          <span className="workflow-result-icon" aria-hidden>📦</span>
          <span>{t('workflow.resultTitle')}</span>
          <span className={`status-dot ${meta.className}`}>{t(`status.${meta.labelKey}`)}</span>
        </div>
        <button
          type="button"
          className="workflow-result-collapse-btn"
          onClick={onToggleExpanded}
          title={t('workflow.collapseResult')}
          aria-label={t('workflow.collapseResult')}
        >
          ▸
        </button>
      </header>

      <div className="workflow-result-body">
        {/* 规划摘要：给结果一个上下文，说明这段工作流在做什么 */}
        {workflow.plan_summary && (
          <div className="workflow-result-summary">📋 {workflow.plan_summary}</div>
        )}

        {/* 工作流级产物清单：各子任务 output_files 的并集。
            置于正文之前 —— 长文已落盘时，文件才是真正的交付物，
            叶子节点的 reply_text 只剩摘要。 */}
        {workflow.output_files && workflow.output_files.length > 0 && (
          <OutputFiles files={workflow.output_files} label={t('workflow.deliverables')} />
        )}

        {!finished ? (
          <div className="workflow-result-empty">
            <div className="workflow-result-empty-title">{t('workflow.notFinishedTitle')}</div>
            <div className="workflow-result-empty-desc">
              {t('workflow.notFinishedDesc', { done, total: nodes.length })}
            </div>
          </div>
        ) : finals.length === 0 ? (
          <div className="workflow-result-empty">
            <div className="workflow-result-empty-title">{t('workflow.finishedTitle')}</div>
            <div className="workflow-result-empty-desc">{t('workflow.finishedDesc')}</div>
          </div>
        ) : (
          finals.map((n) => (
            <section key={n.node_id} className="workflow-result-item">
              <div className="workflow-result-item-title">
                <span className="workflow-result-item-name">{n.name || n.node_id}</span>
                <span className="workflow-result-item-id">#{n.node_id}</span>
              </div>

              {n.output?.reply_text && (
                <div className="workflow-result-item-output">
                  <Markdown>{n.output.reply_text}</Markdown>
                </div>
              )}

              {n.output?.output_files?.length ? (
                <OutputFiles files={n.output.output_files} />
              ) : null}
            </section>
          ))
        )}
      </div>

      <footer className="workflow-result-footer">
        <button
          type="button"
          className="btn btn-ghost btn-sm"
          onClick={() => navigate(`/chat/${chatId}/workflow`)}
        >
          🕸️ {t('workflow.viewFullDag')}
        </button>
      </footer>
    </aside>
  );
}

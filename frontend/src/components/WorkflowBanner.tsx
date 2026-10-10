import { useNavigate } from 'react-router-dom';
import type { ConversationWorkflow } from '../types/api';
import { TaskStatus } from '../types/api';
import { useT } from '../i18n';

// ============================================================
// 对话页工作流状态横幅
//
// 工作流 DAG 已移到独立页面（/chat/:chatId/workflow），但对话页仍需要
// 让用户一眼看到「这段对话有一个全局工作流，以及它现在进行到哪一步」。
// 本组件是紧凑状态条（非完整 DAG）：状态徽标 + 节点完成进度 + 规划摘要，
// 整条可点击跳转详情页。
//
// 状态徽标不再复用只有 8px 圆点的 .status-dot，而是每个状态一个 SVG
// 图标 + 各自动画（排队呼吸 / 执行中旋转 / 完成弹入 / 失败抖动），
// 与对话任务的状态展示在视觉强度上对齐、但信息更丰富。
// ============================================================

// 每个状态一个 SVG 图标（stroke 风格，跟随 currentColor 着色）
function StatusIcon({ status }: { status: number }) {
  switch (status) {
    case 2: // 执行中：3/4 圆弧，配合旋转动画形成 spinner
      return (
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.6" strokeLinecap="round">
          <path d="M12 3a9 9 0 1 1-9 9" />
        </svg>
      );
    case 3: // 已完成：对勾
      return (
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.8" strokeLinecap="round" strokeLinejoin="round">
          <polyline points="4 12 9 17 20 6" />
        </svg>
      );
    case 4: // 失败：叉
      return (
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.8" strokeLinecap="round">
          <line x1="6" y1="6" x2="18" y2="18" />
          <line x1="18" y1="6" x2="6" y2="18" />
        </svg>
      );
    case 5: // 已终止：停止方块
      return (
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
          <rect x="6" y="6" width="12" height="12" rx="2" />
        </svg>
      );
    case 6: // 部分失败：警告三角
      return (
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M12 4 21 20 H3 Z" />
          <line x1="12" y1="10" x2="12" y2="14" />
          <line x1="12" y1="16.6" x2="12" y2="16.6" />
        </svg>
      );
    case 1: // 排队中：时钟
    default:
      return (
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
          <circle cx="12" cy="12" r="9" />
          <polyline points="12 7 12 12 15.5 14" />
        </svg>
      );
  }
}

interface StatusMeta {
  /** 状态文案的词典 key（status.* 命名空间） */
  labelKey: string;
  /** 挂在 banner 上的状态 class，驱动左侧色条与边框 */
  className: string;
  /** 挂到 .wf-status-badge 上的 class，驱动图标颜色与动画 */
  badgeClass: string;
}

const STATUS_META: Record<number, StatusMeta> = {
  1: { labelKey: 'pending', className: 'pending', badgeClass: 'pending' },
  2: { labelKey: 'running', className: 'running', badgeClass: 'running' },
  3: { labelKey: 'completed', className: 'completed', badgeClass: 'completed' },
  4: { labelKey: 'failed', className: 'failed', badgeClass: 'failed' },
  5: { labelKey: 'cancelled', className: 'cancelled', badgeClass: 'cancelled' },
  6: { labelKey: 'partialFailed', className: 'failed', badgeClass: 'failed' },
};

export default function WorkflowBanner({
  workflow,
  chatId,
  onRetry,
  onStop,
}: {
  workflow: ConversationWorkflow;
  chatId: string;
  /** 失败/终止态时的重试回调，传入后失败态会显示「重试」按钮 */
  onRetry?: () => void;
  /** 排队/执行中时的停止回调，传入后运行态会显示「停止」按钮 */
  onStop?: () => void;
}) {
  const navigate = useNavigate();
  const t = useT();
  const allNodes = workflow.nodes || [];
  // 已归档节点（失败分支的残留）仍在 DAG 里展示，但不参与执行 ——
  // 进度分母、完成数一律只看未归档节点，否则「修复一半的失败工作流」
  // 进度永远卡在某个小数上，看着像卡死了。
  const nodes = allNodes.filter((n) => !n.archived);
  const archived = allNodes.length - nodes.length;
  const total = nodes.length;
  const done = nodes.filter((n) => n.status === TaskStatus.COMPLETED).length;
  const running = nodes.filter((n) => n.status === TaskStatus.RUNNING).length;
  const meta = STATUS_META[workflow.task_status] || STATUS_META[1];
  // 优先按节点状态算进度；节点为空时退回后端给的 progress 字段
  const pct = total > 0 ? Math.round((done / total) * 100) : workflow.progress ?? 0;
  const retryable =
    workflow.task_status === TaskStatus.FAILED ||
    workflow.task_status === TaskStatus.PARTIAL_FAILED ||
    workflow.task_status === TaskStatus.CANCELLED;
  // 排队(1)/执行中(2)时可停止
  const stoppable =
    workflow.task_status === TaskStatus.PENDING ||
    workflow.task_status === TaskStatus.RUNNING;

  return (
    <div
      className={`workflow-banner ${meta.className}`}
      onClick={() => navigate(`/chat/${chatId}/workflow`)}
      role="button"
      tabIndex={0}
      title={t('workflow.bannerTip')}
    >
      <span className="workflow-banner-accent" />

      <span className="workflow-banner-main">
        <span className="workflow-banner-icon" aria-hidden>🕸️</span>
        <span
          className="workflow-banner-title"
          title={workflow.title || workflow.plan_summary || t('workflow.title')}
        >
          {workflow.title || t('workflow.title')}
        </span>
        <span className={`wf-status-badge ${meta.badgeClass}`}>
          <span className="wf-status-icon" aria-hidden>
            <StatusIcon status={workflow.task_status} />
          </span>
          {t(`status.${meta.labelKey}`)}
        </span>

        <span className="workflow-banner-progress">
          {t('workflow.stepsDone', { done, total })}
          {running > 0 && (
            <span className="workflow-banner-running-count">
              {' '}· {t('workflow.stepsRunning', { n: running })}
            </span>
          )}
          {archived > 0 && (
            <span
              className="workflow-banner-archived"
              title={t('workflow.archivedTipBanner', { n: archived })}
            >
              {' '}· ⌀{archived} {t('status.abandoned')}
            </span>
          )}
        </span>

        {workflow.task_id && (
          <span
            className="workflow-banner-taskid"
            title={t('workflow.taskIdValue', { id: workflow.task_id })}
          >
            #{workflow.task_id.slice(0, 8)}
          </span>
        )}

        <span className="workflow-banner-action">
          {t('workflow.viewDetail')}
          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
            <polyline points="9 18 15 12 9 6" />
          </svg>
        </span>

        {stoppable && onStop && (
          <button
            type="button"
            className="workflow-banner-stop"
            onClick={(e) => { e.stopPropagation(); onStop(); }}
          >
            {t('common.stop')}
          </button>
        )}

        {retryable && onRetry && (
          <button
            type="button"
            className="workflow-banner-retry"
            onClick={(e) => { e.stopPropagation(); onRetry(); }}
          >
            {t('common.retry')}
          </button>
        )}
      </span>

      {workflow.plan_summary && (
        <span className="workflow-banner-summary">{workflow.plan_summary}</span>
      )}

      {total > 0 && (
        <span className="workflow-banner-bar">
          <span className="workflow-banner-bar-fill" style={{ width: `${pct}%` }} />
        </span>
      )}
    </div>
  );
}

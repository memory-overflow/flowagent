import { useState } from 'react';
import { setActiveWorkflow } from '../services/api';
import type { WorkflowListItem } from '../types/api';
import { TaskStatus } from '../types/api';
import { useT, type Translate } from '../i18n';

// ============================================================
// 工作流切换器
//
// v1.4 起一个对话可以绑定多条工作流：每轮规划由模型决定「续接已有工作流」
// 还是「新开工作流」，于是同一个对话里会积累好几段 DAG。本组件是它们之间
// 的切换入口 —— 横向 pill 列表，点一下就切当前查看的那条。
//
// 切换是纯指针操作（只改 conversation.workflow_task_id），不改 DAG、
// 不影响正在执行的调度，所以切错了也没有代价，随时能切回来。
//
// 只有一条工作流时不渲染：那时没有「切换」这件事，信息由
// WorkflowBanner 承担，多一条横条只是噪音。
// ============================================================

/** 工作流状态 → 状态点 class（与后端 TaskStatus 对齐） */
const STATUS_CLASS: Record<number, string> = {
  [TaskStatus.PENDING]: 'pending',
  [TaskStatus.RUNNING]: 'running',
  [TaskStatus.COMPLETED]: 'completed',
  [TaskStatus.FAILED]: 'failed',
  [TaskStatus.CANCELLED]: 'cancelled',
  [TaskStatus.PARTIAL_FAILED]: 'failed',
};

export default function WorkflowSwitcher({
  chatId,
  list,
  onSwitched,
}: {
  chatId: string;
  /** 该对话下的全部工作流（按创建时间升序） */
  list: WorkflowListItem[];
  /** 切换成功后回调，通常是重新拉对话详情 */
  onSwitched?: () => void;
}) {
  // 正在切换的目标 ID —— 只用于给对应 pill 一个「切换中」的视觉反馈，
  // 不做乐观更新：切换要重新拉完整 DAG，本地造不出那份数据。
  const t = useT();
  const [switching, setSwitching] = useState<string | null>(null);

  if (!list || list.length < 2) return null;

  const handleSwitch = async (taskId: string) => {
    if (taskId === switching) return;
    setSwitching(taskId);
    try {
      await setActiveWorkflow(chatId, taskId);
      onSwitched?.();
    } catch (e: any) {
      alert(t('workflow.switchFailed', { msg: e.message }));
    } finally {
      setSwitching(null);
    }
  };

  return (
    <div className="wf-switcher">
      <span
        className="wf-switcher-label"
        title={t('workflow.totalWorkflows', { n: list.length })}
      >
        🕸️ {t('workflow.workflowCount', { n: list.length })}
      </span>

      <div className="wf-switcher-tabs">
        {list.map((wf) => (
          <button
            key={wf.task_id}
            type="button"
            className={`wf-switcher-tab${wf.active ? ' is-active' : ''}`}
            onClick={() => handleSwitch(wf.task_id)}
            disabled={switching !== null}
            title={buildTooltip(t, wf)}
          >
            <span className="wf-switcher-idx">{wf.index}</span>
            <span className="wf-switcher-name">{wf.title}</span>
            <span className={`status-dot ${STATUS_CLASS[wf.task_status] || 'pending'}`}>
              {switching === wf.task_id ? t('workflow.switching') : wf.status_name}
            </span>
            {/* 已归档节点是失败分支的残留：留在 DAG 里灰显，但不参与执行。
                这里给个角标，让用户不用点进去就知道这条工作流踩过坑。 */}
            {wf.node_archived > 0 && (
              <span
                className="wf-switcher-archived"
                title={t('workflow.archivedTipSwitcher', { n: wf.node_archived })}
              >
                ⌀{wf.node_archived}
              </span>
            )}
          </button>
        ))}
      </div>
    </div>
  );
}

/** pill 的 tooltip：状态 + 节点构成 + 能否续接，一眼看清这条工作流的价值 */
function buildTooltip(t: Translate, wf: WorkflowListItem): string {
  const parts = [
    t('workflow.tipWorkflow', { index: wf.index, title: wf.title }),
    t('workflow.tipStatus', { status: wf.status_name }),
    t('workflow.tipNodes', { done: wf.node_done, total: wf.node_active }) +
      (wf.node_archived > 0 ? t('workflow.tipArchived', { n: wf.node_archived }) : ''),
  ];
  if (wf.can_append) {
    parts.push(t('workflow.tipAppendable', { n: wf.reusable_count }));
  } else {
    parts.push(
      t('workflow.tipNotAppendable', {
        reason: wf.append_reason || t('workflow.noReusableNodes'),
      }),
    );
  }
  if (wf.plan_summary) parts.push(wf.plan_summary);
  return parts.join('\n');
}

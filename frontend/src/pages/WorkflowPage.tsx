import { useState, useEffect, useCallback, useRef, type MouseEvent as ReactMouseEvent } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import {
  getConversationDetail,
  getTaskDetail,
  retryTask,
  retryWorkflowNode,
  stopTask,
  subscribeTaskStream,
} from '../services/api';
import type { ConversationWorkflow, WorkflowListItem, WorkflowNode } from '../types/api';
import { TaskStatus } from '../types/api';
import WorkflowGraph, { WorkflowNodeDetail } from '../components/WorkflowGraph';
import WorkflowSwitcher from '../components/WorkflowSwitcher';
import { MarkdownWorkspaceContext } from '../components/Markdown';
import { fmtTokens } from '../components/ContextUsage';
import DurationText, { useDurationTooltip } from '../components/DurationText';
import { useT } from '../i18n';

// 工作流状态轮询间隔（毫秒），与对话页保持一致。
const POLL_INTERVAL_MS = 5000;

/**
 * 工作流独立页面：与对话页平行，用于完整展示对话绑定的全局工作流 DAG。
 * 画布可缩放/平移、节点可拖拽，点击节点在右侧抽屉查看详情。
 */
export default function WorkflowPage() {
  const { chatId } = useParams<{ chatId: string }>();
  const navigate = useNavigate();
  const t = useT();
  const durationTip = useDurationTooltip();
  const [workflow, setWorkflow] = useState<ConversationWorkflow | null>(null);
  // 该对话下的全部工作流。一个对话可有多条，本页展示其中「当前激活」的那条，
  // 用切换器换一条看 —— 切换只改指针，不影响任何正在执行的调度。
  const [workflowList, setWorkflowList] = useState<WorkflowListItem[]>([]);
  const [chatTitle, setChatTitle] = useState('');
  const [loading, setLoading] = useState(true);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [retrying, setRetrying] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [retryingNode, setRetryingNode] = useState(false);

  // 运行中子任务的实时思考过程：node_id -> 步骤列表。
  // 与 node.output.think_process（终态落库）互补 —— 执行中 node.output 尚未写，
  // 这里靠订阅子任务 SSE，在 turn_done/task_done 时回拉 getTaskDetail 实时重建。
  const [liveThink, setLiveThink] = useState<Record<string, string[]>>({});
  // 每个子任务的 SSE 关闭函数，key = 子任务 task_id，避免重复订阅。
  const sseClosersRef = useRef<Record<string, () => void>>({});

  // 详情面板（结果预览）宽度：null = 用 CSS 默认 2/3，拖拽后为像素值
  const [detailWidth, setDetailWidth] = useState<number | null>(null);
  const [resizing, setResizing] = useState(false);
  const resizeState = useRef<{ startX: number; startWidth: number } | null>(null);
  const mainRef = useRef<HTMLDivElement>(null);

  const copyTaskId = async () => {
    if (!workflow?.task_id) return;
    try {
      await navigator.clipboard.writeText(workflow.task_id);
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    } catch {
      /* 剪贴板不可用就忽略，不打断交互 */
    }
  };

  const handleRetry = async () => {
    if (!workflow?.task_id || retrying) return;
    setRetrying(true);
    try {
      await retryTask(workflow.task_id);
      // 重试后工作流回到排队中，立即刷新并进入轮询
      await fetchWorkflow();
    } catch (e: any) {
      alert(t('workflow.retryFailed', { msg: e?.message || t('common.unknown') }));
    } finally {
      setRetrying(false);
    }
  };

  const handleStop = async () => {
    if (!workflow?.task_id || stopping) return;
    setStopping(true);
    try {
      await stopTask(workflow.task_id);
      // 停止后工作流进入终止态，立即刷新让画布节点状态与横幅同步
      await fetchWorkflow();
    } catch (e: any) {
      alert(t('workflow.stopFailed', { msg: e?.message || t('common.unknown') }));
    } finally {
      setStopping(false);
    }
  };

  // 单节点重试：只重试指定失败节点及其下游，保留已完成上游
  const handleRetryNode = async (nodeId: string) => {
    if (!workflow?.task_id || retryingNode) return;
    setRetryingNode(true);
    try {
      await retryWorkflowNode(workflow.task_id, nodeId);
      await fetchWorkflow();
    } catch (e: any) {
      alert(t('workflow.retryNodeFailed', { msg: e?.message || t('common.unknown') }));
    } finally {
      setRetryingNode(false);
    }
  };

  const onResizeStart = (e: ReactMouseEvent<HTMLDivElement>) => {
    e.preventDefault();
    // resizer 紧邻的 nextElementSibling 即详情面板
    const panel = e.currentTarget.nextElementSibling as HTMLElement | null;
    if (!panel) return;
    resizeState.current = {
      startX: e.clientX,
      startWidth: panel.getBoundingClientRect().width,
    };
    setResizing(true);
  };

  // 拖拽期间在 window 上监听 mousemove/mouseup，避免鼠标移出面板后中断
  useEffect(() => {
    if (!resizing) return;
    const onMove = (e: MouseEvent) => {
      const st = resizeState.current;
      const main = mainRef.current;
      if (!st || !main) return;
      const containerW = main.getBoundingClientRect().width;
      // 左边界向左拖 → 面板变宽；面板最小 360，画布至少留 326px（320 + resizer 6）
      const next = st.startWidth + (st.startX - e.clientX);
      setDetailWidth(Math.min(Math.max(next, 360), containerW - 326));
    };
    const onUp = () => {
      setResizing(false);
      resizeState.current = null;
    };
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
    return () => {
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
    };
  }, [resizing]);

  const fetchWorkflow = useCallback(async () => {
    if (!chatId) return;
    try {
      const res = await getConversationDetail(chatId);
      const info = res.data.data.chat_info;
      setChatTitle(info.title || t('workflow.untitledChat'));
      setWorkflow(info.workflow || null);
      setWorkflowList(info.workflow_list || []);
    } catch (e: any) {
      if (e.message?.includes('404') || e.message?.includes('not found')) {
        navigate('/');
      }
    } finally {
      setLoading(false);
    }
  }, [chatId, navigate, t]);

  useEffect(() => { fetchWorkflow(); }, [fetchWorkflow]);

  // 拉取某个运行中子任务的实时思考过程。
  const fetchLiveThink = useCallback(async (nodeId: string, taskId: string) => {
    try {
      const res = await getTaskDetail(taskId);
      const steps = res.data.data.agent_output?.think_process;
      if (steps && steps.length > 0) {
        setLiveThink((prev) => ({ ...prev, [nodeId]: steps }));
      }
    } catch {
      /* 子任务尚未就绪或已清理，忽略，交给下次刷新 */
    }
  }, []);

  // 工作流处于排队(1)或执行中(2)时周期性刷新，让画布节点状态与整体进度实时更新。
  const workflowActive =
    workflow?.task_status === 1 || workflow?.task_status === 2;
  useEffect(() => {
    if (!workflowActive) return;
    const interval = setInterval(() => {
      fetchWorkflow();
    }, POLL_INTERVAL_MS);
    return () => clearInterval(interval);
  }, [workflowActive, fetchWorkflow]);

  const nodes: WorkflowNode[] = workflow?.nodes || [];

  // 订阅运行中/排队中子任务的 SSE：turn_done 回拉实时思考过程，task_done 收尾关闭。
  // 复用与对话页 ChatPage 相同的 subscribeTaskStream 约定（task_done 后自动 close），
  // 子任务成功/失败都会发 task_done，故失败节点同样能拿到已发生的思考轨迹。
  useEffect(() => {
    for (const node of nodes) {
      const taskId = node.task_id;
      if (!taskId) continue;
      if (node.status !== TaskStatus.PENDING && node.status !== TaskStatus.RUNNING) continue;
      if (sseClosersRef.current[taskId]) continue; // 已订阅，跳过
      const close = subscribeTaskStream(taskId, {
        onTurnDone: () => {
          fetchLiveThink(node.node_id, taskId);
        },
        onTaskDone: () => {
          fetchLiveThink(node.node_id, taskId);
          const c = sseClosersRef.current[taskId];
          delete sseClosersRef.current[taskId];
          c?.();
        },
      });
      sseClosersRef.current[taskId] = close;
    }
  }, [nodes, fetchLiveThink]);

  // 卸载时关闭所有子任务 SSE 连接
  useEffect(() => {
    return () => {
      Object.values(sseClosersRef.current).forEach((c) => c());
      sseClosersRef.current = {};
    };
  }, []);

  const selected = selectedId ? nodes.find((n) => n.node_id === selectedId) || null : null;
  // 整条工作流的 token 合计（规划期调用 + 各节点子树），后端随 DAG 一起返回
  const wfTokens = workflow?.token_usage;
  // 失败 / 部分失败 / 已终止 时允许重试
  const retryable =
    !!workflow &&
    (workflow.task_status === TaskStatus.FAILED ||
      workflow.task_status === TaskStatus.PARTIAL_FAILED ||
      workflow.task_status === TaskStatus.CANCELLED);
  // 排队 / 执行中 时允许停止
  const stoppable =
    !!workflow &&
    (workflow.task_status === TaskStatus.PENDING ||
      workflow.task_status === TaskStatus.RUNNING);

  return (
    /* 节点输出里的相对路径文件链接需要 chat_id 才能映射到工作区预览 */
    <MarkdownWorkspaceContext.Provider value={chatId}>
    <div className="workflow-page">
      <header className="workflow-page-header">
        <div className="workflow-page-header-left">
          <button
            className="btn btn-ghost btn-sm"
            onClick={() => navigate(`/chat/${chatId}`)}
            title={t('workflow.backToChat')}
          >
            ← {t('workflow.backToChat')}
          </button>
          <span className="workflow-page-title">{t('workflow.title')}</span>
          {workflow?.title && (
            <span className="workflow-page-wf-title" title={workflow.plan_summary || workflow.title}>
              {workflow.title}
            </span>
          )}
          {chatTitle && <span className="workflow-page-chat">{chatTitle}</span>}
          {workflow?.duration && (
            <span
              className="workflow-page-tokens workflow-page-duration"
              title={durationTip(t('workflow.durationWhole'), workflow.duration)}
            >
              ⏱{' '}
              <DurationText
                duration={workflow.duration}
                title={durationTip(t('workflow.durationWhole'), workflow.duration)}
              />
            </span>
          )}
          {wfTokens && (
            <span
              className="workflow-page-tokens"
              title={t('workflow.tokenTip', {
                input: fmtTokens(wfTokens.prompt_tokens),
                output: fmtTokens(wfTokens.completion_tokens),
              })}
            >
              {t('workflow.tokenTotal', { n: fmtTokens(wfTokens.total_tokens) })}
            </span>
          )}
          {workflow?.task_id && (
            <span className="workflow-page-taskid" title={t('workflow.taskIdTip')}>
              <span className="workflow-page-taskid-label">{t('workflow.taskIdLabel')}</span>
              <code className="workflow-page-taskid-code">{workflow.task_id}</code>
              <button
                type="button"
                className="workflow-page-taskid-copy"
                onClick={copyTaskId}
              >
                {copied ? t('common.copied') : t('common.copy')}
              </button>
              <a
                className="workflow-page-taskid-debug"
                href={`/debug/${workflow.task_id}`}
                target="_blank"
                rel="noreferrer"
                title={t('workflow.openDebugTip')}
                onClick={(e) => e.stopPropagation()}
              >
                🐞
              </a>
            </span>
          )}
        </div>
        <div className="workflow-page-header-right">
          {stoppable && (
            <button
              className="btn btn-warning btn-sm"
              onClick={handleStop}
              disabled={stopping}
              title={t('workflow.stopTip')}
            >
              {stopping ? t('workflow.stopping') : t('common.stop')}
            </button>
          )}
          {retryable && (
            <button
              className="btn btn-primary btn-sm"
              onClick={handleRetry}
              disabled={retrying}
              title={t('workflow.retryTip')}
            >
              {retrying ? t('workflow.retrying') : t('common.retry')}
            </button>
          )}
          <button className="btn btn-ghost btn-sm" onClick={fetchWorkflow}>
            {t('common.refresh')}
          </button>
        </div>
      </header>

      {/* 一个对话可有多条工作流：这里在它们之间切换查看。
          组件内部在只有一条时直接返回 null，不占位置。 */}
      {workflowList.length > 1 && (
        <div className="workflow-page-switcher-row">
          <WorkflowSwitcher
            chatId={chatId!}
            list={workflowList}
            onSwitched={() => {
              // 切到另一条 DAG 后原先选中的节点多半已经不存在，
              // 清掉选中态，避免右侧抽屉留着一个空面板
              setSelectedId(null);
              fetchWorkflow();
            }}
          />
        </div>
      )}

      {loading ? (
        <div className="empty-state">
          <div className="empty-desc">{t('common.loading')}</div>
        </div>
      ) : !workflow ? (
        <div className="empty-state">
          <div className="empty-icon">🕸️</div>
          <div className="empty-title">{t('workflow.emptyTitle')}</div>
          <div className="empty-desc">{t('workflow.emptyDesc')}</div>
        </div>
      ) : (
        <div className="workflow-page-body">
          {workflow.plan_summary && (
            <div className="workflow-plan-banner">📋 {workflow.plan_summary}</div>
          )}

          <div
            className={`workflow-page-main${selected ? ' has-detail' : ''}`}
            ref={mainRef}
          >
            <div className="workflow-canvas">
              <WorkflowGraph workflow={workflow} onSelect={setSelectedId} />
            </div>

            {selected && (
              <>
                <div
                  className={`workflow-detail-resizer${resizing ? ' active' : ''}`}
                  onMouseDown={onResizeStart}
                  title={t('workflow.resizeTip')}
                />
                <aside
                  className="workflow-detail-panel"
                  style={
                    detailWidth != null
                      ? { width: detailWidth, flexBasis: detailWidth }
                      : undefined
                  }
                >
                  <WorkflowNodeDetail
                    key={selected.node_id}
                    node={selected}
                    nodes={nodes}
                    liveThink={liveThink}
                    workflowTaskId={workflow?.task_id}
                    onRetryNode={handleRetryNode}
                    onClose={() => setSelectedId(null)}
                  />
                </aside>
              </>
            )}
          </div>
        </div>
      )}
    </div>
    </MarkdownWorkspaceContext.Provider>
  );
}

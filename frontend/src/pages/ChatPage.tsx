import { useState, useEffect, useRef, useCallback, useMemo } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTickingElapsed, formatElapsedMs } from '../hooks/useTickingElapsed';
import {
  getConversationDetail,
  sendMessage,
  uploadFile,
  getTaskDetail,
  retryTask,
  stopTask,
  addConversationMCP,
  removeConversationMCP,
  listConversationMCPServers,
  listMCPServers,
  addConversationSkill,
  removeConversationSkill,
  listConversationSkills,
  listSkills,
  listModels,
  updateConversationModel,
  updateConversationTitle,
  openWorkspace,
  setActiveWorkflow,
  subscribeTaskStream,
} from '../services/api';
import type {
  ConversationDetail,
  ConversationInfo,
  FileInfo,
  TaskDetail,
  AgentOutput,
  ThinkLayers,
  ThinkStep,
  MCPServerConfig,
  SkillConfig,
  SkillRef,
  ModelConfig,
} from '../types/api';
import { TaskStatus } from '../types/api';
import OutputFiles from '../components/OutputFiles';
import ToolCalls from '../components/ToolCalls';
import Markdown, { MarkdownWorkspaceContext } from '../components/Markdown';
import WorkflowBanner from '../components/WorkflowBanner';
import WorkflowResultPanel from '../components/WorkflowResultPanel';
import WorkflowSwitcher from '../components/WorkflowSwitcher';
import { ContextUsage } from '../components/ContextUsage';
import InputMentionMenu, { type MentionItem } from '../components/InputMentionMenu';
import { useT } from '../i18n';
import { displayTitle, isPlaceholderTitle } from '../utils/conversationTitle';
import { applyMention, detectMention, filterMentionItems, type MentionQuery } from '../utils/mention';

// 前端任务状态轮询间隔（毫秒）。
// 有 SSE 流式通道做实时增量，轮询只作兜底，故放宽到 5s 降低请求频率。
const POLL_INTERVAL_MS = 5000;

/** 正在被调用的工具（思考过程实时状态用） */
interface CallingTool {
  turnId: number;
  toolName: string;
}

/** 正在压缩上下文（思考过程实时状态用） */
interface CompactingState {
  /** 触发压缩时的输入 token 总量 */
  tokens: number;
  /** 当前模型的上下文上限 */
  limit: number;
  /**
   * 分块压缩的进度，如「第 3/7 段」。
   * 素材一次装不下时按段压缩，段数可达几十，没有进度用户会以为卡死。
   */
  stage?: string;
}

export default function ChatPage() {
  const { chatId } = useParams<{ chatId: string }>();
  const navigate = useNavigate();
  const t = useT();
  const [detail, setDetail] = useState<ConversationDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [sending, setSending] = useState(false);
  const [inputText, setInputText] = useState('');
  const [pollingTasks, setPollingTasks] = useState<Set<string>>(new Set());
  // 流式文本缓冲：task_id -> 已收到的 delta 增量拼成的实时文本
  const [streamText, setStreamText] = useState<Record<string, string>>({});
  const streamTextRef = useRef<Record<string, string>>({});
  // 各任务最近一次收到 delta 的 turn_id：跨 turn 换行分隔用。task_id -> turn_id。
  const lastTurnIdRef = useRef<Record<string, number>>({});
  // 正在调用的工具：task_id -> { turnId, toolName }。由 tool_calling 事件写入、
  // turn_done / task_done 清除，用于在思考过程中实时显示「正在调用 XXX 工具…」。
  const [callingTools, setCallingTools] = useState<Record<string, CallingTool>>({});
  const callingToolsRef = useRef<Record<string, CallingTool>>({});
  // 正在压缩上下文：task_id -> { tokens, limit }。由 compacting 事件的
  // phase=start 写入、phase=done / task_done 清除。压缩会阻塞本轮 LLM 调用
  // 数秒到数十秒，没有提示的话前端看起来就是卡住了。
  const [compacting, setCompacting] = useState<Record<string, CompactingState>>({});
  const compactingRef = useRef<Record<string, CompactingState>>({});
  const sseClosersRef = useRef<Record<string, () => void>>({});
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const messagesRef = useRef<HTMLDivElement>(null);
  // 用户是否处于消息区底部。只有贴底时才自动跟随新内容滚动，
  // 用户向上翻阅历史时不被强制拉回底部。
  const isAtBottomRef = useRef(true);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  // 工作流结果分栏：默认展开、宽度默认 40vw（clamp 在 320–800 之间）。
  // 之所以独立出 collapsed/expanded 与 width 两个状态：宽度是连续量，
  // 「是否展开」是离散量，混在一起做状态切换时容易出竞态。
  const [panelExpanded, setPanelExpanded] = useState(true);
  const [panelWidth, setPanelWidth] = useState(() => {
    // 默认取视口宽度的 40%，落进 [320, 800] 区间。SSR 安全：width 默认 0。
    const vw = typeof window !== 'undefined' ? window.innerWidth : 1280;
    return Math.max(320, Math.min(800, Math.round(vw * 0.4)));
  });
  // 上一次观察到的工作流状态，用于检测「执行中 → 完成」的转变，从而自动展开结果分栏
  const prevWorkflowStatusRef = useRef<number | null>(null);
  // 是否正在拖拽分割条 —— 拖拽过程中关闭 CSS transition、屏蔽鼠标选择
  const splitterDraggingRef = useRef(false);

  // ---- 附件上传 ----
  // pendingFiles 已上传成功、等待随下一条消息发出的附件（后端返回的 FileInfo）。
  // 之所以「先上传、再发消息」而不是随消息一起 multipart 提交：
  //   - 上传后立刻能拿到工作区路径与预览，用户发送前就能确认附件对不对
  //   - sendMessage 的契约保持纯 JSON，无需为附件改动对话协议
  const [pendingFiles, setPendingFiles] = useState<FileInfo[]>([]);
  const [uploading, setUploading] = useState(false);
  const [dragOver, setDragOver] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  // 「新开工作流」开关：一个对话可以有多条工作流，规划期默认由模型决定
  // 续接还是新开，这个开关是给用户的显式入口。只作用于下一条消息，
  // 发送后自动复位 —— 否则用户忘记关掉，后续每轮都会强开新工作流。
  const [forceNewWorkflow, setForceNewWorkflow] = useState(false);

  // ---- 输入框 @ / # 引用 ----
  // mention 是「光标前正在输入的引用片段」，为空表示没有在输入引用。
  // 同时保留一份 ref：插入时要拿到「按下回车那一刻」的片段位置，
  // 只靠 state 会读到 setState 之前的旧值。
  const [mention, setMention] = useState<MentionQuery | null>(null);
  const [mentionIndex, setMentionIndex] = useState(0);
  const mentionRef = useRef<MentionQuery | null>(null);
  // 用户按 Esc 主动关掉了引用菜单。仅在正文再次变化（onChange）时解除，
  // 否则光标一动就又弹回来，等于 Esc 无效。
  const mentionDismissedRef = useRef(false);

  // ---- 技能：对话启用的 + 全部可选的 ----
  // 状态提到 ChatPage 而不是留在 SkillToolbar：输入框的「#」菜单与
  // 上方的技能条是同一份数据，各拉一份既多一次请求，也会出现
  // 「菜单里启用了、技能条上没出现」的不一致。
  const [skills, setSkills] = useState<SkillConfig[]>([]);
  const [attachedSkills, setAttachedSkills] = useState<SkillRef[]>([]);
  const [skillsLoading, setSkillsLoading] = useState(true);

  const chatInfo: ConversationInfo | null = detail?.chat_info || null;

  const fetchDetail = useCallback(async () => {
    if (!chatId) return;
    try {
      const res = await getConversationDetail(chatId);
      setDetail(res.data.data);
      // Find pending/running tasks to poll
      const tasks = res.data.data.tasks || [];
      const activeTasks = tasks.filter(
        (t) => t.task_status === 1 || t.task_status === 2
      );
      if (activeTasks.length > 0) {
        setPollingTasks(new Set(activeTasks.map((t) => t.task_id)));
      }
    } catch (e: any) {
      if (e.message?.includes('404') || e.message?.includes('not found')) {
        navigate('/');
      }
    } finally {
      setLoading(false);
    }
  }, [chatId, navigate]);

  useEffect(() => { fetchDetail(); }, [fetchDetail]);

  // 拉取「全部技能」与「本对话已启用的技能」。
  // 两个请求并发：技能条与 # 菜单都要用，串行会白白多等一个 RTT。
  const reloadSkills = useCallback(async () => {
    if (!chatId) return;
    try {
      const [skillRes, convRes] = await Promise.all([
        listSkills(),
        listConversationSkills(chatId),
      ]);
      setSkills(skillRes.data.data?.list || []);
      setAttachedSkills(convRes.data.data?.items || []);
    } catch (e) {
      console.error(e);
    } finally {
      setSkillsLoading(false);
    }
  }, [chatId]);

  useEffect(() => { reloadSkills(); }, [reloadSkills]);

  // 单个任务即时刷新：SSE 收到 turn_done 时立即回拉该任务详情，
  // 让 think_process 跟随每个已结束的 turn 实时推进，而不是干等 5s 轮询兜底。
  const refreshTask = useCallback(async (taskId: string) => {
    try {
      const res = await getTaskDetail(taskId);
      const t = res.data.data;
      setDetail((prev) =>
        prev
          ? { ...prev, tasks: prev.tasks.map((x) => (x.task_id === taskId ? t : x)) }
          : prev
      );
    } catch {
      // SSE 是尽力而为通道，回拉失败交给轮询兜底
    }
  }, []);

  // 重试失败的工作流：调后端重置 DAG 并重新排队，再整段刷新进入轮询。
  const handleRetryWorkflow = useCallback(async () => {
    const wfTaskId = chatInfo?.workflow?.task_id;
    if (!wfTaskId) return;
    try {
      await retryTask(wfTaskId);
      await fetchDetail();
    } catch (e: any) {
      alert(t('chat.retryFailed', { msg: e?.message || t('chat.unknownError') }));
    }
  }, [chatInfo?.workflow?.task_id, fetchDetail, t]);

  // 重试失败的对话任务：清空旧 turn 后从头重新生成，再刷新进入轮询。
  const handleRetryConversation = useCallback(async (taskId: string) => {
    try {
      await retryTask(taskId);
      // 清掉该任务遗留的流式文本缓冲，避免重试后与新回复串行
      delete streamTextRef.current[taskId];
      delete lastTurnIdRef.current[taskId];
      setStreamText({ ...streamTextRef.current });
      await fetchDetail();
    } catch (e: any) {
      alert(t('chat.retryFailed', { msg: e?.message || t('chat.unknownError') }));
    }
  }, [fetchDetail, t]);

  // 停止执行中的工作流：终止所有未完成子任务并收口工作流。
  const handleStopWorkflow = useCallback(async () => {
    const wfTaskId = chatInfo?.workflow?.task_id;
    if (!wfTaskId) return;
    try {
      await stopTask(wfTaskId);
      await fetchDetail();
    } catch (e: any) {
      alert(t('chat.stopFailed', { msg: e?.message || t('chat.unknownError') }));
    }
  }, [chatInfo?.workflow?.task_id, fetchDetail, t]);

  // 重新生成某一轮：以该轮完全相同的输入（正文 + 附件）发起新一轮任务。
  //
  // 为什么不复用「重试」：后端 /tasks/retry 只放行失败/终止态的任务，已完成
  // 的轮次会被直接拒绝（见 handler.RetryTask 的状态校验）。而「重新生成」的
  // 语义是「再问一遍」—— 走 /conversations/chat 新建一条任务，原轮记录与产物
  // 完整保留，新旧两轮可以对照着看。
  const handleRegenerate = useCallback(
    async (text: string, files: FileInfo[]) => {
      if (!chatId) return;
      try {
        const res = await sendMessage(chatId, text, files || [], false);
        const taskId = res.data.data.task_id;
        setPollingTasks((prev) => new Set([...prev, taskId]));
        // 新一轮追加在末尾，跳到底部让用户立刻看到它开始跑
        isAtBottomRef.current = true;
        await fetchDetail();
        messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' });
      } catch (e: any) {
        alert(t('chat.regenerateFailed', { msg: e?.message || t('chat.unknownError') }));
      }
    },
    [chatId, fetchDetail, t],
  );

  // 停止执行中的对话任务：标记为终止，清掉遗留的流式缓冲。
  const handleStopConversation = useCallback(async (taskId: string) => {
    try {
      await stopTask(taskId);
      delete streamTextRef.current[taskId];
      delete lastTurnIdRef.current[taskId];
      setStreamText({ ...streamTextRef.current });
      await fetchDetail();
    } catch (e: any) {
      alert(t('chat.stopFailed', { msg: e?.message || t('chat.unknownError') }));
    }
  }, [fetchDetail, t]);

  // Poll active tasks
  //
  // 轮询不只判断任务是否结束，还要把最新的 task 内容写回列表 ——
  // 后端在执行中就会实时返回已产生的 think_process，
  // 不回写的话用户在任务跑完前看不到任何进展。
  useEffect(() => {
    if (pollingTasks.size === 0) return;
    const interval = setInterval(async () => {
      const stillActive = new Set(pollingTasks);
      const updated: Record<string, TaskDetail> = {};

      for (const taskId of pollingTasks) {
        try {
          const res = await getTaskDetail(taskId);
          const t = res.data.data;
          updated[taskId] = t;
          if (t.task_status !== 1 && t.task_status !== 2) {
            stillActive.delete(taskId);
          }
        } catch {
          stillActive.delete(taskId);
        }
      }

      // 用最新数据替换 detail.tasks 里对应的 task，保持顺序不变
      if (Object.keys(updated).length > 0) {
        setDetail((prev) =>
          prev
            ? { ...prev, tasks: prev.tasks.map((t) => updated[t.task_id] ?? t) }
            : prev
        );
      }

      setPollingTasks(stillActive);
      if (stillActive.size < pollingTasks.size) {
        fetchDetail(); // refresh full list when some tasks complete
      }
    }, POLL_INTERVAL_MS);
    return () => clearInterval(interval);
  }, [pollingTasks, fetchDetail]);

  // 工作流状态轮询：工作流处于排队(1)或执行中(2)时，周期性刷新整段对话，
  // 让横幅的状态徽标与节点进度实时更新。
  //
  // 注意：工作流任务（task_type=workflow）被 ListTasksByChatID 过滤，不在
  // detail.tasks 里，上面的对话任务轮询覆盖不到它，必须单独判断
  // chat_info.workflow.task_status 是否活跃。
  const workflowActive =
    chatInfo?.workflow?.task_status === 1 || chatInfo?.workflow?.task_status === 2;
  useEffect(() => {
    if (!workflowActive) return;
    const interval = setInterval(() => {
      fetchDetail();
    }, POLL_INTERVAL_MS);
    return () => clearInterval(interval);
  }, [workflowActive, fetchDetail]);

  // 对话层「活跃→全停」跳变（顶层工作流与所有对话任务都不再 running/queued）
  // 通知 Sidebar 重拉对话列表，刷新 has_running_task / has_failed_task —— 否则
  // Sidebar 永远停留在任务启动那一瞬的快照上，即便后端任务早已收口。
  // 只在翻转时派发一次，避免 fetchDetail 周期触发的 setPollingTasks 也算"停"。
  const conversationActive = workflowActive || pollingTasks.size > 0;
  const prevConversationActiveRef = useRef(false);
  useEffect(() => {
    if (prevConversationActiveRef.current && !conversationActive) {
      window.dispatchEvent(new Event('conversation-list-updated'));
    }
    prevConversationActiveRef.current = conversationActive;
  }, [conversationActive]);

  // 工作流从「排队/执行中」转为「完成」时，自动展开右侧结果分栏。
  // 初次进入页面时若工作流已经完成（prev=null），不自动展开，
  // 避免每次刷新都强行盖住对话 —— 用户想看时可点头部「结果」按钮。
  const workflowStatus = chatInfo?.workflow?.task_status;
  useEffect(() => {
    if (workflowStatus == null) return;
    const prev = prevWorkflowStatusRef.current;
    if (prev != null && prev !== TaskStatus.COMPLETED && workflowStatus === TaskStatus.COMPLETED) {
      setPanelExpanded(true);
    }
    prevWorkflowStatusRef.current = workflowStatus;
  }, [workflowStatus]);

  // 拖拽右侧分栏的边界条 —— 实时改写 panelWidth。
  // 用 mousemove 直接写到 ref，避免每个 move 都触发 React 重渲染。
  // mouseup 时再 setState 一次性提交最终值。
  const handleSplitterMouseDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    splitterDraggingRef.current = true;
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';

    const startX = e.clientX;
    const startWidth = panelWidth;
    const minW = 280;
    const maxW = Math.max(minW + 40, Math.round(window.innerWidth * 0.7));

    let nextWidth = startWidth;

    const onMove = (ev: MouseEvent) => {
      // 拖动方向：splitter 是结果分栏的「左边界」，右边界贴死视口右缘。
      // 向右拖 splitter → 分栏变窄 → width 应减小，故取「起点 - 当前」，
      // 让 splitter 始终跟随鼠标，避免出现鼠标右移、边界左移的反向感。
      nextWidth = Math.max(minW, Math.min(maxW, startWidth + (startX - ev.clientX)));
      // 直接写 inline style，绕过 React 渲染节流，让拖拽跟手
      const rail = document.querySelector<HTMLElement>('.workflow-result-rail:not(.is-collapsed)');
      if (rail) rail.style.width = nextWidth + 'px';
    };
    const onUp = () => {
      splitterDraggingRef.current = false;
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
      setPanelWidth(nextWidth);
    };
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  }, [panelWidth]);

  // 订阅 active task 的 SSE 流：delta 事件逐字拼进 streamText 实时渲染；
  // task_done 后 api.ts 会主动 es.close()，避免浏览器原生 ES 的 ≈3s 自动重连
  // 与服务端 task_done + close 之间形成死循环（参见 api.ts 注释）。
  // 轮询仍作最终兜底：SSE 是「尽力而为」通道，断线时 5s 轮询会补齐。
  useEffect(() => {
    for (const taskId of pollingTasks) {
      if (sseClosersRef.current[taskId]) continue; // 已订阅，跳过
      const close = subscribeTaskStream(taskId, {
        onDelta: (text, ev) => {
          // 跨 turn 换行分隔：多个 turn 的文本（规划期 / 各轮叙述 / 最终答案）
          // 若直接拼在一起会连成一大坨，换 turn 时补一个空行把它们区分开。
          const prevTurn = lastTurnIdRef.current[taskId];
          const cur = streamTextRef.current[taskId] || '';
          let separator = '';
          if (ev.turn_id != null && prevTurn != null && prevTurn !== ev.turn_id && cur) {
            separator = '\n\n';
          }
          if (ev.turn_id != null) lastTurnIdRef.current[taskId] = ev.turn_id;
          streamTextRef.current[taskId] = cur + separator + text;
          setStreamText({ ...streamTextRef.current });
        },
        onToolCalling: (ev) => {
          // 工具调用开始：记录「正在调用 XXX 工具」，思考过程区域实时显示。
          const toolName = String(ev.data?.tool_name ?? '').trim();
          if (!toolName) return;
          callingToolsRef.current[taskId] = { turnId: ev.turn_id ?? 0, toolName };
          setCallingTools({ ...callingToolsRef.current });
        },
        onCompacting: (ev) => {
          // 上下文压缩开始/结束。压缩发生在 LLM 调用之前、turn 创建之前，
          // 所以它不属于任何 turn，只按 taskId 记账。
          const phase = ev.data?.phase;
          if (phase === 'start') {
            compactingRef.current[taskId] = {
              tokens: Number(ev.data?.tokens ?? 0),
              limit: Number(ev.data?.limit ?? 0),
            };
          } else if (phase === 'progress') {
            // 分块压缩：素材一次装不下时按段压，段数可达几十，
            // 报进度避免用户以为卡死。进度只更新 stage，保留原有水位。
            const cur = compactingRef.current[taskId];
            if (cur) compactingRef.current[taskId] = { ...cur, stage: String(ev.data?.stage ?? '') };
          } else {
            delete compactingRef.current[taskId];
          }
          setCompacting({ ...compactingRef.current });
        },
        onTurnDone: (ev) => {
          // 工具调用的 turn 结束（turn_id 匹配）即清除「正在调用」提示；
          // 若 turn_id 缺失则兜底清除该任务的提示。随后回拉详情让思考过程推进。
          const cur = callingToolsRef.current[taskId];
          if (cur && (ev.turn_id == null || cur.turnId === 0 || ev.turn_id === cur.turnId)) {
            delete callingToolsRef.current[taskId];
            setCallingTools({ ...callingToolsRef.current });
          }
          refreshTask(taskId);
        },
        onTaskDone: () => {
          // 清理缓冲 + 全量刷新拉最终 reply_text。close 由 api.ts 内部完成。
          delete streamTextRef.current[taskId];
          delete lastTurnIdRef.current[taskId];
          setStreamText({ ...streamTextRef.current });
          delete callingToolsRef.current[taskId];
          setCallingTools({ ...callingToolsRef.current });
          delete compactingRef.current[taskId];
          setCompacting({ ...compactingRef.current });
          delete sseClosersRef.current[taskId];
          fetchDetail();
        },
      });
      sseClosersRef.current[taskId] = close;
    }
  }, [pollingTasks, fetchDetail, refreshTask]);

  // 组件卸载时关闭所有 SSE 连接
  useEffect(() => {
    const closers = sseClosersRef.current;
    return () => {
      Object.values(closers).forEach((c) => c());
    };
  }, []);

  // 监听消息区滚动，实时记录用户是否贴底（阈值 80px 容忍轻微浮动）
  const handleMessagesScroll = useCallback(() => {
    const el = messagesRef.current;
    if (!el) return;
    const dist = el.scrollHeight - el.scrollTop - el.clientHeight;
    isAtBottomRef.current = dist < 80;
  }, []);

  // 自动跟随滚动：仅当用户本就停在底部时才滚到最新内容。
  // streamText 是 SSE 逐字增量、detail.tasks 是轮询/快照更新，两者都会撑高列表。
  useEffect(() => {
    if (isAtBottomRef.current) {
      messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' });
    }
  }, [detail?.tasks, streamText]);

  // 自动撑高 textarea，最多到 maxRows 行
  const autoResize = useCallback(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = 'auto';
    const lineHeight = 22; // 与 css 中 textarea 行高保持一致
    const maxHeight = lineHeight * 10 + 24; // 约 10 行 + padding
    el.style.height = Math.min(el.scrollHeight, maxHeight) + 'px';
  }, []);

  useEffect(() => { autoResize(); }, [inputText, autoResize]);

  // 上传一批文件到对话工作区，成功的追加到 pendingFiles。
  // 逐个上传而非并发：后端要在 inputs/ 里做重名检测，
  // 并发上传同名文件会竞态命中同一个 uniqueFileName 结果。
  const handleUploadFiles = useCallback(async (files: File[]) => {
    if (!chatId || files.length === 0) return;
    setUploading(true);
    const failed: string[] = [];
    for (const file of files) {
      try {
        const res = await uploadFile(chatId, file);
        const info = res.data.data;
        if (info) setPendingFiles((prev) => [...prev, info]);
      } catch (e: any) {
        failed.push(
          t('chat.uploadEntry', { name: file.name, msg: e?.message || t('chat.unknownError') }),
        );
      }
    }
    setUploading(false);
    if (failed.length > 0) {
      alert(t('chat.uploadPartialFailed', { list: failed.join('\n') }));
    }
  }, [chatId, t]);

  const handleFilePick = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(e.target.files || []);
    handleUploadFiles(files);
    // 清空 value，否则连续选同一个文件不触发 change
    e.target.value = '';
  }, [handleUploadFiles]);

  // 粘贴上传：截图场景的主要入口（Cmd+Shift+4 后直接 Cmd+V）
  const handlePaste = useCallback((e: React.ClipboardEvent) => {
    const items = Array.from(e.clipboardData?.items || []);
    const files = items
      .filter((it) => it.kind === 'file')
      .map((it) => it.getAsFile())
      .filter((f): f is File => !!f);
    if (files.length === 0) return;
    // 有文件就阻止默认粘贴，避免图片被当成文本插进 textarea
    e.preventDefault();
    handleUploadFiles(files);
  }, [handleUploadFiles]);

  const handleDrop = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    setDragOver(false);
    const files = Array.from(e.dataTransfer?.files || []);
    if (files.length > 0) handleUploadFiles(files);
  }, [handleUploadFiles]);

  const removePendingFile = useCallback((path?: string, idx?: number) => {
    // 仅从待发送列表移除，不删工作区文件 ——
    // 文件已落盘，保留它无害，且用户可能只是想改这一轮不带附件
    setPendingFiles((prev) => prev.filter((f, i) => (path ? f.path !== path : i !== idx)));
  }, []);

  // ---- 输入框 @ 引用：本对话可引用的文件 ----
  //
  // 数据来源是对话自己的任务记录，而不是扫工作区目录：
  //   - 用户上传的附件：各轮 task_input.input_files
  //   - Agent 产出的交付物：各轮 agent_output.output_files，
  //     以及工作流各节点的 output.output_files（节点产物不属于任何对话轮次）
  // 这样列出来的都是「这个对话里真实出现过、用户认得」的文件，
  // 不会把工作区的临时文件（.scratch/ 里的调试脚本）混进来。
  // 只收有 path 的文件：没有工作区相对路径就没有可写入正文的引用字面量。
  const conversationFiles = useMemo(() => {
    const map = new Map<string, { path: string; name: string; fileType?: number; origin: 'upload' | 'output' }>();
    const add = (f: FileInfo, origin: 'upload' | 'output') => {
      if (!f?.path || map.has(f.path)) return;
      map.set(f.path, { path: f.path, name: f.name || f.path, fileType: f.file_type, origin });
    };
    for (const task of detail?.tasks || []) {
      for (const f of task.task_input?.input_files || []) add(f, 'upload');
      for (const f of task.agent_output?.output_files || []) add(f, 'output');
    }
    for (const node of chatInfo?.workflow?.nodes || []) {
      for (const f of node.output?.output_files || []) add(f, 'output');
    }
    // 刚上传、还没随消息发出的附件也要能引用 —— 用户刚拖进来就想 @ 它
    for (const f of pendingFiles) add(f, 'upload');
    return [...map.values()];
  }, [detail, chatInfo, pendingFiles]);

  // 本对话已启用的技能 id 集合，# 菜单据此区分「已启用」与「选中即启用」
  const attachedSkillIds = useMemo(
    () => new Set(attachedSkills.map((s) => s.skill_id)),
    [attachedSkills],
  );

  // 选中未启用的技能时顺手挂到本对话上。
  // 只是把 #技能名 写进正文是不够的 —— 技能要真正生效必须挂到对话上，
  // 否则模型那一侧根本看不到这个技能。挂完刷新技能条，两处状态保持一致。
  const enableSkillFromMention = useCallback(async (skill: SkillConfig) => {
    if (!chatId || !skill.id) return;
    try {
      await addConversationSkill(chatId, [skill.id]);
      await reloadSkills();
      window.dispatchEvent(new Event('conversation-list-updated'));
    } catch (e: any) {
      alert(t('chat.addFailed', { msg: e?.message || t('chat.unknownError') }));
    }
  }, [chatId, reloadSkills, t]);

  // # 菜单里的本地指令：都是「就地生效、不进正文」的动作，
  // 选中即执行，输入框里不留字面量（insert 为空串）。
  const mentionCommands = useMemo<MentionItem[]>(() => {
    const commands: MentionItem[] = [];
    const runningTaskId = (detail?.tasks || []).find(
      (x) => x.task_status === TaskStatus.PENDING || x.task_status === TaskStatus.RUNNING,
    )?.task_id;
    if (runningTaskId) {
      commands.push({
        id: 'cmd:stop',
        group: t('chat.mentionGroupCommands'),
        icon: '⏹',
        label: t('chat.mentionCmdStop'),
        desc: t('chat.mentionCmdStopDesc'),
        insert: '',
        onPick: () => { handleStopConversation(runningTaskId); },
      });
    }
    // 对话还没有任何工作流时不显示这个开关 —— 那时没有「续接」可言
    if ((chatInfo?.workflow_list?.length ?? 0) > 0) {
      commands.push({
        id: 'cmd:new-workflow',
        group: t('chat.mentionGroupCommands'),
        icon: '🆕',
        label: t('chat.mentionCmdNewWf', {
          state: forceNewWorkflow ? t('chat.mentionCmdNewWfOn') : t('chat.mentionCmdNewWfOff'),
        }),
        desc: t('chat.mentionCmdNewWfDesc'),
        insert: '',
        onPick: () => { setForceNewWorkflow((v) => !v); },
      });
    }
    commands.push({
      id: 'cmd:clear',
      group: t('chat.mentionGroupCommands'),
      icon: '🧹',
      label: t('chat.mentionCmdClear'),
      desc: t('chat.mentionCmdClearDesc'),
      insert: '',
      onPick: () => { setInputText(''); },
    });
    return commands;
  }, [detail, chatInfo, forceNewWorkflow, handleStopConversation, t]);

  // 当前触发符对应的候选列表（已按输入的片段过滤）
  const mentionItems = useMemo<MentionItem[]>(() => {
    if (!mention) return [];
    if (mention.trigger === '@') {
      const items: MentionItem[] = conversationFiles.map((f) => ({
        id: `file:${f.path}`,
        group: t('chat.mentionGroupFiles'),
        icon: f.fileType === 1 ? '🖼' : f.fileType === 3 ? '📄' : '📦',
        label: f.name,
        desc: f.path,
        tag: f.origin === 'upload' ? t('chat.mentionTagUpload') : t('chat.mentionTagOutput'),
        insert: f.path,
      }));
      // 名称与路径都参与匹配：用户可能记得文件名，也可能只记得目录
      return filterMentionItems(items, mention.query, (it) => `${it.label} ${it.desc ?? ''}`);
    }
    const skillItems: MentionItem[] = skills.map((s) => {
      const enabled = !!s.id && attachedSkillIds.has(s.id);
      // 展示名按界面语言折算（英文界面显示英文名，与技能页一致）；
      // 没有译文时 display 回落原文，因此这里永远拿得到值
      const displayName = s.name_display || s.name;
      return {
        id: `skill:${s.id ?? s.name}`,
        group: t('chat.mentionGroupSkills'),
        icon: '🧩',
        label: displayName,
        desc: s.description_display || s.description,
        tag: enabled ? t('chat.mentionTagEnabled') : t('chat.mentionTagEnable'),
        // 插入与展示保持一致：用户看到哪个名字，正文里就是哪个名字。
        // 技能真正生效靠的是「挂到对话上」（onPick 里做），正文里的 #名字
        // 只是给模型的一个提示 —— 名字跟界面语言走更自然。
        insert: displayName,
        onPick: enabled ? undefined : () => { enableSkillFromMention(s); },
      };
    });
    return filterMentionItems(
      [...skillItems, ...mentionCommands],
      mention.query,
      (it) => `${it.label} ${it.desc ?? ''}`,
    );
  }, [mention, conversationFiles, skills, attachedSkillIds, mentionCommands, enableSkillFromMention, t]);

  // 输入框内容/光标变化时重新判定是否正在输入引用片段
  const syncMention = useCallback((text: string, caret: number | null | undefined) => {
    if (mentionDismissedRef.current) return;
    const next = detectMention(text, caret);
    const prev = mentionRef.current;
    // 同一段引用里继续敲字（片段起点与触发符都没变）不重置高亮，
    // 否则每敲一个字高亮都跳回第一项
    const sameSpan = !!prev && !!next && prev.start === next.start && prev.trigger === next.trigger;
    mentionRef.current = next;
    setMention(next);
    if (!sameSpan) setMentionIndex(0);
  }, []);

  const closeMention = useCallback(() => {
    mentionRef.current = null;
    setMention(null);
  }, []);

  // 选中候选项：把「触发符 + 输入片段」整体替换成选中项的字面量
  const pickMention = useCallback((item: MentionItem) => {
    const q = mentionRef.current;
    if (!q) return;
    // 以 textarea 的当前值为准：state 在极快输入下可能落后一帧
    const current = textareaRef.current?.value ?? inputText;
    const next = applyMention(current, q, item.insert);
    setInputText(next.text);
    closeMention();
    item.onPick?.();
    // 插入后把光标挪到字面量之后（用 rAF 等 React 提交完 DOM 再设），
    // 否则光标会停在末尾，用户想接着写还得手动移位
    requestAnimationFrame(() => {
      const el = textareaRef.current;
      if (!el) return;
      el.focus();
      const caret = Math.min(next.caret, el.value.length);
      el.setSelectionRange(caret, caret);
    });
  }, [inputText, closeMention]);

  const handleSend = async () => {
    if (!chatId || sending) return;
    // 允许「只发附件不写字」—— 附件本身就是意图（如"看看这张图"）
    if (!inputText.trim() && pendingFiles.length === 0) return;
    setSending(true);
    try {
      const res = await sendMessage(chatId, inputText.trim(), pendingFiles, forceNewWorkflow);
      const taskId = res.data.data.task_id;
      setPollingTasks((prev) => new Set([...prev, taskId]));
      // 首次输入时后端会把默认标题「新对话」替换为输入的前若干字符，
      // 派发事件让 Sidebar 刷新列表，使标题即时更新。
      window.dispatchEvent(new Event('conversation-list-updated'));
      setInputText('');
      setPendingFiles([]);
      closeMention();
      // 一次性开关：发出后即复位，避免影响后续轮次
      setForceNewWorkflow(false);
      // 发送后强制回到底部并恢复自动跟随，让用户立即看到新任务与后续回复
      isAtBottomRef.current = true;
      messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' });
      // Refresh immediately to show new task
      await fetchDetail();
    } catch (e: any) {
      alert(t('chat.sendFailed', { msg: e.message }));
    } finally {
      setSending(false);
    }
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    // 引用菜单打开时优先接管导航键与确认键。
    // 不接管的话，Enter 会把半截「@outp」当消息直接发出去。
    if (mention) {
      if (e.key === 'Escape') {
        e.preventDefault();
        // 打上「已关闭」标记：Esc 只改了光标不改正文，不禁用的话
        // 紧接着的 keyup / click 会立刻把菜单重新弹出来
        mentionDismissedRef.current = true;
        closeMention();
        return;
      }
      if (mentionItems.length > 0) {
        if (e.key === 'ArrowDown') {
          e.preventDefault();
          setMentionIndex((i) => (i + 1) % mentionItems.length);
          return;
        }
        if (e.key === 'ArrowUp') {
          e.preventDefault();
          setMentionIndex((i) => (i - 1 + mentionItems.length) % mentionItems.length);
          return;
        }
        if (e.key === 'Enter' || e.key === 'Tab') {
          e.preventDefault();
          pickMention(mentionItems[mentionIndex]);
          return;
        }
      }
    }
    if (e.key === 'Enter' && !e.shiftKey && !e.metaKey && !e.ctrlKey) {
      // 纯 Enter 发送；Shift+Enter / Cmd+Enter 走换行
      e.preventDefault();
      handleSend();
    }
  };

  if (loading) {
    return (
      <div className="chat-view">
        <div className="empty-state">
          <div className="empty-desc">{t('common.loading')}</div>
        </div>
      </div>
    );
  }

  if (!chatInfo) {
    return (
      <div className="chat-view">
        <div className="empty-state">
          <div className="empty-title">{t('chat.notFound')}</div>
        </div>
      </div>
    );
  }

  const tasks = detail?.tasks || [];

  return (
    /* 消息正文里的相对路径文件链接（outputs/xxx.md）需要 chat_id 才能映射到工作区预览 */
    <MarkdownWorkspaceContext.Provider value={chatId}>
    <div className="chat-view is-split">
      {/* 主对话区 —— 占满除了右侧分栏之外的所有空间。
          min-width: 0 让内部 max-width 800px 等不再撑爆 flex 子项 */}
      <div className="chat-main">
        <ChatHeader
          chatInfo={chatInfo}
          onRefresh={fetchDetail}
          panelExpanded={panelExpanded}
          onTogglePanel={() => setPanelExpanded((v) => !v)}
        />

        {/* 工作流切换器：一个对话可以积累多条工作流，这里在它们之间切换查看。
            只有一条时组件内部直接返回 null，避免多一条没有意义的横条。 */}
        {chatInfo.workflow_list && chatInfo.workflow_list.length > 1 && (
          <WorkflowSwitcher
            chatId={chatInfo.chat_id}
            list={chatInfo.workflow_list}
            onSwitched={fetchDetail}
          />
        )}

        {/* 当前激活工作流的紧凑状态条。置顶于消息区上方，
            只展示整体状态（不渲染完整 DAG），点击进入独立工作流页。 */}
        {chatInfo.workflow && (
          <WorkflowBanner
            workflow={chatInfo.workflow}
            chatId={chatInfo.chat_id}
            onRetry={handleRetryWorkflow}
            onStop={handleStopWorkflow}
          />
        )}

        <div
          className="messages-container"
          ref={messagesRef}
          onScroll={handleMessagesScroll}
        >
          {tasks.length === 0 && (
            <div className="empty-state">
              <div className="empty-icon">💬</div>
              <div className="empty-title">{t('chat.emptyTitle')}</div>
              <div className="empty-desc">{t('chat.emptyDesc')}</div>
            </div>
          )}

          {tasks.map((task) => (
            <TaskMessage
              key={task.task_id}
              task={task}
              chatId={chatId!}
              streamText={streamText[task.task_id]}
              callingTool={callingTools[task.task_id]}
              compacting={compacting[task.task_id]}
              onRetry={handleRetryConversation}
              onStop={handleStopConversation}
              onRegenerate={handleRegenerate}
            />
          ))}

          <div ref={messagesEndRef} />
        </div>

        <div className="chat-input-area">
          <div
            className={`chat-input-card${dragOver ? ' is-dragover' : ''}`}
            onDragOver={(e) => { e.preventDefault(); setDragOver(true); }}
            onDragLeave={() => setDragOver(false)}
            onDrop={handleDrop}
          >
            <MCPToolbar chatId={chatId!} onUpdated={fetchDetail} />
            <SkillToolbar
              chatId={chatId!}
              skills={skills}
              attached={attachedSkills}
              loading={skillsLoading}
              reload={reloadSkills}
              onUpdated={fetchDetail}
            />

            {/* 待发送附件条：上传成功后立即出现，发送时随消息带走 */}
            {(pendingFiles.length > 0 || uploading) && (
              <div className="pending-files">
                {pendingFiles.map((f, i) => (
                  <span key={f.path || i} className="pending-file-pill" title={f.path || f.name}>
                    <span className="pending-file-icon" aria-hidden>
                      {f.file_type === 1 ? '🖼' : f.file_type === 3 ? '📄' : '📦'}
                    </span>
                    <span className="pending-file-name">{f.name}</span>
                    <span
                      className="pending-file-remove"
                      onClick={() => removePendingFile(f.path, i)}
                      title={t('chat.remove')}
                    >
                      ×
                    </span>
                  </span>
                ))}
                {uploading && (
                  <span className="pending-file-uploading">{t('chat.uploading')}</span>
                )}
              </div>
            )}

            {/* @ / # 引用菜单：浮在输入卡片上方，不与正文重叠 */}
            {mention && (
              <InputMentionMenu
                title={
                  mention.trigger === '@'
                    ? t('chat.mentionTitleFile')
                    : t('chat.mentionTitleSkill')
                }
                emptyText={
                  mention.trigger === '@'
                    ? t('chat.mentionEmptyFile')
                    : t('chat.mentionEmptySkill')
                }
                footer={t('chat.mentionFooter')}
                items={mentionItems}
                activeIndex={Math.min(mentionIndex, Math.max(0, mentionItems.length - 1))}
                onHover={setMentionIndex}
                onPick={pickMention}
              />
            )}

            <textarea
              ref={textareaRef}
              className="chat-input-textarea"
              value={inputText}
              onChange={(e) => {
                // 正文真正变了，解除 Esc 的关闭状态
                mentionDismissedRef.current = false;
                setInputText(e.target.value);
                syncMention(e.target.value, e.target.selectionStart);
              }}
              onKeyDown={handleKeyDown}
              // 方向键 / Home / End 只挪光标不改正文，onChange 不触发；
              // 这里补一次判定，让用户把光标移回片段末尾时菜单能重新出现
              onKeyUp={(e) => {
                if (e.key === 'Escape') return;
                syncMention(inputText, (e.target as HTMLTextAreaElement).selectionStart);
              }}
              onClick={(e) =>
                syncMention(inputText, (e.target as HTMLTextAreaElement).selectionStart)
              }
              onBlur={closeMention}
              onPaste={handlePaste}
              placeholder={t('chat.inputPlaceholder')}
              disabled={sending}
              rows={3}
            />
            <div className="chat-input-bottom-bar">
              <div className="chat-input-bottom-left">
                {/* 隐藏的原生 file input，由回形针按钮触发 */}
                <input
                  ref={fileInputRef}
                  type="file"
                  multiple
                  style={{ display: 'none' }}
                  onChange={handleFilePick}
                />
                <button
                  type="button"
                  className="chat-attach-btn"
                  onClick={() => fileInputRef.current?.click()}
                  disabled={uploading || sending}
                  title={t('chat.attachTitle')}
                >
                  {t('chat.attach')}
                </button>
                {/* 「新开工作流」开关。默认由模型判断本轮是续接已有工作流还是
                    新开，但用户往往更清楚；开启后无条件新开，仅作用于本条消息。
                    对话还没有任何工作流时不显示 —— 那时没有「续接」这个选项。 */}
                {(chatInfo.workflow_list?.length ?? 0) > 0 && (
                  <button
                    type="button"
                    className={`chat-new-wf-btn${forceNewWorkflow ? ' is-active' : ''}`}
                    onClick={() => setForceNewWorkflow((v) => !v)}
                    disabled={sending}
                    title={
                      forceNewWorkflow
                        ? t('chat.newWfOnTitle')
                        : t('chat.newWfOffTitle')
                    }
                  >
                    {forceNewWorkflow ? t('chat.newWfOn') : t('chat.newWfOff')}
                  </button>
                )}
                <span className="chat-input-hint">{t('chat.inputHint')}</span>
              </div>
              <div className="chat-input-bottom-right">
                {/* 上下文用量指示器：最近一次 LLM 调用的输入占比，点击看分块明细 */}
                <ContextUsage chatId={chatId!} />
                <span className="chat-input-char-count">
                  {t('chat.charCount', { n: inputText.length })}
                </span>
                <button
                  className="chat-input-send-btn"
                  onClick={handleSend}
                  disabled={sending || uploading || (!inputText.trim() && pendingFiles.length === 0)}
                  title={t('chat.sendTitle')}
                >
                  {sending ? (
                    <span className="chat-input-send-spinner" />
                  ) : (
                    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
                      <line x1="12" y1="19" x2="12" y2="5" />
                      <polyline points="5 12 12 5 19 12" />
                    </svg>
                  )}
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>

      {/* 工作流结果分栏：
          - 展开：渲染 splitter + panel（panel 自带 width 样式 + 折叠按钮）
          - 折叠：panel 内部自行渲染侧边的折叠条（width=0），splitter 隐藏
          两种状态都不影响对话区 —— 对话区靠 flex:1 自动撑满剩余空间 */}
      {chatInfo.workflow && !workflowActive && (
        <>
          {panelExpanded && (
            <div
              className="chat-splitter"
              onMouseDown={handleSplitterMouseDown}
              role="separator"
              aria-orientation="vertical"
              aria-label={t('chat.splitterAria')}
              title={t('chat.splitterTitle')}
            />
          )}
          <WorkflowResultPanel
            workflow={chatInfo.workflow}
            expanded={panelExpanded}
            width={panelWidth}
            onToggleExpanded={() => setPanelExpanded((v) => !v)}
            chatId={chatInfo.chat_id}
          />
        </>
      )}
    </div>
    </MarkdownWorkspaceContext.Provider>
  );
}

// ============================================================
// Chat Header
// ============================================================

function ChatHeader({
  chatInfo,
  onRefresh,
  panelExpanded,
  onTogglePanel,
}: {
  chatInfo: ConversationInfo;
  onRefresh: () => void;
  panelExpanded?: boolean;
  onTogglePanel?: () => void;
}) {
  const navigate = useNavigate();
  const t = useT();
  const [editing, setEditing] = useState(false);
  const [titleDraft, setTitleDraft] = useState('');
  // 工作流执行中（排队/进行中）不提供「结果」入口，只有到达终态才展示。
  const wf = chatInfo.workflow;
  const wfActive =
    wf?.task_status === TaskStatus.PENDING || wf?.task_status === TaskStatus.RUNNING;

  const startEditTitle = () => {
    // 占位标题不进编辑框，用户直接敲新标题即可
    setTitleDraft(isPlaceholderTitle(chatInfo.title) ? '' : chatInfo.title || '');
    setEditing(true);
  };

  const commitEditTitle = async () => {
    if (!editing) return;
    setEditing(false);
    const title = titleDraft.trim();
    if (!title || title === (chatInfo.title || '')) return;
    try {
      await updateConversationTitle(chatInfo.chat_id, title);
      window.dispatchEvent(new Event('conversation-list-updated'));
      onRefresh();
    } catch (err: any) {
      alert(t('sidebar.renameFailed', { msg: err.message }));
    }
  };

  const openWorkspaceDir = async () => {
    try {
      await openWorkspace(chatInfo.chat_id);
    } catch (err: any) {
      alert(t('chat.openWorkspaceFailed', { msg: err?.message || t('chat.unknownError') }));
    }
  };

  return (
    <div className="chat-header">
      {editing ? (
        <input
          className="chat-title-input"
          autoFocus
          value={titleDraft}
          onChange={(e) => setTitleDraft(e.target.value)}
          onBlur={commitEditTitle}
          onKeyDown={(e) => {
            if (e.key === 'Enter') commitEditTitle();
            else if (e.key === 'Escape') setEditing(false);
          }}
        />
      ) : (
        <span className="chat-title">
          {displayTitle(chatInfo.title, t('nav.newChat'), t('chat.untitled'))}
          <span style={{ fontSize: 12, color: 'var(--text-muted)', marginLeft: 10, fontWeight: 400 }}>
            {chatInfo.chat_id}
          </span>
          <button
            className="btn btn-ghost btn-icon btn-sm"
            onClick={startEditTitle}
            title={t('sidebar.rename')}
            style={{ marginLeft: 6 }}
          >
            ✎
          </button>
        </span>
      )}
      <div className="chat-actions">
        <ModelSelector
          chatId={chatInfo.chat_id}
          currentModelId={chatInfo.model_id}
          onChanged={onRefresh}
        />
        {wf && !wfActive && (
          <button
            className={`btn btn-ghost btn-sm${panelExpanded ? ' is-active' : ''}`}
            onClick={onTogglePanel}
            title={panelExpanded ? t('chat.collapsePanelTitle') : t('chat.expandPanelTitle')}
          >
            {panelExpanded ? t('chat.collapseResult') : t('chat.expandResult')}
          </button>
        )}
        {wf && (
          <button
            className="btn btn-ghost btn-sm"
            onClick={() => navigate(`/chat/${chatInfo.chat_id}/workflow`)}
            title={t('chat.viewWorkflowTitle')}
          >
            {t('chat.workflow')}
          </button>
        )}
        <button
          className="btn btn-ghost btn-sm"
          onClick={openWorkspaceDir}
          title={t('chat.openWorkspaceTitle')}
        >
          {t('chat.openWorkspace')}
        </button>
        <button className="btn btn-ghost btn-sm" onClick={onRefresh}>{t('common.refresh')}</button>
      </div>
    </div>
  );
}

// ============================================================
// Model Selector —— 对话头部模型选择器
// ============================================================

function ModelSelector({
  chatId,
  currentModelId,
  onChanged,
}: {
  chatId: string;
  currentModelId?: string;
  onChanged: () => void;
}) {
  const [models, setModels] = useState<ModelConfig[]>([]);
  const [changing, setChanging] = useState(false);
  const t = useT();

  useEffect(() => {
    listModels()
      .then((res) => setModels(res.data.data?.list || []))
      .catch((e) => console.error('加载模型列表失败', e));
  }, []);

  const enabled = models.filter((m) => m.enabled);

  const handleChange = async (e: React.ChangeEvent<HTMLSelectElement>) => {
    const id = e.target.value;
    setChanging(true);
    try {
      await updateConversationModel(chatId, id);
      onChanged();
    } catch (err: any) {
      alert(t('chat.switchModelFailed', { msg: err?.message || t('chat.unknownError') }));
    } finally {
      setChanging(false);
    }
  };

  return (
    <div className="model-selector" title={t('chat.modelSelectorTitle')}>
      <span className="model-selector-icon" aria-hidden>🤖</span>
      <select
        className="model-selector-select"
        value={currentModelId || ''}
        onChange={handleChange}
        disabled={changing || enabled.length === 0}
      >
        <option value="">{t('chat.defaultModel')}</option>
        {enabled.map((m) => (
          <option key={m.id} value={m.id}>
            {m.name}{m.is_default ? t('chat.defaultSuffix') : ''}
          </option>
        ))}
      </select>
    </div>
  );
}

// ============================================================
// Task Message
// ============================================================

function TaskMessage({
  task,
  chatId,
  streamText,
  callingTool,
  compacting,
  onRetry,
  onStop,
  onRegenerate,
}: {
  task: TaskDetail;
  /** 当前对话 ID：规划回复的「查看工作流」跳转需要它定位工作流面板 */
  chatId: string;
  streamText?: string;
  callingTool?: CallingTool;
  compacting?: CompactingState;
  onRetry?: (taskId: string) => void;
  onStop?: (taskId: string) => void;
  /**
   * 重新生成：以本轮完全相同的输入（正文 + 附件）发起新一轮。
   * 由页面统一实现，因为它要负责把新任务塞进轮询集合并刷新列表。
   */
  onRegenerate?: (text: string, files: FileInfo[]) => Promise<void>;
}) {
  const navigate = useNavigate();
  const t = useT();
  const isRunning = task.task_status === 1 || task.task_status === 2;

  // ---- 用户输入的复制 / 重新生成 ----
  const inputText = task.task_input?.input_text || '';
  const inputFiles = task.task_input?.input_files || [];
  const [copied, setCopied] = useState(false);
  const [regenerating, setRegenerating] = useState(false);

  // 复制只取正文，不含附件 —— 附件在下方 OutputFiles 里各自可下载，
  // 拼进剪贴板反而会掺进一堆工作区路径。
  const handleCopyInput = async () => {
    if (!inputText) return;
    try {
      await navigator.clipboard.writeText(inputText);
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    } catch {
      /* 剪贴板不可用（非 https / 无权限）就忽略，不打断交互 */
    }
  };

  const handleRegenerateClick = async () => {
    if (!onRegenerate || regenerating || isRunning) return;
    setRegenerating(true);
    try {
      await onRegenerate(inputText, inputFiles);
    } finally {
      setRegenerating(false);
    }
  };
  // 对话任务可重试的终态：失败(4) / 已终止(5)。
  // 部分失败(6)是工作流专有状态，不会出现在对话任务上，故不在此列。
  //
  // 已移交工作流的规划回复（agent_output.workflow_task_id 非空）不显示重试：
  // 它的失败/终止状态绑定自工作流节点（后端 DeriveBoundTaskStatus），
  // 按对话任务重试只会再生成一段重复回复，修不了工作流 —— 正确入口是工作流。
  const delegatedToWorkflow = !!task.agent_output?.workflow_task_id;
  const retryable =
    (task.task_status === 4 || task.task_status === 5) && !delegatedToWorkflow;

  // 规划回复 → 工作流详情：先激活该工作流（一个对话可有多条，
  // 详情页默认展示当前激活的那条），再跳转，保证落到的是本轮规划的那条。
  const jumpToWorkflow = async (wfTaskId: string) => {
    try {
      await setActiveWorkflow(chatId, wfTaskId);
    } catch {
      // 激活失败（网络抖动等）也照常跳转：详情页还有切换器可手动切
    }
    navigate(`/chat/${chatId}/workflow`);
  };

  // 状态文案按前端枚举从词典取词，不直接用后端返回的中文状态名
  const statusLabel: Record<number, string> = {
    1: t('status.pending'),
    2: t('status.running'),
    3: t('status.completed'),
    4: t('status.failed'),
    5: t('status.cancelled'),
    6: t('status.partialFailed'),
  };

  const statusClass: Record<number, string> = {
    1: 'pending',
    2: 'running',
    3: 'completed',
    4: 'failed',
    5: 'cancelled',
    6: 'failed',
  };

  // 执行中/排队中时，耗时由前端按秒累加（基线=最近轮询的 time_cost_ms），
  // 不再等 5s 一次的轮询才更新；终态直接用后端算死的 time_cost 字符串。
  const tickingMs = useTickingElapsed(task.time_cost_ms || 0, isRunning);

  return (
    <>
      {/* User input */}
      <div className="message-wrapper user">
        <div className="message-avatar">U</div>
        <div className="message-body">
          <div className="message-role">User</div>
          <div className="message-content">{inputText || t('chat.noTextInput')}</div>
          {inputFiles.length > 0 && (
            <OutputFiles files={inputFiles} label={t('chat.attachments')} compact />
          )}
          {/* 用户输入操作条：复制正文 / 以相同输入重新生成一轮 */}
          <div className="user-msg-actions">
            <button
              type="button"
              className="user-msg-btn"
              onClick={handleCopyInput}
              disabled={!inputText}
              title={inputText ? t('chat.copyInputTitle') : t('chat.noTextInputTitle')}
            >
              {copied ? t('common.copied') : t('common.copy')}
            </button>
            <button
              type="button"
              className="user-msg-btn"
              onClick={handleRegenerateClick}
              disabled={
                !onRegenerate ||
                regenerating ||
                isRunning ||
                (!inputText && inputFiles.length === 0)
              }
              title={
                isRunning
                  ? t('chat.regenerateRunningTitle')
                  : t('chat.regenerateTitle')
              }
            >
              {regenerating ? t('chat.regenerating') : t('chat.regenerate')}
            </button>
          </div>
        </div>
      </div>

      {/* Agent response */}
      <div className="message-wrapper agent">
        <div className="message-avatar">AI</div>
        <div className="message-body">
          <div className="message-role" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span>Agent</span>
            <span style={{ color: 'var(--text-muted)', fontSize: 11 }}>
              {task.task_id}
            </span>
            {task.agent_output?.workflow_task_id && (
              <button
                type="button"
                className="wf-jump-btn"
                onClick={() => jumpToWorkflow(task.agent_output!.workflow_task_id!)}
                title={t('chat.viewWfFromTurnTitle')}
              >
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" aria-hidden>
                  <circle cx="5" cy="6" r="2.4" />
                  <circle cx="19" cy="5" r="2.4" />
                  <circle cx="12" cy="18" r="2.4" />
                  <path d="M7 7l3.6 9M17 7l-3.6 9M7.4 6h9.2" />
                </svg>
                {t('chat.viewWorkflow')}
              </button>
            )}
            <a
              className="btn btn-ghost btn-sm"
              style={{ marginLeft: 'auto', fontSize: 11, padding: '2px 8px', textDecoration: 'none' }}
              href={`/debug/${task.task_id}`}
              target="_blank"
              rel="noopener noreferrer"
              title={t('chat.debugTitle')}
            >
              {t('chat.debug')}
            </a>
          </div>

          {isRunning && (
            <div className="task-card">
              <div className="task-header">
                {/* 状态文案/样式跟随 task_status 变动 —— 排队中与执行中不能再共用同一行硬编码 */}
                <span className={`status-dot ${statusClass[task.task_status] || 'running'}`}>
                  {statusLabel[task.task_status] || t('status.running')}
                </span>
                <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                  <span style={{ fontSize: 12, color: 'var(--text-muted)' }}>
                    {/* 耗时前端按秒自主累加，不再跟轮询一起跳动 */}
                    {formatElapsedMs(tickingMs)}
                    {task.progress != null && ` · ${task.progress.toFixed(0)}%`}
                  </span>
                  {/* 已移交工作流的规划回复不显示「停止」：它本身已收口，真正在跑的是
                      绑定的工作流，停止入口在工作流面板/横幅上。这里硬停只会被工作流
                      下一轮 sync 刷回 running（与重试按钮的 delegatedToWorkflow 同理）。 */}
                  {onStop && !delegatedToWorkflow && (
                    <button
                      type="button"
                      className="btn btn-warning btn-sm"
                      onClick={() => onStop(task.task_id)}
                      title={t('chat.stopTaskTitle')}
                    >
                      {t('common.stop')}
                    </button>
                  )}
                </div>
              </div>
              {task.progress != null && (
                <div className="progress-bar">
                  <div className="progress-fill" style={{ width: `${task.progress}%` }} />
                </div>
              )}

              {/* 正在压缩上下文：历史超过模型上限 80% 时触发，压缩本身要调
                  LLM（数秒到数十秒），期间没有任何 turn 产生，必须显式提示，
                  否则用户只看到界面卡住。素材一次装不下时会分多段压缩，
                  此时额外显示段进度。 */}
              {compacting && (
                <div className="calling-tool">
                  <span className="calling-tool-spinner" />
                  {t('chat.compacting')}
                  {compacting.limit > 0 && (
                    <span className="compacting-meta">
                      {` (${fmtTokenShort(compacting.tokens)} / ${fmtTokenShort(compacting.limit)})`}
                    </span>
                  )}
                  {compacting.stage && (
                    <span className="compacting-meta">{` ${compacting.stage}`}</span>
                  )}
                </div>
              )}

              {/* 正在调用工具：tool_calling 事件实时驱动，工具执行耗时较长时
                  让用户看到「正在进行到哪一步」而非干等。 */}
              {callingTool && (
                <div className="calling-tool">
                  <span className="calling-tool-spinner" />
                  {t('chat.callingTool')}
                  <code className="calling-tool-name">{callingTool.toolName}</code>
                  {t('chat.callingToolTail')}
                </div>
              )}

              {/* 流式回复：SSE delta 实时拼出的文本，逐字渲染 */}
              {streamText && (
                <div className="stream-reply">
                  <div className="stream-reply-label">
                    {t('chat.generating')}<span className="stream-caret" />
                  </div>
                  <div className="message-content is-markdown">
                    <Markdown>{streamText}</Markdown>
                  </div>
                </div>
              )}

              {/* 执行中已产生的思考过程。默认展开 —— 任务还在跑时，
                  用户打开页面就是想看「现在进行到哪一步」。 */}
              {task.agent_output && (
                <AgentOutputView output={task.agent_output} defaultShowThink running />
              )}
            </div>
          )}

          {!isRunning && (
            <div className="task-card">
              <div className="task-header">
                <span className={`status-dot ${statusClass[task.task_status]}`}>
                  {statusLabel[task.task_status] || t('common.unknown')}
                </span>
                <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                  <span className="task-id">{task.time_cost}</span>
                  <TokenBadge task={task} />
                  {retryable && onRetry && (
                    <button
                      type="button"
                      className="btn btn-ghost btn-sm"
                      onClick={() => onRetry(task.task_id)}
                      title={t('chat.retryTitle')}
                    >
                      {t('common.retry')}
                    </button>
                  )}
                </div>
              </div>

              {task.task_status === 4 && task.failed_reason && (
                <div style={{ color: 'var(--danger)', fontSize: 13, marginTop: 6, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                  {typeof task.failed_reason === 'string'
                    ? task.failed_reason
                    : `[${task.failed_reason.code}] ${task.failed_reason.message}`}
                </div>
              )}

              {/* Single agent output。失败时后端也会带回已有的思考过程，
                  此时默认展开 —— 中间过程正是排查失败原因的关键。 */}
              {task.agent_output && (
                <AgentOutputView
                  output={task.agent_output}
                  defaultShowThink={task.task_status === 4}
                />
              )}
            </div>
          )}
        </div>
      </div>
    </>
  );
}

// ============================================================
// Token Badge
// ============================================================

/** 紧凑显示 token 数：>=10000 用 k，保留一位小数 */
function fmtTokenShort(n: number): string {
  if (n >= 10000) return `${(n / 1000).toFixed(1)}k`;
  return n.toLocaleString();
}

/**
 * 任务的 token 消耗徽标。
 *
 * 显示的是**含子任务**的总量。这一点是刻意的：deep_search 会派生独立
 * 子任务去做多轮「搜索 → 抓正文」，消耗全记在子任务名下，只看主循环
 * 会严重低估 —— 实测一次带检索的对话，子任务占到总量的 88%。
 *
 * 子任务有消耗时额外显示「🔍 N」分量，让这部分开销显性化，
 * 而不是埋在一个总数里。悬停可看输入/输出拆分（输入侧才是上下文水位）。
 *
 * 老后端不返回这些字段 → total 为 0 → 整个徽标不渲染，不占位。
 */
function TokenBadge({ task }: { task: TaskDetail }) {
  const t = useT();
  const total = task.total_tokens ?? 0;
  if (total <= 0) return null;

  const sub = task.sub_task_tokens ?? 0;
  const own = task.own_tokens ?? 0;
  const prompt = task.prompt_tokens ?? 0;
  const completion = task.completion_tokens ?? 0;

  const tip = [
    t('chat.tokenTipTotal', { n: total.toLocaleString() }),
    t('chat.tokenTipSplit', {
      prompt: prompt.toLocaleString(),
      completion: completion.toLocaleString(),
    }),
    sub > 0
      ? t('chat.tokenTipSub', {
          own: own.toLocaleString(),
          sub: sub.toLocaleString(),
          pct: ((sub / total) * 100).toFixed(0),
        })
      : '',
  ]
    .filter(Boolean)
    .join('\n');

  return (
    <span className="token-badge" title={tip}>
      <span className="token-badge-total">{fmtTokenShort(total)} tok</span>
      {sub > 0 && <span className="token-badge-sub">🔍 {fmtTokenShort(sub)}</span>}
    </span>
  );
}

// ============================================================
// Agent Output View
// ============================================================
function AgentOutputView({
  output,
  defaultShowThink = false,
  running = false,
}: {
  output: AgentOutput;
  /** 默认展开思考过程。执行中与失败时置为 true —— 这两种情况用户最需要看中间过程 */
  defaultShowThink?: boolean;
  /** 任务仍在执行中。此时不显示「无文本回复」占位，因为答案还没产生 */
  running?: boolean;
}) {
  const t = useT();
  const [showThink, setShowThink] = useState(defaultShowThink);
  const hasReply = !!output.reply_text?.trim();
  // 后端已产出分层思考过程（规划期 / 执行期）时，改用两层分组展示；
  // 老数据或未分层时回退到平铺列表。
  // 后端 omitempty 可能让 planning/execution 某一层缺字段，统一兜底为空数组
  const planning = output.think_layers?.planning ?? [];
  const execution = output.think_layers?.execution ?? [];
  const hasLayers = !!output.think_layers && (planning.length > 0 || execution.length > 0);

  return (
    <div style={{ marginTop: 10 }}>
      {hasReply ? (
        <div className="message-content is-markdown">
          <Markdown>{output.reply_text}</Markdown>
        </div>
      ) : (
        // 任务已结束但没有文本回复时给出占位，
        // 否则用户只看到「已完成」加一片空白，会以为界面出错。
        // 执行中不显示 —— 答案本来就还没生成。
        !running && <div className="message-empty-hint">{t('chat.noTextReply')}</div>
      )}

      {output.output_files?.length > 0 && (
        <OutputFiles files={output.output_files} label={t('chat.outputFiles')} />
      )}

      {output.tool_calls?.length > 0 && (
        <ToolCalls calls={output.tool_calls} />
      )}

      {output.think_process?.length > 0 && (
        <div className="think-process">
          <div className="think-title" onClick={() => setShowThink(!showThink)}>
            {showThink ? '▾' : '▸'} {t('chat.thinkProcess', { n: output.think_process.length })}
            {running && <span className="think-live">{t('chat.inProgress')}</span>}
          </div>
          {showThink &&
            (hasLayers ? (
              <ThinkLayersView layers={output.think_layers!} />
            ) : (
              output.think_process.map((t, i) => (
                <div key={i} className="think-item">
                  <Markdown compact>{t}</Markdown>
                </div>
              ))
            ))}
        </div>
      )}
    </div>
  );
}

/** 分层思考过程：规划期 / 执行期 两个可独立折叠的分组 */
function ThinkLayersView({ layers }: { layers: ThinkLayers }) {
  const t = useT();
  const planning = layers.planning ?? [];
  const execution = layers.execution ?? [];
  return (
    <div className="think-layers">
      {planning.length > 0 && (
        <ThinkGroup title={t('chat.planningPhase')} steps={planning} defaultOpen />
      )}
      {execution.length > 0 && (
        <ThinkGroup
          title={t('chat.executionPhase')}
          steps={execution}
          // 执行期默认折叠，但当前有步骤在跑时必须展开 ——
          // 折叠状态下用户只能看到「进行中」标题，不知道在跑什么。
          defaultOpen={execution.some((s) => s.running)}
        />
      )}
    </div>
  );
}

function ThinkGroup({
  title,
  steps,
  defaultOpen,
}: {
  title: string;
  steps: ThinkStep[];
  defaultOpen: boolean;
}) {
  const t = useT();
  const hasRunning = steps.some((s) => s.running);
  const [open, setOpen] = useState(defaultOpen || hasRunning);
  // 该分组首次出现执行中的步骤时自动展开。
  //
  // defaultOpen 只是 useState 初值：分组已渲染（含已完成步骤）且处于折叠态时，
  // 新步骤开始执行并不会重新触发初始化，用户就永远看不到当前进度。
  // 这里只在「无 → 有」的跳变时展开，之后用户手动折叠不会被强行拉回。
  const wasRunning = useRef(hasRunning);
  useEffect(() => {
    if (hasRunning && !wasRunning.current) {
      setOpen(true);
    }
    wasRunning.current = hasRunning;
  }, [hasRunning]);

  return (
    <div className="think-group">
      <div className="think-group-title" onClick={() => setOpen(!open)}>
        {open ? '▾' : '▸'} {t('chat.thinkGroup', { title, n: steps.length })}
      </div>
      {open &&
        steps.map((s, i) => (
          <ThinkItem key={i} step={s} />
        ))}
    </div>
  );
}

/** 单条思考过程：默认只显示 summary，带 detail 时可点击展开完整执行信息 */
function ThinkItem({ step }: { step: ThinkStep }) {
  const t = useT();
  const hasDetail = !!step.detail?.trim();
  const [expanded, setExpanded] = useState(false);
  // 执行中的步骤尾部挂一个呼吸的「进行中」，与已完成的步骤区分开
  const live = step.running ? <span className="think-live">{t('chat.inProgress')}</span> : null;
  const cls = 'think-item' + (step.running ? ' think-item-running' : '');

  if (!hasDetail) {
    return (
      <div className={cls}>
        <Markdown compact>{step.summary}</Markdown>
        {live}
      </div>
    );
  }

  return (
    <div className={cls + ' think-item-expandable'}>
      <div
        className="think-step-summary"
        onClick={() => setExpanded(!expanded)}
        style={{ cursor: 'pointer' }}
      >
        <span className="think-step-toggle">{expanded ? '▾' : '▸'}</span>{' '}
        <Markdown compact>{step.summary}</Markdown>
        {live}
      </div>
      {expanded && (
        <div className="think-step-detail">
          <Markdown compact>{step.detail!}</Markdown>
        </div>
      )}
    </div>
  );
}

// ============================================================
// MCP Toolbar (常驻于输入框上方, 像截图里 "+ [tag] [tag]" 的形式)
// ============================================================

function MCPToolbar({
  chatId,
  onUpdated,
}: {
  chatId: string;
  onUpdated: () => void;
}) {
  const [servers, setServers] = useState<MCPServerConfig[]>([]);
  const [attached, setAttached] = useState<Array<{ server_id: string; name: string; enabled: boolean }>>([]);
  const [loading, setLoading] = useState(true);
  const [addOpen, setAddOpen] = useState(false);
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const popoverRef = useRef<HTMLDivElement>(null);
  const t = useT();

  useEffect(() => {
    (async () => {
      try {
        const [mcpRes, convRes] = await Promise.all([
          listMCPServers(),
          listConversationMCPServers(chatId),
        ]);
        setServers(mcpRes.data.data?.list || []);
        setAttached(convRes.data.data?.items || []);
      } catch (e) {
        console.error(e);
      } finally {
        setLoading(false);
      }
    })();
  }, [chatId]);

  // 点击外部关闭添加面板
  useEffect(() => {
    if (!addOpen) return;
    const handler = (e: MouseEvent) => {
      if (popoverRef.current && !popoverRef.current.contains(e.target as Node)) {
        setAddOpen(false);
      }
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [addOpen]);

  const handleRemove = async (serverId: string) => {
    try {
      await removeConversationMCP(chatId, [serverId]);
      setAttached((prev) => prev.filter((s) => s.server_id !== serverId));
      onUpdated();
    } catch (e: any) {
      alert(t('chat.removeFailed', { msg: e.message }));
    }
  };

  const handleAdd = async () => {
    const ids = Array.from(selectedIds);
    if (ids.length === 0) return;
    try {
      await addConversationMCP(chatId, ids);
      const newAttached = servers.filter((s) => ids.includes(s.id!)).map((s) => ({
        server_id: s.id!,
        name: s.name,
        enabled: true,
      }));
      setAttached((prev) => [...prev, ...newAttached]);
      setSelectedIds(new Set());
      setAddOpen(false);
      onUpdated();
    } catch (e: any) {
      alert(t('chat.addFailed', { msg: e.message }));
    }
  };

  const availableServers = servers.filter(
    (s) => !attached.find((a) => a.server_id === s.id)
  );

  return (
    <div className="mcp-toolbar">
      <div className="mcp-toolbar-inner">
        <button
          className="mcp-add-btn"
          onClick={() => setAddOpen(!addOpen)}
          title={t('chat.addMcpTitle')}
        >
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round">
            <line x1="12" y1="5" x2="12" y2="19" />
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
          <span>{t('chat.addTool')}</span>
        </button>

        {loading ? (
          <span className="mcp-empty-hint">{t('common.loading')}</span>
        ) : attached.length === 0 ? (
          <span className="mcp-empty-hint">{t('chat.noMcpMounted')}</span>
        ) : (
          attached.map((s) => (
            <span key={s.server_id} className="mcp-pill">
              <span className="mcp-pill-dot" />
              {s.name}
              <span className="mcp-pill-remove" onClick={() => handleRemove(s.server_id)} title={t('chat.remove')}>×</span>
            </span>
          ))
        )}

        {addOpen && (
          <div className="mcp-popover" ref={popoverRef}>
            <div className="mcp-popover-title">{t('chat.selectMcpServers')}</div>
            {availableServers.length === 0 ? (
              <div className="mcp-popover-empty">
                {t('chat.noMcpAvailable')}
                <a href="/mcp" target="_blank" rel="noreferrer">{t('chat.goToMcpPage')}</a>
              </div>
            ) : (
              <>
                <div className="mcp-popover-list">
                  {availableServers.map((s) => (
                    <label key={s.id} className="mcp-popover-item">
                      <input
                        type="checkbox"
                        checked={selectedIds.has(s.id!)}
                        onChange={() => {
                          const next = new Set(selectedIds);
                          if (next.has(s.id!)) next.delete(s.id!);
                          else next.add(s.id!);
                          setSelectedIds(next);
                        }}
                      />
                      <span className="mcp-popover-name">{s.name}</span>
                      {s.description && (
                        <span className="mcp-popover-desc">{s.description}</span>
                      )}
                    </label>
                  ))}
                </div>
                <div className="mcp-popover-footer">
                  <span className="mcp-popover-count">
                    {t('chat.selectedCount', { n: selectedIds.size })}
                  </span>
                  <div style={{ display: 'flex', gap: 6 }}>
                    <button className="btn btn-ghost btn-sm" onClick={() => setAddOpen(false)}>{t('common.cancel')}</button>
                    <button
                      className="btn btn-primary btn-sm"
                      onClick={handleAdd}
                      disabled={selectedIds.size === 0}
                    >
                      {t('chat.confirmAdd')}
                    </button>
                  </div>
                </div>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// ============================================================
// Skill Toolbar (常驻于输入框上方, 对话内启用 Skill)
// ============================================================

/**
 * 对话技能条。
 *
 * 技能数据由 ChatPage 持有并作为 props 传入，而不是本组件自己加载：
 * 输入框的「#」菜单用的是同一份数据，各自加载会出现
 * 「在菜单里启用了、技能条上没出现」的状态不一致，也会重复请求。
 * 本组件只负责展示 + 增删，改动后回调 reload 让父级重拉。
 */
function SkillToolbar({
  chatId,
  skills,
  attached,
  loading,
  reload,
  onUpdated,
}: {
  chatId: string;
  skills: SkillConfig[];
  attached: SkillRef[];
  loading: boolean;
  reload: () => Promise<void>;
  onUpdated: () => void;
}) {
  const [addOpen, setAddOpen] = useState(false);
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const popoverRef = useRef<HTMLDivElement>(null);
  const t = useT();

  useEffect(() => {
    if (!addOpen) return;
    const handler = (e: MouseEvent) => {
      if (popoverRef.current && !popoverRef.current.contains(e.target as Node)) {
        setAddOpen(false);
      }
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [addOpen]);

  const handleRemove = async (skillId: string) => {
    try {
      await removeConversationSkill(chatId, [skillId]);
      await reload();
      onUpdated();
    } catch (e: any) {
      alert(t('chat.removeFailed', { msg: e.message }));
    }
  };

  const handleAdd = async () => {
    const ids = Array.from(selectedIds);
    if (ids.length === 0) return;
    try {
      await addConversationSkill(chatId, ids);
      setSelectedIds(new Set());
      setAddOpen(false);
      await reload();
      onUpdated();
    } catch (e: any) {
      alert(t('chat.addFailed', { msg: e.message }));
    }
  };

  const availableSkills = skills.filter(
    (s) => !attached.find((a) => a.skill_id === s.id)
  );

  return (
    <div className="mcp-toolbar">
      <div className="mcp-toolbar-inner">
        <button
          className="mcp-add-btn"
          onClick={() => setAddOpen(!addOpen)}
          title={t('chat.addSkillTitle')}
        >
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round">
            <line x1="12" y1="5" x2="12" y2="19" />
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
          <span>{t('chat.addSkill')}</span>
        </button>

        {loading ? (
          <span className="mcp-empty-hint">{t('common.loading')}</span>
        ) : attached.length === 0 ? (
          <span className="mcp-empty-hint">{t('chat.noSkillEnabled')}</span>
        ) : (
          attached.map((s) => (
            <span key={s.skill_id} className="mcp-pill">
              <span className="mcp-pill-dot" />
              {s.name}
              <span className="mcp-pill-remove" onClick={() => handleRemove(s.skill_id)} title={t('chat.remove')}>×</span>
            </span>
          ))
        )}

        {addOpen && (
          <div className="mcp-popover" ref={popoverRef}>
            <div className="mcp-popover-title">{t('chat.selectSkills')}</div>
            {availableSkills.length === 0 ? (
              <div className="mcp-popover-empty">
                {t('chat.noSkillAvailable')}
                <a href="/skills" target="_blank" rel="noreferrer">{t('chat.goToSkillPage')}</a>
              </div>
            ) : (
              <>
                <div className="mcp-popover-list">
                  {availableSkills.map((s) => (
                    <label key={s.id} className="mcp-popover-item">
                      <input
                        type="checkbox"
                        checked={selectedIds.has(s.id!)}
                        onChange={() => {
                          const next = new Set(selectedIds);
                          if (next.has(s.id!)) next.delete(s.id!);
                          else next.add(s.id!);
                          setSelectedIds(next);
                        }}
                      />
                      <span className="mcp-popover-name">{s.name}</span>
                      {s.description && (
                        <span className="mcp-popover-desc">{s.description}</span>
                      )}
                    </label>
                  ))}
                </div>
                <div className="mcp-popover-footer">
                  <span className="mcp-popover-count">
                    {t('chat.selectedCount', { n: selectedIds.size })}
                  </span>
                  <div style={{ display: 'flex', gap: 6 }}>
                    <button className="btn btn-ghost btn-sm" onClick={() => setAddOpen(false)}>{t('common.cancel')}</button>
                    <button
                      className="btn btn-primary btn-sm"
                      onClick={handleAdd}
                      disabled={selectedIds.size === 0}
                    >
                      {t('chat.confirmAdd')}
                    </button>
                  </div>
                </div>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

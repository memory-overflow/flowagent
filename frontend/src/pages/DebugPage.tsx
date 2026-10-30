// DebugPage —— 任务调试页面（极简版）
//
// 设计原则：只看 input / output，别的都是噪音。
//
// 布局关键（踩过的坑，别改回去）：
//   1. 整页只有 **一个** 滚动容器 —— `.debug-page { height:100vh; overflow-y:auto }`。
//      之前用 flex 父子链 + 每个 <pre> 各自 overflow，嵌套滚动器互相抢事件，
//      导致「input 拉不到最后面」。现在全部走文档流，页面滚动即可。
//   2. <pre> 不设 max-height，内容完整铺开，靠页面滚动查看。
//      超长内容用「折叠」按钮手动收起，而不是硬截断。
//   3. header 用 position:sticky 吸顶，不参与 flex 计算。

import { useCallback, useEffect, useRef, useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { listTaskTurns, getTurnDetail, subscribeTaskStream } from '../services/api';
import type { TurnDebugView, TaskDebugSummary, SubTaskSummary } from '../services/api';
import { useBlockMeta } from '../components/ContextUsage';
import type { ContextBlocksView } from '../components/ContextUsage';
import { useI18n, useT } from '../i18n';

// ============================================================
// 常量
// ============================================================

// 调用类型（turn_type）：这一次在调什么
const TURN_TYPE_COLOR: Record<string, string> = {
  functional_call: '#5b8def',
  tool_call: '#f59e0b',
  workflow: '#10b981',
  // 上下文压缩：内部动作，不是 agent 循环里的一步，用灰色系与正常调用区分
  context_compact: '#64748b',
};

// 语义阶段（phase）：这次调用发生在哪个阶段。
// 与 turn_type 正交 —— 「规划期调工具」是完全正常的组合，
// 所以两者必须各占一个徽标，不能二选一。
const PHASE_COLOR: Record<number, string> = {
  1: '#8b5cf6',
  2: '#22b8a6',
};

const STATUS_COLOR: Record<string, string> = {
  running: '#f59e0b',
  success: '#10b981',
  failed: '#ef4444',
  interrupted: '#94a3b8',
};

// 任务状态（model.TaskStatus 枚举值）
const TASK_STATUS_COLOR: Record<number, string> = {
  1: '#888',
  2: '#f59e0b',
  3: '#10b981',
  4: '#ef4444',
  5: '#888',
  6: '#ef4444',
};

// 超过这个行数默认折叠，避免一屏被单个 turn 塞满
const COLLAPSE_LINE_THRESHOLD = 60;

// 单个工具调用的流式累积（raw 分片拼装结果）
interface ToolCallAcc {
  id: string;
  name: string;
  args: string;
}

// 工具调用流式累积：turn_seq -> tool_call index -> 累积结果
type StreamToolCalls = Record<number, Record<number, ToolCallAcc>>;

// ============================================================
// Page
// ============================================================

export default function DebugPage() {
  const { taskId = '' } = useParams<{ taskId: string }>();
  const navigate = useNavigate();
  const t = useT();
  const { lang } = useI18n();

  const [summary, setSummary] = useState<TaskDebugSummary | null>(null);
  const [turns, setTurns] = useState<TurnDebugView[]>([]);
  const [unmappedSubTasks, setUnmappedSubTasks] = useState<SubTaskSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>('');

  // 流式输出缓冲：turn_seq -> 已收到的 delta 增量拼成的实时文本。
  // 用 ref 避免 SSE 回调里的闭包读到旧 state，state 副本驱动渲染。
  const [streamText, setStreamText] = useState<Record<number, string>>({});
  const streamTextRef = useRef<Record<number, string>>({});

  // 思考链流式缓冲：turn_seq -> 已收到的 reasoning_content 增量拼成的实时文本。
  const [streamReasoning, setStreamReasoning] = useState<Record<number, string>>({});
  const streamReasoningRef = useRef<Record<number, string>>({});

  // 工具调用流式累积：turn_seq -> index -> { id, name, args }。
  const [streamToolCalls, setStreamToolCalls] = useState<StreamToolCalls>({});
  const streamToolCallsRef = useRef<StreamToolCalls>({});

  // 折叠状态：turn_seq -> 是否展开，默认折叠（不在 map 里即视为折叠）。
  // 上提到 Page 层，才能支持「一键折叠 / 展开」统一控制。
  const [openTurns, setOpenTurns] = useState<Record<number, boolean>>({});
  const [userInputOpen, setUserInputOpen] = useState(false);

  // 唯一滚动容器 ref，供「跳最前 / 跳最后」按钮使用。
  const scrollRef = useRef<HTMLDivElement>(null);

  // 静默刷新：不切换 loading，供 SSE 事件与轮询兜底使用，避免界面闪烁。
  const refresh = useCallback(async () => {
    try {
      const res = await listTaskTurns(taskId);
      const data = res.data.data;
      setSummary(data.task);
      setTurns(data.turns || []);
      setUnmappedSubTasks(data.unmapped_sub_tasks || []);
    } catch {
      // 静默刷新失败不打断，保留上次数据，等下一次刷新
    }
  }, [taskId]);

  const load = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const res = await listTaskTurns(taskId);
      const data = res.data.data;
      setSummary(data.task);
      setTurns(data.turns || []);
      setUnmappedSubTasks(data.unmapped_sub_tasks || []);
    } catch (e: any) {
      setError(e?.message || t('debug.loadFailed'));
    } finally {
      setLoading(false);
    }
  }, [taskId, t]);

  useEffect(() => {
    load();
  }, [load]);

  // 订阅 SSE：delta 按 turn_seq 累积实时文本，turn_done 时静默回拉。
  // subscribeTaskStream 内部在 task_done 后会自动 es.close()，根除浏览器原生
  // EventSource 的 ≈3s 自动重连 + 服务端反复 task_done 的死循环。
  // 不再需要任何轮询兜底——SSE 连接一次就好。
  useEffect(() => {
    if (!taskId) return;
    return subscribeTaskStream(taskId, {
      onDelta: (text, ev) => {
        if (ev.turn_seq == null) return;
        streamTextRef.current[ev.turn_seq] = (streamTextRef.current[ev.turn_seq] || '') + text;
        setStreamText({ ...streamTextRef.current });
      },
      onReasoning: (text, ev) => {
        if (ev.turn_seq == null) return;
        streamReasoningRef.current[ev.turn_seq] = (streamReasoningRef.current[ev.turn_seq] || '') + text;
        setStreamReasoning({ ...streamReasoningRef.current });
      },
      onToolCall: (tc, ev) => {
        if (ev.turn_seq == null) return;
        const byTurn = streamToolCallsRef.current[ev.turn_seq]
          || (streamToolCallsRef.current[ev.turn_seq] = {});
        const acc = byTurn[tc.index] || (byTurn[tc.index] = { id: '', name: '', args: '' });
        if (tc.id) acc.id = tc.id;
        if (tc.name) acc.name = tc.name;
        if (tc.args) acc.args += tc.args;
        setStreamToolCalls({ ...streamToolCallsRef.current });
      },
      onTurnDone: () => { refresh(); },
      // task_done 由 api.ts 内 es.close() 自动处理，不必再操作。
      onError: () => {
        // 兜底：连接被踢、网络抖动、服务端崩溃等异常。一次性回拉，不重启订阅——
        // 当前任务的最终状态由 refresh 拿到；如确实还活着，业务侧可另行起订阅。
        refresh();
      },
    });
  }, [taskId, refresh]);

  const scrollTop = () => scrollRef.current?.scrollTo({ top: 0, behavior: 'smooth' });
  const scrollBottom = () => scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight, behavior: 'smooth' });

  // 是否已全部折叠（决定「一键折叠」按钮的文案与行为）
  const allCollapsed = turns.every((t) => !openTurns[t.turn_seq]) && !userInputOpen;
  const toggleAll = () => {
    if (allCollapsed) {
      const next: Record<number, boolean> = {};
      turns.forEach((t) => { next[t.turn_seq] = true; });
      setOpenTurns(next);
      setUserInputOpen(true);
    } else {
      setOpenTurns({});
      setUserInputOpen(false);
    }
  };

  return (
    <div className="debug-page" ref={scrollRef}>
      <div className="debug-header">
        <div className="debug-header-left">
          {/* 子任务页：顶部提供「返回父任务」入口，避免调试时迷失在子任务里 */}
          {summary?.parent_task_id && (
            <button
              className="debug-btn"
              onClick={() => navigate(`/debug/${summary.parent_task_id}`)}
              title={t('debug.backToParent')}
            >
              {t('debug.parentTask')}
            </button>
          )}
          <h2>{t('debug.title')}</h2>
          <code>{taskId}</code>
        </div>
        <div className="debug-header-right">
          <button className="debug-btn" onClick={scrollTop} title={t('debug.topTip')}>⏫ {t('debug.top')}</button>
          <button className="debug-btn" onClick={scrollBottom} title={t('debug.bottomTip')}>⏬ {t('debug.bottom')}</button>
          <button className="debug-btn" onClick={toggleAll} title={t('debug.toggleAllTip')}>
            {allCollapsed ? t('debug.expandAll') : t('debug.collapseAll')}
          </button>
          <button className="debug-btn" onClick={load} disabled={loading}>
            {loading ? t('common.loading') : t('common.refresh')}
          </button>
        </div>
      </div>

      <div className="debug-body">
        {error && <div className="debug-error">{error}</div>}

        {summary && (
          <TaskInfo
            summary={summary}
            turnCount={turns.length}
            userInputOpen={userInputOpen}
            onToggleUserInput={() => setUserInputOpen((v) => !v)}
          />
        )}

        {/* 子任务入口已迁移到对应 tool_call 行尾部（turn.sub_task_id → 「子任务调试」链接）。
            但对改动前跑完的旧任务，子任务没存 parent_turn_id、映射不到 turn 行，
            这里退回顶部列表兜底展示，避免入口完全消失。新任务全部映射后此列表为空、不渲染。 */}
        {unmappedSubTasks.length > 0 && (
          <UnmappedSubTaskSection subTasks={unmappedSubTasks} />
        )}

        {turns.length === 0 && !loading && !error && (
          <div className="debug-empty">{t('debug.noTurns')}</div>
        )}

        {turns.map((t) => (
          <TurnCard
            key={t.id}
            turn={t}
            open={!!openTurns[t.turn_seq]}
            onToggle={() => setOpenTurns((prev) => ({ ...prev, [t.turn_seq]: !prev[t.turn_seq] }))}
            streamText={streamText[t.turn_seq]}
            streamReasoning={streamReasoning[t.turn_seq]}
            streamToolCalls={streamToolCalls[t.turn_seq]}
          />
        ))}
      </div>

      <style>{DEBUG_PAGE_STYLE}</style>
    </div>
  );
}

// ============================================================
// 任务基础信息 —— 紧凑网格，不抢 turn 的空间
// ============================================================

function TaskInfo({ summary, turnCount, userInputOpen, onToggleUserInput }: {
  summary: TaskDebugSummary;
  turnCount: number;
  userInputOpen: boolean;
  onToggleUserInput: () => void;
}) {
  const t = useT();
  const { lang } = useI18n();
  const s = summary;
  const statusColor = TASK_STATUS_COLOR[s.task_status] || '#888';

  // 任务状态 / 阶段：前端按枚举重新映射（后端 status_name / phase_name 原样兜底）
  const TASK_STATUS_LABEL: Record<number, string> = {
    1: t('status.pending'),
    2: t('status.running'),
    3: t('status.completed'),
    4: t('status.failed'),
    5: t('status.cancelled'),
    6: t('status.partialFailed'),
  };
  const PHASE_LABEL: Record<number, string> = {
    1: t('debug.phase.planning'),
    2: t('debug.phase.execution'),
  };

  // 未结束的任务用「至今」算耗时
  const durationMs = s.end_ts > 0
    ? s.end_ts - (s.start_ts || s.create_ts)
    : Date.now() - (s.start_ts || s.create_ts);

  return (
    <div className="debug-task">
      <div className="debug-task-grid">
        <Field label={t('common.status')}>
          <span style={{ color: statusColor, fontWeight: 600 }}>
            ●&nbsp;{TASK_STATUS_LABEL[s.task_status] || s.status_name}
          </span>
        </Field>
        <Field label={t('common.type')}>{s.task_type === 2 ? t('debug.workflowTask') : t('debug.chatTask')}</Field>
        {/* Phase 与 Stage 是两个维度：前者「在规划还是在执行」，
            后者「调度推进到哪一步」。都要显示，否则看不出任务卡在哪。 */}
        <Field label="Phase">
          <span style={{ color: PHASE_COLOR[s.phase] || '#888' }}>
            {PHASE_LABEL[s.phase] || s.phase_name || '-'}
          </span>
        </Field>
        <Field label="Stage">{s.stage_name}</Field>
        <Field label={t('debug.progress')}>{s.progress?.toFixed(0) ?? 0}%</Field>
        <Field label="Turns">{turnCount}{s.turn_count !== turnCount ? ` / ${s.turn_count}` : ''}</Field>
        {/* 总量含子任务 —— deep_search 会派生独立子任务做多轮检索，
            消耗记在子任务名下。只显示一个总数看不出钱烧在哪，
            故把「子任务 / 其中 deep_search」与「输入 / 输出」都放进 tooltip。 */}
        <Field label="Tokens">
          <span
            title={
              t('debug.tokensInout', { in: fmtTok(s.prompt_tokens), out: fmtTok(s.completion_tokens) }) +
              (s.sub_task_tokens
                ? `\n${t('debug.tokensBreakdown', { own: fmtTok(s.own_tokens), sub: fmtTok(s.sub_task_tokens) })}` +
                  (s.deep_search_tokens ? t('debug.tokensDeepSearch', { n: fmtTok(s.deep_search_tokens) }) : '')
                : '')
            }
          >
            {fmtTok(s.total_tokens)}
            {s.sub_task_tokens ? <span className="debug-tok-sub">+{fmtTok(s.sub_task_tokens)}</span> : null}
          </span>
        </Field>
        <Field label={t('debug.duration')}>{formatDuration(durationMs)}</Field>
        <Field label={t('debug.created')}>{formatDateTime(s.create_ts, lang)}</Field>
        <Field label="Chat" mono>{s.chat_id || '-'}</Field>
        {s.parent_task_id && <Field label={t('debug.parent')} mono>{s.parent_task_id}</Field>}
      </div>

      <UserInputBlock text={s.input_text} open={userInputOpen} onToggle={onToggleUserInput} />

      {s.failed_reason && (
        <div className="debug-task-io">
          <span className="debug-block-label" style={{ color: '#ef4444' }}>{t('debug.failReason')}</span>
          <div className="debug-task-text" style={{ color: '#f87171' }}>
            [{s.failed_reason.code}] {s.failed_reason.message}
          </div>
        </div>
      )}
    </div>
  );
}

// 未映射子任务兜底列表 —— 仅旧数据（子任务无 parent_turn_id）时出现。
// 入口正常情况下已挂在对应 tool_call 行，这里只为「映射不上」的历史子任务保留入口。
function UnmappedSubTaskSection({ subTasks }: { subTasks: SubTaskSummary[] }) {
  const t = useT();
  const TASK_STATUS_LABEL: Record<number, string> = {
    1: t('status.pending'),
    2: t('status.running'),
    3: t('status.completed'),
    4: t('status.failed'),
    5: t('status.cancelled'),
    6: t('status.partialFailed'),
  };
  return (
    <div className="debug-subtasks">
      <div className="debug-subtasks-head">
        <span className="debug-block-label" style={{ color: '#22b8a6' }}>{t('debug.unmappedSubtasks')}</span>
        <span className="debug-dim">{t('debug.count', { n: subTasks.length })}</span>
      </div>
      {subTasks.map((st) => {
        const text = st.input_text || '';
        const preview = text.length > 60 ? text.slice(0, 60).replace(/\n/g, ' ') + '…' : text.replace(/\n/g, ' ');
        return (
          <a
            key={st.task_id}
            className="debug-subtask-item"
            href={`/debug/${st.task_id}`}
            target="_blank"
            rel="noopener noreferrer"
            title={t('debug.openSubtaskNewTab')}
          >
            <span className="debug-subtask-status" style={{ color: TASK_STATUS_COLOR[st.task_status] || '#888' }}>
              ●&nbsp;{TASK_STATUS_LABEL[st.task_status] || st.status_name}
            </span>
            {st.sub_task_kind === 'deep_search' && (
              <span className="debug-subtask-kind">deep_search</span>
            )}
            <span className="debug-subtask-text">{preview || t('debug.noInput')}</span>
            <span className="debug-spacer" />
            <span className="debug-dim">{t('debug.turnsCount', { n: st.turn_count })}</span>
            <span className="debug-subtask-open">{t('debug.open')}</span>
          </a>
        );
      })}
    </div>
  );
}

function Field({ label, children, mono }: { label: string; children: React.ReactNode; mono?: boolean }) {
  return (
    <div className="debug-field">
      <span className="debug-field-label">{label}</span>
      <span className={mono ? 'debug-field-value debug-field-mono' : 'debug-field-value'}>
        {children}
      </span>
    </div>
  );
}

// 用户输入块：受控折叠（open/onToggle 由 Page 层下发），默认折叠，
// 折叠态显示前 8 行预览，短内容折叠时直接显示全文（无内容可藏）。
function UserInputBlock({ text, open, onToggle }: { text?: string; open: boolean; onToggle: () => void }) {
  const t = useT();
  const content = text || '';
  const lineCount = content ? content.split('\n').length : 0;
  const lines = content ? content.split('\n') : [];
  const peek = lines.length > 8 ? lines.slice(0, 8).join('\n') + '\n…' : content;

  if (!content) {
    return (
      <div className="debug-task-io">
        <div className="debug-block-head">
          <span className="debug-block-label" style={{ color: '#a78bfa' }}>{t('debug.userInput')}</span>
          <span className="debug-dim">{t('common.empty')}</span>
        </div>
      </div>
    );
  }

  return (
    <div className="debug-task-io">
      <div className="debug-block-head">
        <span className="debug-block-label" style={{ color: '#a78bfa' }}>{t('debug.userInput')}</span>
        <span className="debug-dim">{t('debug.lines', { n: lineCount })} · {t('debug.chars', { n: content.length })}</span>
        <span className="debug-spacer" />
        <button className="debug-mini-btn" onClick={onToggle}>
          {open ? t('common.collapse') : t('common.expand')}
        </button>
      </div>

      {open
        ? <div className="debug-task-text">{content}</div>
        : <div className="debug-task-text debug-task-text-peek">{peek}</div>}
    </div>
  );
}

// ============================================================
// 单条 turn
// ============================================================

function TurnCard({ turn, open, onToggle, streamText, streamReasoning, streamToolCalls }: {
  turn: TurnDebugView;
  open: boolean;
  onToggle: () => void;
  streamText?: string;
  streamReasoning?: string;
  streamToolCalls?: Record<number, ToolCallAcc>;
}) {
  const t = useT();
  // 列表接口不返回 input/output 大字段，展开时按需拉取单条 turn 完整 payload。
  // detailTried 防止拉取失败后 useEffect 反复重试造成死循环。
  const [detail, setDetail] = useState<TurnDebugView | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailTried, setDetailTried] = useState(false);
  useEffect(() => {
    if (!open || turn.input_raw !== undefined || detail || detailLoading || detailTried) return;
    setDetailLoading(true);
    getTurnDetail(turn.task_id, turn.turn_seq)
      .then((res) => setDetail(res.data.data))
      .catch(() => setDetail(null))
      .finally(() => {
        setDetailLoading(false);
        setDetailTried(true);
      });
  }, [open, turn.input_raw, turn.task_id, turn.turn_seq, detail, detailLoading, detailTried]);
  const payload = detail || turn;
  const color = TURN_TYPE_COLOR[turn.turn_type_name] || '#5b8def';
  const statusColor = STATUS_COLOR[turn.status_name] || '#888';
  const phaseColor = PHASE_COLOR[turn.phase] || '#888';
  const PHASE_LABEL: Record<number, string> = {
    1: t('debug.phase.planning'),
    2: t('debug.phase.execution'),
  };
  const TURN_TYPE_LABEL: Record<string, string> = {
    context_compact: t('debug.turnType.contextCompact'),
  };
  const phaseLabel = PHASE_LABEL[turn.phase] || turn.phase_name || t('debug.unknownPhase');

  // 正在流式输出的是「运行中的 functional_call（LLM 调用）」turn
  const isStreaming = turn.status_name === 'running' && turn.turn_type_name === 'functional_call';

  // 标题：类型 + 模型/工具
  const subject = turn.tool_name
    ? `${turn.server_name ? turn.server_name + '/' : ''}${turn.tool_name}`
    : turn.model || '';

  // 结束时间：running（未回填 end_ts）时用 … 表示仍在进行
  const endText = turn.end_ts > 0 ? formatTs(turn.end_ts) : '…';

  // 左边框用 phase 着色，徽标用 turn_type 着色 ——
  // 扫一眼纵向色带就能看出规划期到执行期的分界在哪。
  return (
    <div className="debug-turn" style={{ borderLeftColor: phaseColor }}>
      {/* 一行元信息：始终显示，点击可收起/展开整个 turn */}
      <div
        className="debug-turn-bar"
        onClick={onToggle}
        title={open ? t('debug.clickToCollapse') : t('debug.clickToExpand')}
      >
        <span className="debug-chevron">{open ? '▾' : '▸'}</span>
        <span className="debug-seq">#{turn.turn_seq}</span>
        <span
          className="debug-badge"
          style={{ color: phaseColor, borderColor: phaseColor }}
          title={t('debug.phaseTip')}
        >
          {phaseLabel}
        </span>
        <span
          className="debug-badge"
          style={{ color, borderColor: color }}
          title={t('debug.turnTypeTip')}
        >
          {TURN_TYPE_LABEL[turn.turn_type_name] || turn.turn_type_name}
        </span>
        {subject && <span className="debug-subject">{subject}</span>}
        <span className="debug-status" style={{ color: statusColor }}>●&nbsp;{turn.status_name}</span>
        <span className="debug-spacer" />
        <span className="debug-dim">{t('debug.round', { n: turn.round })}</span>
        <span className="debug-dim">{formatDuration(turn.time_cost_ms)}</span>
        {turn.first_token_ms > 0 && (
          <span className="debug-dim" title={t('debug.firstTokenTip')}>
            {t('debug.firstToken', { ms: formatDuration(turn.first_token_ms) })}
          </span>
        )}
        {turn.total_tokens > 0 && (
          <span
            className="debug-dim"
            title={t('debug.tokensInout', { in: fmtTok(turn.prompt_tokens), out: fmtTok(turn.completion_tokens) })}
          >
            {turn.total_tokens.toLocaleString()} tok
          </span>
        )}
        <span className="debug-dim debug-time-range">
          {formatTs(turn.start_ts)} ~ {endText}
        </span>
        {/* 该 turn 派生了子任务（典型：deep_search 子任务）时，行内显示调试入口，
            让调试时一眼对应到具体的 tool_call，不必再到顶部列表里找。
            点击需 stopPropagation，避免冒泡触发整个 turn-bar 的展开/折叠。 */}
        {turn.sub_task_id && (
          <a
            className="debug-turn-subtask"
            href={`/debug/${turn.sub_task_id}`}
            target="_blank"
            rel="noopener noreferrer"
            title={t('debug.openDerivedSubtaskNewTab')}
            onClick={(e) => e.stopPropagation()}
          >
            {t('debug.subtaskDebug')}
          </a>
        )}
      </div>

      {open && (
        <>
          {/* 错误优先展示 */}
          {turn.error_msg && (
            <div className="debug-block">
              <div className="debug-block-label" style={{ color: '#ef4444' }}>ERROR</div>
              <pre className="debug-pre debug-pre-error">{turn.error_msg}</pre>
            </div>
          )}

          {/* LLM 调用的输入分块占比：system / tools / 各历史段各占多少，
              一眼看出「这轮上下文被什么撑大了」 */}
          {turn.turn_type_name === 'functional_call' && turnCtxBlocks(payload) && (
            <ContextBlocksBlock
              blocks={turnCtxBlocks(payload)!}
              promptTokens={turn.prompt_tokens}
            />
          )}

          <RawBlock label="INPUT" raw={payload.input_raw} color="#5b8def" />

          {/* 模型正在流式输出时，实时展示已累积的文本。没有内容时给占位，
              方便判断「卡死」（占位迟迟不变）还是「输出慢」（文本持续增长）。 */}
          {isStreaming && (
            <div className="debug-block">
              <div className="debug-block-head">
                <span className="debug-block-label" style={{ color: '#22b8a6' }}>{t('debug.streamLive')}</span>
                <span className="debug-status" style={{ color: '#f59e0b' }}>● streaming</span>
                <span className="debug-spacer" />
                <span className="debug-dim">{t('debug.chars', { n: (streamText || '').length })}</span>
              </div>
              <pre className="debug-pre">{streamText || t('debug.waitingModelOutput')}</pre>
            </div>
          )}

          {/* 思考链（reasoning_content）实时输出：推理模型先吐思考再吐正文，
              这里是原始思考链，默认展开、可复制，与上面的正文 stream 分开。 */}
          {isStreaming && streamReasoning && (
            <RawBlock label={t('debug.reasoningLive')} raw={streamReasoning} color="#a855f7" defaultOpen />
          )}

          {/* 工具调用的 raw 流式输出：按 tool_call index 归并 arguments 分片，
              展示模型正在发起的函数名与尚未拼完的参数 JSON。 */}
          {isStreaming && streamToolCalls && Object.keys(streamToolCalls).length > 0 && (
            <div className="debug-block">
              <div className="debug-block-head">
                <span className="debug-block-label" style={{ color: '#f59e0b' }}>{t('debug.toolCallLive')}</span>
                <span className="debug-dim">{t('debug.toolCallLiveDesc')}</span>
              </div>
              {Object.entries(streamToolCalls)
                .sort(([a], [b]) => Number(a) - Number(b))
                .map(([idx, tc]) => (
                  <div key={idx} className="debug-tool-call">
                    <div className="debug-tool-call-head">
                      <span className="debug-seq">#{idx}</span>
                      <span className="debug-subject">{tc.name || t('debug.noFuncNameYet')}</span>
                      {tc.id && <span className="debug-dim">{tc.id}</span>}
                    </div>
                    <pre className="debug-pre">{tc.args || t('debug.waitingArgs')}</pre>
                  </div>
                ))}
            </div>
          )}

          <RawBlock label="OUTPUT" raw={payload.output_raw} color="#10b981" />
        </>
      )}
    </div>
  );
}

// ============================================================
// raw JSON 块：pretty-print + 折叠 + 复制
// ============================================================

function RawBlock({ label, raw, color, defaultOpen }: { label: string; raw?: string; color: string; defaultOpen?: boolean }) {
  const t = useT();
  // pretty-print：能解析就格式化，不能就原样
  const text = (() => {
    if (!raw) return '';
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  })();

  const lineCount = text ? text.split('\n').length : 0;
  const longContent = lineCount > COLLAPSE_LINE_THRESHOLD;

  // 超长默认折叠，短内容始终展开；defaultOpen 显式指定时以它为准（思考链默认展开）。
  const [open, setOpen] = useState(defaultOpen ?? !longContent);
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    } catch {
      /* 剪贴板不可用就算了，不打断调试 */
    }
  };

  return (
    <div className="debug-block">
      <div className="debug-block-head">
        <span className="debug-block-label" style={{ color }}>{label}</span>
        {text ? (
          <>
            <span className="debug-dim">{t('debug.lines', { n: lineCount })} · {t('debug.chars', { n: text.length })}</span>
            <span className="debug-spacer" />
            {longContent && (
              <button className="debug-mini-btn" onClick={() => setOpen(!open)}>
                {open ? t('common.collapse') : t('common.expand')}
              </button>
            )}
            <button className="debug-mini-btn" onClick={copy}>
              {copied ? t('common.copied') : t('common.copy')}
            </button>
          </>
        ) : (
          <span className="debug-dim">{t('common.empty')}</span>
        )}
      </div>

      {text && open && <pre className="debug-pre">{text}</pre>}
      {text && !open && (
        <pre className="debug-pre debug-pre-peek">{text.split('\n').slice(0, 8).join('\n')}
{'\n…'}</pre>
      )}
    </div>
  );
}

// ============================================================
// LLM 调用的输入分块占比
//
// 数据来自 turn input 快照里的 context_blocks（后端组装消息时实测计量），
// 回答「这轮上下文被什么撑大了」：system / tools / 各历史段各占多少。
// 老数据没有该字段时整块不渲染。
// ============================================================

interface TurnCtxBlocks extends ContextBlocksView { context_limit?: number }

function turnCtxBlocks(turn: TurnDebugView): TurnCtxBlocks | null {
  const input = turn.input as { context_blocks?: TurnCtxBlocks } | undefined;
  const b = input?.context_blocks;
  if (!b || typeof b !== 'object') return null;
  const allZero = !b.total && !b.system_prompt && !b.tools && !b.detail &&
    !b.compacted && !b.workflow_summary && !b.user_and_current;
  return allZero ? null : b;
}

function ContextBlocksBlock({ blocks, promptTokens }: { blocks: TurnCtxBlocks; promptTokens: number }) {
  const t = useT();
  const blockMeta = useBlockMeta();
  // 默认折叠：这是辅助信息（这轮上下文被什么撑大），不是每次都要看的核心内容，
  // 折叠态只留一行总量摘要，点「展开」再看进度条与分段占比。
  const [open, setOpen] = useState(false);
  const total = blocks.total || 0;
  const limit = blocks.context_limit || 0;
  const percent = limit > 0 ? (total / limit) * 100 : 0;
  const overTrigger = limit > 0 && total >= limit * 0.8;
  const segments = blockMeta
    .map((m) => ({ ...m, value: blocks[m.key] ?? 0 }))
    .filter((s) => s.value > 0);

  return (
    <div className="debug-block">
      <div className="debug-block-head">
        <span className="debug-block-label" style={{ color: '#5b8def' }}>{t('debug.ctxBlockRatio')}</span>
        <span className="debug-dim">
          {t('debug.ctxEst', { n: fmtTok(total) })}
          {limit > 0 ? t('debug.ctxLimit', { n: fmtTok(limit), percent: percent.toFixed(1) }) : ''}
          {promptTokens > 0 ? t('debug.ctxProvider', { n: fmtTok(promptTokens) }) : ''}
          {overTrigger ? t('debug.ctxOverTrigger') : ''}
        </span>
        <span className="debug-spacer" />
        <button className="debug-mini-btn" onClick={() => setOpen(!open)}>
          {open ? t('common.collapse') : t('common.expand')}
        </button>
      </div>

      {open && (
        <>
          {/* 分段彩色进度条（按占上限比例画，无上限时按占比归一化）+ 触发线刻度 */}
          <div className="ctx-usage-bar">
            {segments.map((s) => (
              <div
                key={s.key}
                className="ctx-usage-bar-seg"
                style={{ width: `${total > 0 ? (s.value / (limit || total)) * 100 : 0}%`, background: s.color }}
                title={`${s.label} ${fmtTok(s.value)}`}
              />
            ))}
            {limit > 0 && (
              <div
                className="ctx-usage-bar-trigger"
                style={{ left: '80%' }}
                title={t('debug.ctxTriggerLine')}
              />
            )}
          </div>

          <div className="ctx-usage-rows">
            {segments.map((s) => (
              <div key={s.key} className="ctx-usage-row" title={s.desc}>
                <span className="ctx-usage-dot" style={{ background: s.color }} />
                <span className="ctx-usage-label">{s.label}</span>
                <span className="ctx-usage-spacer" />
                <span className="ctx-usage-val">
                  {total > 0 ? `${((s.value / total) * 100).toFixed(1)}%` : '-'}
                </span>
                <span className="ctx-usage-tok">{fmtTok(s.value)}</span>
              </div>
            ))}
          </div>
        </>
      )}
    </div>
  );
}

// ============================================================
// utils
// ============================================================

function formatTs(ts: number): string {
  if (!ts) return '-';
  const d = new Date(ts);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

// 任务面板用：带月日，跨天任务能分辨。按当前界面语言选 locale。
function formatDateTime(ts: number, lang: 'zh' | 'en'): string {
  if (!ts) return '-';
  const locale = lang === 'zh' ? 'zh-CN' : 'en-US';
  const d = new Date(ts);
  // 月/日/时/分/秒用同一套数字格式，仅分隔与顺序交给 locale
  return d.toLocaleString(locale, {
    month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  }).replace(/\//g, '-');
}

function formatDuration(ms: number): string {
  if (!ms || ms < 0) return '-';
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(2)}s`;
  const m = Math.floor(s / 60);
  return `${m}m ${(s - m * 60).toFixed(1)}s`;
}

/** 千分位格式化 token；undefined / 0 都显示 0（老数据缺列时的兜底） */
function fmtTok(n?: number): string {
  return (n ?? 0).toLocaleString();
}

// ============================================================
// 样式
// ============================================================

const DEBUG_PAGE_STYLE = `
/* 唯一滚动容器 —— 不要再往里加 overflow。
   父级 .main-area 是 flex column（height 由 .app-layout 的 100vh 给定），
   所以这里必须用 flex:1 + min-height:0 而不是 height:100vh，
   否则 flex 收缩规则会让内容溢出、滚不到底。 */
.debug-page {
  flex: 1 1 auto;
  min-height: 0;
  overflow-y: auto;
  background: var(--bg-primary);
  color: var(--text-primary);
}

.debug-header {
  position: sticky;
  top: 0;
  z-index: 10;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  /* 右侧给全局偏好控件（语言/主题）预留空间，避免遮挡跳转/展开按钮 */
  padding: 12px 180px 12px 20px;
  background: var(--bg-secondary);
  border-bottom: 1px solid var(--border);
}
.debug-header-left { display: flex; align-items: center; gap: 12px; min-width: 0; }
.debug-header-left h2 { margin: 0; font-size: 15px; font-weight: 600; white-space: nowrap; }
.debug-header-left code {
  font-size: 11px;
  color: var(--text-muted);
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
}

.debug-btn {
  padding: 5px 14px;
  font-size: 12px;
  border-radius: 5px;
  border: 1px solid var(--border);
  background: var(--bg-tertiary);
  color: var(--text-primary);
  cursor: pointer;
  flex-shrink: 0;
}
.debug-btn:hover:not(:disabled) { background: var(--bg-hover); }
.debug-btn:disabled { opacity: 0.5; cursor: default; }
.debug-header-right { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }

.debug-body { padding: 16px 20px 60px; display: flex; flex-direction: column; gap: 14px; }

.debug-error {
  padding: 10px 14px;
  border-radius: 6px;
  background: rgba(239, 68, 68, 0.12);
  color: #ef4444;
  font-size: 13px;
}
.debug-empty {
  padding: 60px 0;
  text-align: center;
  color: var(--text-muted);
  font-size: 13px;
}

/* ---- 任务基础信息 ---- */
.debug-task {
  background: var(--bg-secondary);
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.debug-task-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  gap: 8px 18px;
}
.debug-field { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
.debug-field-label {
  font-size: 10.5px;
  color: var(--text-muted);
  letter-spacing: 0.6px;
  text-transform: uppercase;
}
.debug-field-value {
  font-size: 12.5px;
  color: var(--text-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.debug-field-mono {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 11px;
  color: var(--text-secondary);
}
.debug-task-io { display: flex; flex-direction: column; gap: 5px; }
/* 不设 max-height —— 保持整页单一滚动容器 */
.debug-task-text {
  padding: 9px 11px;
  background: var(--bg-tertiary);
  border: 1px solid var(--border);
  border-radius: 4px;
  font-size: 12.5px;
  line-height: 1.6;
  color: var(--text-primary);
  white-space: pre-wrap;
  word-break: break-word;
  overflow-wrap: anywhere;
}
.debug-task-text-peek { opacity: 0.65; }

.debug-turn {
  background: var(--bg-secondary);
  border: 1px solid var(--border);
  border-left: 3px solid #5b8def;
  border-radius: 6px;
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.debug-turn-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  font-size: 12px;
  padding-bottom: 10px;
  border-bottom: 1px solid var(--border);
  cursor: pointer;
  user-select: none;
}
.debug-turn-bar:hover .debug-chevron,
.debug-turn-bar:hover .debug-seq {
  color: var(--text-primary);
}
.debug-chevron {
  font-size: 10px;
  color: var(--text-muted);
  width: 12px;
  text-align: center;
  flex-shrink: 0;
}
.debug-seq {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-weight: 600;
  color: var(--text-muted);
}
.debug-badge {
  padding: 1px 7px;
  font-size: 11px;
  font-weight: 600;
  border: 1px solid currentColor;
  border-radius: 4px;
  white-space: nowrap;
  flex-shrink: 0;
}
.debug-subject {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  color: var(--text-primary);
  font-size: 11.5px;
}
.debug-status { font-size: 11px; }
.debug-spacer { flex: 1 1 auto; }
.debug-dim { color: var(--text-muted); font-size: 11px; white-space: nowrap; }
.debug-time-range {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  letter-spacing: 0.2px;
}

.debug-block { display: flex; flex-direction: column; gap: 6px; }

/* 任务摘要里「子任务贡献的 token」角标。
   deep_search 这类派发子任务的工具消耗经常远超主循环本身，
   不给个视觉标记，用户只会看到一个总数，意识不到大头在检索上。 */
.debug-tok-sub {
  margin-left: 4px;
  padding: 0 4px;
  border: 1px solid var(--border);
  border-radius: 3px;
  color: var(--text-muted);
  font-size: 10px;
}
.debug-block-head { display: flex; align-items: center; gap: 10px; }
.debug-block-label {
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 1px;
}
.debug-mini-btn {
  padding: 2px 9px;
  font-size: 11px;
  border-radius: 4px;
  border: 1px solid var(--border);
  background: transparent;
  color: var(--text-secondary);
  cursor: pointer;
}
.debug-mini-btn:hover { background: var(--bg-hover); color: var(--text-primary); }

/* 不设 max-height —— 内容完整铺开，靠页面滚动看到底 */
.debug-pre {
  margin: 0;
  padding: 10px 12px;
  background: var(--bg-tertiary);
  border: 1px solid var(--border);
  border-radius: 4px;
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 12px;
  line-height: 1.55;
  color: var(--text-primary);
  white-space: pre-wrap;
  word-break: break-word;
  overflow-wrap: anywhere;
}
.debug-pre-peek { opacity: 0.65; }
.debug-pre-error {
  background: rgba(239, 68, 68, 0.1);
  border-color: rgba(239, 68, 68, 0.4);
  color: #f87171;
}

/* 工具调用流式块：每个 tool_call index 一个卡片，函数名 + raw 参数累积 */
.debug-tool-call {
  display: flex;
  flex-direction: column;
  gap: 5px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-left: 3px solid #f59e0b;
  border-radius: 4px;
  background: var(--bg-tertiary);
}
.debug-tool-call-head {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 12px;
}

/* 子任务 section —— 父任务调试页专用入口列表已移除。
   子任务入口现在直接挂在对应的 turn 行（turn.sub_task_id → 「子任务调试」链接）。 */

/* turn 行的子任务调试入口：与原来 .debug-subtask 视觉一致（深绿色系），
   让用户在父任务调试页一眼能找到 deep_search 子任务的对应 turn。 */
.debug-turn-subtask {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 1px 8px;
  margin-left: 6px;
  font-size: 11px;
  font-weight: 600;
  color: #22b8a6;
  border: 1px solid #22b8a6;
  border-radius: 3px;
  background: transparent;
  text-decoration: none;
  white-space: nowrap;
  transition: background 0.12s, color 0.12s;
}
.debug-turn-subtask:hover {
  background: #22b8a6;
  color: #fff;
}

/* 未映射子任务兜底列表 —— 旧数据（子任务无 parent_turn_id）时出现，
   视觉与 turn 行的「子任务调试」入口同色系，让父任务调试页一眼能认出子任务入口。 */
.debug-subtasks {
  border: 1px solid rgba(34, 184, 166, 0.35);
  border-radius: 6px;
  background: rgba(34, 184, 166, 0.04);
  padding: 8px 10px;
  margin-bottom: 12px;
}
.debug-subtasks-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.debug-subtask-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 5px 8px;
  font-size: 12px;
  color: inherit;
  text-decoration: none;
  border-radius: 4px;
}
.debug-subtask-item:hover {
  background: rgba(34, 184, 166, 0.10);
}
.debug-subtask-status {
  white-space: nowrap;
  font-weight: 600;
}
.debug-subtask-kind {
  padding: 0 6px;
  font-size: 11px;
  color: #22b8a6;
  border: 1px solid #22b8a6;
  border-radius: 3px;
  white-space: nowrap;
}
.debug-subtask-text {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: #cbd5e1;
}
.debug-subtask-open {
  flex-shrink: 0;
  font-size: 11px;
  font-weight: 600;
  color: #22b8a6;
}
`;

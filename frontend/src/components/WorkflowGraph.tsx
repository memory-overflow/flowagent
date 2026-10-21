import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  ReactFlow,
  Background,
  BackgroundVariant,
  Controls,
  MiniMap,
  Handle,
  Position,
  useNodesState,
  useEdgesState,
  MarkerType,
  type Node,
  type Edge,
  type NodeProps,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import type { ConversationWorkflow, WorkflowNode, TaskStatus } from '../types/api';
import Markdown from './Markdown';
import ToolCalls from './ToolCalls';
import { fmtTokens } from './ContextUsage';
import DurationText, { useDurationTooltip } from './DurationText';
import { useT, type Translate } from '../i18n';

// ============================================================
// 状态映射（后端 model.TaskStatus → 展示文案/样式）
// ============================================================

/** 状态 → 词典 key（status.* 命名空间），展示文案由 t 翻译 */
const STATUS_KEY: Record<number, string> = {
  1: 'pending',
  2: 'running',
  3: 'completed',
  4: 'failed',
  5: 'cancelled',
  6: 'partialFailed',
  // 7 已废弃：上游失败导致本节点永不执行。仅用于 DAG 节点。
  7: 'abandoned',
};

const STATUS_CLASS: Record<number, string> = {
  1: 'pending',
  2: 'running',
  3: 'completed',
  4: 'failed',
  5: 'cancelled',
  6: 'failed',
  7: 'archived',
};

/** 供外部按 TaskStatus 取展示文案，避免各处重复维护映射 */
export function taskStatusLabel(t: Translate, s: TaskStatus): string {
  const key = STATUS_KEY[s];
  return key ? t(`status.${key}`) : t('common.unknown');
}

// ============================================================
// 布局常量。React Flow 只消费节点 position（左上角），
// 画布尺寸交给 fitView 自动适配，无需手算总宽高。
// ============================================================

const NODE_W = 210;
const NODE_H = 96;
const GAP_X = 56;
const GAP_Y = 84;

// ============================================================
// 数据转换：后端 WorkflowNode[] → React Flow Node/Edge
// ============================================================

type WorkflowNodeData = Record<string, unknown> & {
  name: string;
  description: string;
  status: number;
  statusLabel: string;
  statusClass: string;
  /** 已归档（失败分支残留）：灰显 + 虚线边框，不再参与执行 */
  archived: boolean;
  /** 归档原因，节点卡片 title 与详情面板展示 */
  archiveReason?: string;
};

type WFNode = Node<WorkflowNodeData, 'workflow'>;

/** 拓扑分层：一个节点的层级 = 其所有依赖的最大层级 + 1。环/脏依赖也要能出图 */
function layoutPositions(nodes: WorkflowNode[]): Map<string, { x: number; y: number }> {
  const pos = new Map<string, { x: number; y: number }>();
  if (nodes.length === 0) return pos;

  const known = new Set(nodes.map((n) => n.node_id));
  const placed = new Set<string>();
  const levels: WorkflowNode[][] = [];
  let remaining = nodes.slice();

  while (remaining.length > 0) {
    const ready = remaining.filter((n) =>
      (n.depends_on || []).every((d) => !known.has(d) || placed.has(d)),
    );
    const current = ready.length > 0 ? ready : remaining;
    current.forEach((n) => placed.add(n.node_id));
    levels.push(current.slice());
    const currentIds = new Set(current.map((n) => n.node_id));
    remaining = remaining.filter((n) => !currentIds.has(n.node_id));
  }

  // 交叉最小化：每层内节点的左右顺序不能沿用后端原始顺序（否则边会乱穿），
  // 用 barycenter 重心法让有依赖关系的节点尽量对齐，减少边与边、边与节点的交叉。
  const ordered = minimizeCrossings(levels, nodes);

  const maxPerRow = Math.max(...ordered.map((l) => l.length));
  const totalW = maxPerRow * NODE_W + (maxPerRow - 1) * GAP_X;

  ordered.forEach((level, row) => {
    const rowW = level.length * NODE_W + (level.length - 1) * GAP_X;
    const startX = (totalW - rowW) / 2;
    level.forEach((nodeId, col) => {
      pos.set(nodeId, {
        x: startX + col * (NODE_W + GAP_X),
        y: row * (NODE_H + GAP_Y),
      });
    });
  });

  return pos;
}

/**
 * 重心法（barycenter）层内排序：把该层节点按「相邻层邻居位置的平均值」升序排列。
 * 无邻居的节点用其当前下标作为兜底重心，保持相对顺序稳定。
 */
function barycenterOrder(
  ids: string[],
  neighborPos: Map<string, number>,
  getNeighbors: (id: string) => string[],
): string[] {
  const items = ids.map((id, idx) => {
    const ns = getNeighbors(id).filter((n) => neighborPos.has(n));
    const b = ns.length === 0 ? idx : ns.reduce((s, n) => s + neighborPos.get(n)!, 0) / ns.length;
    return { id, b, idx };
  });
  items.sort((a, b) => a.b - b.b || a.idx - b.idx);
  return items.map((i) => i.id);
}

/**
 * 层间交叉最小化：自上而下 + 自下而上交替 sweep，逐层用 barycenter 重排。
 * 迭代若干轮后各层顺序收敛，交叉数量显著下降。
 */
function minimizeCrossings(levels: WorkflowNode[][], nodes: WorkflowNode[]): string[][] {
  const byId = new Map(nodes.map((n) => [n.node_id, n]));
  // 前驱（依赖）集合：仅保留本工作流内真实存在的节点
  const preds = (id: string): string[] => {
    const n = byId.get(id);
    return (n?.depends_on || []).filter((d) => byId.has(d));
  };
  // 后继集合：谁依赖了 id
  const succMap = new Map<string, string[]>();
  for (const n of nodes) {
    for (const d of n.depends_on || []) {
      if (!byId.has(d)) continue;
      const arr = succMap.get(d) || [];
      arr.push(n.node_id);
      succMap.set(d, arr);
    }
  }
  const succ = (id: string): string[] => succMap.get(id) || [];

  const layers = levels.map((l) => l.map((n) => n.node_id));

  for (let iter = 0; iter < 4; iter++) {
    // 自上而下：第 i 层按第 i-1 层的前驱位置排序
    for (let i = 1; i < layers.length; i++) {
      const order = new Map(layers[i - 1].map((id, j) => [id, j]));
      layers[i] = barycenterOrder(layers[i], order, preds);
    }
    // 自下而上：第 i 层按第 i+1 层的后继位置排序
    for (let i = layers.length - 2; i >= 0; i--) {
      const order = new Map(layers[i + 1].map((id, j) => [id, j]));
      layers[i] = barycenterOrder(layers[i], order, succ);
    }
  }

  return layers;
}

// ============================================================
// 节点位置持久化：刷新后若工作流结构未变，保留用户拖拽的布局；
// 结构（节点集合 / 依赖关系）变化时才重新自动布局。
// ============================================================

/** localStorage 键前缀，v1 预留格式升级空间，按 task_id 隔离 */
const LAYOUT_STORAGE_PREFIX = 'wf-layout:v1:';

/** 工作流结构指纹：只对节点集合与依赖关系敏感，对状态/输出/产物不敏感 */
function structureKey(nodes: WorkflowNode[]): string {
  return nodes
    .map((n) => `${n.node_id}:[${(n.depends_on || []).slice().sort().join(',')}]`)
    .sort()
    .join('|');
}

interface SavedLayout {
  /** 保存该布局时的工作流结构指纹，用于判断结构是否已变化 */
  key: string;
  positions: Record<string, { x: number; y: number }>;
}

/** 读取保存的布局：结构指纹一致才返回位置，否则视为结构已变返回 null */
function loadSavedLayout(
  taskId: string,
  key: string,
): Record<string, { x: number; y: number }> | null {
  try {
    const raw = localStorage.getItem(`${LAYOUT_STORAGE_PREFIX}${taskId}`);
    if (!raw) return null;
    const saved = JSON.parse(raw) as SavedLayout;
    if (!saved || saved.key !== key || !saved.positions) return null;
    return saved.positions;
  } catch {
    return null;
  }
}

/** 保存当前布局（拖拽结束后调用），带结构指纹供下次刷新比对 */
function saveLayout(
  taskId: string,
  key: string,
  positions: Record<string, { x: number; y: number }>,
): void {
  try {
    localStorage.setItem(
      `${LAYOUT_STORAGE_PREFIX}${taskId}`,
      JSON.stringify({ key, positions }),
    );
  } catch {
    /* localStorage 满/不可用时忽略，不影响主流程 */
  }
}

function buildGraph(
  t: Translate,
  workflow: ConversationWorkflow,
  savedPositions?: Record<string, { x: number; y: number }> | null,
): { nodes: WFNode[]; edges: Edge[] } {
  const srcNodes = workflow.nodes || [];
  const pos = layoutPositions(srcNodes);

  const archived = new Set(srcNodes.filter((n) => n.archived).map((n) => n.node_id));

  const nodes: WFNode[] = srcNodes.map((n) => ({
    id: n.node_id,
    type: 'workflow',
    position: savedPositions?.[n.node_id] || pos.get(n.node_id) || { x: 0, y: 0 },
    data: {
      name: n.name || n.node_id,
      description: n.description || '',
      status: n.status,
      statusLabel: n.archived ? t('status.abandoned') : taskStatusLabel(t, n.status),
      statusClass: n.archived ? 'archived' : STATUS_CLASS[n.status] || 'pending',
      archived: !!n.archived,
      archiveReason: n.archive_reason,
    },
  }));

  // 依赖边：depId(前驱) → 当前节点。depends_on 引用不存在的节点时
  // React Flow 会自动忽略该边，不会白屏。
  //
  // 只要有一端已归档就把边画成灰色虚线：这条依赖属于被废弃的历史分支，
  // 实线会让人误以为它还在生效。
  const edges: Edge[] = srcNodes.flatMap((n) =>
    (n.depends_on || []).map((depId) => {
      const dead = archived.has(depId) || archived.has(n.node_id);
      return {
        id: `${depId}->${n.node_id}`,
        source: depId,
        target: n.node_id,
        type: 'smoothstep',
        className: dead ? 'is-archived' : undefined,
        style: dead
          ? { stroke: 'var(--text-muted)', strokeDasharray: '5 4', strokeWidth: 1.2 }
          : undefined,
        markerEnd: { type: MarkerType.ArrowClosed, width: 18, height: 18 },
      };
    }),
  );

  return { nodes, edges };
}

// ============================================================
// 自定义节点卡片
// ============================================================

function WorkflowNodeCard({ data }: NodeProps<WFNode>) {
  const t = useT();
  const archived = !!data.archived;
  return (
    <div
      className={`wf-node${data.status === 2 && !archived ? ' is-running' : ''}${archived ? ' is-archived' : ''}`}
      title={archived ? data.archiveReason || t('workflow.nodeArchivedTip') : undefined}
    >
      <Handle type="target" position={Position.Top} className="wf-handle" />
      <div className="wf-node-name">
        {archived && <span className="wf-node-archived-mark" aria-hidden>⌀ </span>}
        {data.name}
      </div>
      {data.description && <div className="wf-node-desc">{data.description}</div>}
      <div className={`wf-node-status status-dot ${data.statusClass}`}>
        {data.statusLabel}
      </div>
      <Handle type="source" position={Position.Bottom} className="wf-handle" />
    </div>
  );
}

/** 模块级常量：节点类型映射只需定义一次，避免每次渲染重建导致 React Flow 反复卸载 */
const nodeTypes = { workflow: WorkflowNodeCard };

// ============================================================
// 主组件：React Flow 画布
// ============================================================

interface WorkflowGraphProps {
  workflow: ConversationWorkflow;
  /** 当前选中的节点 ID（用于详情面板联动），点击画布空白传 null */
  onSelect: (nodeId: string | null) => void;
}

export default function WorkflowGraph({ workflow, onSelect }: WorkflowGraphProps) {
  const t = useT();
  const [rfNodes, setRfNodes, onNodesChange] = useNodesState<WFNode>([]);
  const [rfEdges, setRfEdges, onEdgesChange] = useEdgesState<Edge>([]);

  const taskId = workflow.task_id;
  // 结构指纹：节点集合或依赖关系变化时才不同，状态/输出更新不改变它
  const key = useMemo(() => structureKey(workflow.nodes || []), [workflow.nodes]);

  // 已渲染结构指纹：与 key 一致说明「结构没变」（仅状态/进度轮询更新），
  // 此时跳过重建、保留用户拖拽后的位置，避免 5 秒轮询把布局还原。
  const renderedKeyRef = useRef<string | null>(null);
  // 当前结构下所有节点的完整位置快照：每次拖拽结束累积被拖节点的位置，
  // 供持久化时整体写入（React Flow 的 onNodeDragStop 回调只给被拖节点，不给全集）。
  const positionsRef = useRef<Record<string, { x: number; y: number }>>({});

  useEffect(() => {
    const savedPositions = loadSavedLayout(taskId, key);
    const { nodes, edges } = buildGraph(t, workflow, savedPositions);

    if (renderedKeyRef.current === key) {
      // 结构未变：仅节点状态/进度变化，保留用户拖拽后的位置，只刷新 data。
      setRfNodes((prev) => {
        const prevPos = new Map(prev.map((n) => [n.id, n.position]));
        return nodes.map((n) => ({
          ...n,
          position: prevPos.get(n.id) || positionsRef.current[n.id] || n.position,
        }));
      });
      setRfEdges(edges);
      return;
    }

    // 结构变化（或首次加载）：重建图，用 localStorage 恢复（结构一致时）的拖拽位置。
    setRfNodes(nodes);
    setRfEdges(edges);
    renderedKeyRef.current = key;
    const pos: Record<string, { x: number; y: number }> = {};
    for (const n of nodes) pos[n.id] = { x: n.position.x, y: n.position.y };
    positionsRef.current = pos;
  }, [workflow, taskId, key, setRfNodes, setRfEdges, t]);

  // 拖拽结束后把完整布局落 localStorage，供下次刷新按结构指纹恢复。
  // 注意：onNodeDragStop 的 nodes 参数只包含本次被拖拽的节点（React Flow 的
  // dragItems），这里只更新这些节点到快照，其余沿用 positionsRef 累积的位置。
  const handleNodeDragStop = useCallback(
    (_event: MouseEvent | TouchEvent, _node: WFNode, nodes: WFNode[]) => {
      for (const n of nodes) {
        positionsRef.current[n.id] = { x: n.position.x, y: n.position.y };
      }
      saveLayout(taskId, key, positionsRef.current);
    },
    [taskId, key],
  );

  // 进度只看未归档节点：已归档的失败分支永远跑不完，算进分母
  // 会让「修复后的失败工作流」进度永远到不了 100%。
  const { doneCount, liveCount, archivedCount } = useMemo(() => {
    const all = workflow.nodes || [];
    const live = all.filter((n) => !n.archived);
    return {
      doneCount: live.filter((n) => n.status === 3).length,
      liveCount: live.length,
      archivedCount: all.length - live.length,
    };
  }, [workflow.nodes]);

  return (
    <div className="wf-graph">
      <div className="wf-graph-statusbar">
        <span className={`status-dot ${STATUS_CLASS[workflow.task_status]}`}>
          {taskStatusLabel(t, workflow.task_status)}
        </span>
        <span className="wf-graph-progress">
          {t('workflow.stepsDone', { done: doneCount, total: liveCount })}
        </span>
        {archivedCount > 0 && (
          <span
            className="wf-graph-archived"
            title={t('workflow.archivedTipGraph', { n: archivedCount })}
          >
            ⌀ {t('workflow.archivedCount', { n: archivedCount })}
          </span>
        )}
      </div>

      <ReactFlow
        nodes={rfNodes}
        edges={rfEdges}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        nodeTypes={nodeTypes}
        onNodeClick={(_, node) => onSelect(node.id)}
        onPaneClick={() => onSelect(null)}
        onNodeDragStop={handleNodeDragStop}
        fitView
        fitViewOptions={{ padding: 0.25, maxZoom: 1 }}
        minZoom={0.15}
        maxZoom={2}
        nodesDraggable
        nodesConnectable={false}
        elementsSelectable
        deleteKeyCode={null}
        proOptions={{ hideAttribution: true }}
      >
        <Background variant={BackgroundVariant.Dots} gap={22} size={1.2} />
        <Controls position="bottom-left" />
        <MiniMap
          position="bottom-right"
          pannable
          zoomable
          nodeColor={(n) => {
            const data = n.data as WorkflowNodeData | undefined;
            if (data?.archived) return 'var(--border-light)'; // 已归档：灰，与画布上的灰显一致
            switch (data?.status) {
              case 3: return 'var(--success)';
              case 2: return 'var(--accent)';
              case 4: case 6: return 'var(--danger)';
              default: return 'var(--text-muted)';
            }
          }}
          maskColor="var(--wf-minimap-mask)"
        />
      </ReactFlow>
    </div>
  );
}

// ============================================================
// 节点详情面板（独立页面右侧抽屉）
// ============================================================

interface WorkflowNodeDetailProps {
  node: WorkflowNode;
  nodes: WorkflowNode[];
  onClose: () => void;
  /** 运行中子任务的实时思考过程（node_id -> 步骤列表），执行中 node.output 尚未写入时兜底展示 */
  liveThink?: Record<string, string[]>;
}

export function WorkflowNodeDetail({ node, nodes, onClose, liveThink }: WorkflowNodeDetailProps) {
  const t = useT();
  const durationTip = useDurationTooltip();
  const nameOf = (id: string) => nodes.find((n) => n.node_id === id)?.name || id;
  const deps = node.depends_on || [];
  const dependents = nodes.filter((n) => (n.depends_on || []).includes(node.node_id));
  // 终态思考过程优先（落库最完整）；执行中退回实时重建的步骤
  const finalThink = node.output?.think_process?.length
    ? node.output.think_process
    : liveThink?.[node.node_id] || [];
  const isLive = finalThink.length > 0 && !node.output?.think_process?.length;
  // 思考过程默认展开 —— 子任务的中间过程正是排查该节点执行情况的关键
  const [showThink, setShowThink] = useState(true);

  return (
    <div className="wf-detail">
      <div className="wf-detail-header">
        <span className="wf-detail-title">
          {node.archived && <span className="wf-node-archived-mark" aria-hidden>⌀ </span>}
          {node.name || node.node_id}
        </span>
        <span
          className={`status-dot ${node.archived ? 'archived' : STATUS_CLASS[node.status]}`}
        >
          {node.archived ? t('status.abandoned') : taskStatusLabel(t, node.status)}
        </span>
        <button type="button" className="wf-detail-close" onClick={onClose} title={t('common.close')}>
          ×
        </button>
      </div>

      <dl className="wf-detail-grid">
        <dt>{t('workflow.nodeId')}</dt>
        <dd>
          <code>{node.node_id}</code>
        </dd>

        {node.description && (
          <>
            <dt>{t('workflow.nodeDesc')}</dt>
            <dd>{node.description}</dd>
          </>
        )}

        {/* 归档原因：解释这个节点为什么不再执行。
            用户看到灰显节点时第一个问题就是「它怎么不跑了」，这里直接回答。 */}
        {node.archived && (
          <>
            <dt>{t('status.abandoned')}</dt>
            <dd className="wf-detail-archived">
              {node.archive_reason || t('workflow.archiveReasonFallback')}
            </dd>
          </>
        )}

        {node.input?.input_text && (
          <>
            <dt>{t('workflow.input')}</dt>
            <dd className="wf-detail-pre">{node.input.input_text}</dd>
          </>
        )}

        <dt>{t('workflow.deps')}</dt>
        <dd>
          {deps.length === 0
            ? t('workflow.noDeps')
            : deps.map((d) => (
                <code key={d} className="wf-detail-chip">
                  {nameOf(d)}
                </code>
              ))}
        </dd>

        {dependents.length > 0 && (
          <>
            <dt>{t('workflow.dependents')}</dt>
            <dd>
              {dependents.map((d) => (
                <code key={d.node_id} className="wf-detail-chip">
                  {d.name || d.node_id}
                </code>
              ))}
            </dd>
          </>
        )}

        {node.task_id && (
          <>
            <dt>{t('workflow.subTask')}</dt>
            <dd>
              <a href={`/debug/${node.task_id}`} target="_blank" rel="noreferrer">
                {node.task_id} 🐞
              </a>
            </dd>
          </>
        )}

        {/* 执行时长含它派生的 deep_search 子任务（孙层），与 Token 同口径 */}
        {node.duration && (
          <>
            <dt>{t('workflow.execDuration')}</dt>
            <dd>
              <DurationText
                duration={node.duration}
                title={durationTip(t('workflow.durationNode'), node.duration)}
              />
            </dd>
          </>
        )}

        {/* 节点消耗含它派生的 deep_search 子任务 —— 那部分通常才是大头 */}
        {node.token_usage && (
          <>
            <dt>{t('workflow.tokenUsage')}</dt>
            <dd>
              {fmtTokens(node.token_usage.total_tokens)}
              <span className="wf-detail-tok-sub">
                {t('workflow.tokenInOut', {
                  input: fmtTokens(node.token_usage.prompt_tokens),
                  output: fmtTokens(node.token_usage.completion_tokens),
                })}
              </span>
            </dd>
          </>
        )}

        {node.source_task_id && (
          <>
            <dt>{t('workflow.planSource')}</dt>
            <dd>
              <a href={`/debug/${node.source_task_id}`} target="_blank" rel="noreferrer">
                {node.source_task_id} 🐞
              </a>
            </dd>
          </>
        )}
      </dl>

      {node.output?.reply_text && (
        <div className="wf-detail-output">
          <div className="wf-detail-output-label">{t('workflow.output')}</div>
          <Markdown compact>{node.output.reply_text}</Markdown>
        </div>
      )}

      {!!node.output?.tool_calls?.length && (
        <div className="wf-detail-output wf-detail-tool-calls">
          <div className="wf-detail-output-label">
            {t('workflow.toolCalls', { n: node.output.tool_calls.length })}
          </div>
          <ToolCalls calls={node.output.tool_calls} />
        </div>
      )}

      {finalThink.length > 0 && (
        <div className="wf-detail-output">
          <div className="wf-detail-output-label">
            {t('workflow.thinkSteps', { n: finalThink.length })}
            {isLive ? t('workflow.liveSuffix') : ''}
          </div>
          <div
            className="think-title"
            onClick={() => setShowThink(!showThink)}
            style={{ marginTop: 6, cursor: 'pointer', userSelect: 'none' }}
          >
            {showThink ? `▾ ${t('common.collapse')}` : `▸ ${t('common.expand')}`}
          </div>
          {showThink && (
            <div className="think-process" style={{ marginTop: 4, paddingLeft: 10 }}>
              {finalThink.map((t, i) => (
                <div key={i} className="think-item">
                  <Markdown compact>{t}</Markdown>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {finalThink.length === 0 && node.status === 2 && (
        <div className="wf-detail-output">
          <div className="wf-detail-output-label">{t('workflow.thinkLive')}</div>
          <div className="wf-detail-pre" style={{ color: 'var(--text-dim)', fontSize: 13 }}>
            {t('workflow.waitingModel')}
          </div>
        </div>
      )}
    </div>
  );
}

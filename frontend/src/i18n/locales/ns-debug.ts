// debug 页面/组件的文案。zh 与 en 成对维护（见同目录 index.ts 的说明）。
// 只收前端写死的 UI 文案；后端返回的 turn_type_name / phase_name / status_name、
// 模型 ID、错误码、时间戳等原样展示，不进词典。
export const debugNS = {
  zh: {
    // ---- 页面标题与顶部工具栏 ----
    title: '任务调试',
    backToParent: '返回父任务调试页',
    parentTask: '← 父任务',
    top: '顶部',
    topTip: '滚动到最前',
    bottom: '底部',
    bottomTip: '滚动到最后',
    toggleAllTip: '一键折叠/展开所有 turn',
    expandAll: '一键展开',
    collapseAll: '一键折叠',
    loadFailed: '加载失败',
    noTurns: '暂无 turn 记录',

    // ---- 枚举标签：前端按枚举重新映射（后端字段名/phase_name/status_name 不翻译）----
    turnType: {
      contextCompact: '上下文压缩',
    },
    phase: {
      planning: '规划期',
      execution: '执行期',
    },

    // ---- 任务基础信息字段 ----
    workflowTask: '工作流任务',
    chatTask: '对话任务',
    progress: '进度',
    duration: '耗时',
    created: '创建',
    parent: '父任务',
    failReason: '失败原因',
    tokensInout: '输入 {in} / 输出 {out}',
    tokensBreakdown: '本任务 {own} + 子任务 {sub}',
    tokensDeepSearch: '（其中 deep_search {n}）',

    // ---- 未映射子任务兜底列表 ----
    unmappedSubtasks: '子任务（未映射到 turn）',
    count: '{n} 个',
    turnsCount: '{n} turns',
    openSubtaskNewTab: '在新标签页打开子任务调试页',
    openDerivedSubtaskNewTab: '在新标签页打开该 turn 派生的子任务调试页',
    noInput: '(无输入)',
    open: '打开 →',

    // ---- 用户输入块 ----
    userInput: '用户输入',
    lines: '{n} 行',
    chars: '{n} 字符',

    // ---- 单条 turn ----
    round: '轮次 {n}',
    firstToken: '首 token {ms}',
    firstTokenTip: '从发起请求到收到首个内容增量的耗时（仅流式 LLM 调用）',
    unknownPhase: '未知阶段',
    clickToCollapse: '点击收起整个 turn',
    clickToExpand: '点击展开整个 turn',
    phaseTip: '语义阶段：任务此刻在规划还是在执行',
    turnTypeTip: '调用类型：这一次调的是 LLM 还是工具',
    subtaskDebug: '🔍 子任务调试 →',

    // ---- 流式输出块 ----
    streamLive: 'STREAM · 实时输出',
    waitingModelOutput: '（等待模型输出…）',
    reasoningLive: '思考链 · 实时（reasoning_content）',
    toolCallLive: 'TOOL CALL · 实时',
    toolCallLiveDesc: '模型发起的工具调用 raw 分片累积',
    noFuncNameYet: '（尚未收到函数名）',
    waitingArgs: '（等待参数…）',

    // ---- LLM 输入上下文分块占比 ----
    ctxBlockRatio: '上下文分块占比',
    ctxEst: '估算 {n} tok',
    ctxLimit: ' / 上限 {n}（{percent}%）',
    ctxProvider: ' · provider 实测 {n}',
    ctxOverTrigger: ' · 已过压缩触发线',
    ctxTriggerLine: '压缩触发线（上限的 80%）',
  },

  en: {
    // ---- Page title & top toolbar ----
    title: 'Task debug',
    backToParent: 'Back to parent task debug',
    parentTask: '← Parent',
    top: 'Top',
    topTip: 'Scroll to top',
    bottom: 'Bottom',
    bottomTip: 'Scroll to bottom',
    toggleAllTip: 'Expand/collapse all turns',
    expandAll: 'Expand all',
    collapseAll: 'Collapse all',
    loadFailed: 'Failed to load',
    noTurns: 'No turn records',

    // ---- Enum labels: remapped on the frontend (backend field names/phase_name/status_name untouched) ----
    turnType: {
      contextCompact: 'Context compaction',
    },
    phase: {
      planning: 'Planning',
      execution: 'Execution',
    },

    // ---- Task info fields ----
    workflowTask: 'Workflow task',
    chatTask: 'Chat task',
    progress: 'Progress',
    duration: 'Duration',
    created: 'Created',
    parent: 'Parent',
    failReason: 'Failure reason',
    tokensInout: 'Input {in} / Output {out}',
    tokensBreakdown: 'This task {own} + subtasks {sub}',
    tokensDeepSearch: ' (incl. deep_search {n})',

    // ---- Unmapped subtask fallback list ----
    unmappedSubtasks: 'Subtasks (unmapped to turn)',
    count: '{n}',
    turnsCount: '{n} turns',
    openSubtaskNewTab: 'Open subtask debug in a new tab',
    openDerivedSubtaskNewTab: "Open this turn's derived subtask debug in a new tab",
    noInput: '(no input)',
    open: 'Open →',

    // ---- User input block ----
    userInput: 'User input',
    lines: '{n} lines',
    chars: '{n} chars',

    // ---- Single turn ----
    round: 'Round {n}',
    firstToken: 'first token {ms}',
    firstTokenTip: 'Time from request to first content delta (streaming LLM calls only)',
    unknownPhase: 'Unknown phase',
    clickToCollapse: 'Click to collapse this turn',
    clickToExpand: 'Click to expand this turn',
    phaseTip: 'Semantic phase: is the task planning or executing',
    turnTypeTip: 'Call type: whether this step calls the LLM or a tool',
    subtaskDebug: '🔍 Subtask debug →',

    // ---- Streaming blocks ----
    streamLive: 'STREAM · live output',
    waitingModelOutput: '(waiting for model output…)',
    reasoningLive: 'Reasoning · live (reasoning_content)',
    toolCallLive: 'TOOL CALL · live',
    toolCallLiveDesc: 'Raw fragments accumulated from model-initiated tool calls',
    noFuncNameYet: '(function name not received yet)',
    waitingArgs: '(waiting for arguments…)',

    // ---- LLM input context block breakdown ----
    ctxBlockRatio: 'Context block breakdown',
    ctxEst: 'Est. {n} tok',
    ctxLimit: ' / limit {n} ({percent}%)',
    ctxProvider: ' · {n} provider-measured',
    ctxOverTrigger: ' · over compaction trigger',
    ctxTriggerLine: 'Compaction trigger (80% of limit)',
  },
};

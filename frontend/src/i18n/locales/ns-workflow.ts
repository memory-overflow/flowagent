// workflow 页面/组件的文案。zh 与 en 成对维护（见同目录 index.ts 的说明）。
//
// 只收前端写死的 UI 文案。后端返回的内容一律不进词典：
//   - 失败原因（archive_reason / append_reason / 接口报错）走 {msg} 插值；
//   - status_name 由后端给，作为变量插进 tipStatus，前端不重新翻译。
export const workflowNS = {
  zh: {
    // ---- 通用 ----
    title: '工作流',
    untitledChat: '未命名',
    backToChat: '返回对话',
    viewFullDag: '查看完整工作流 DAG',
    viewDetail: '查看详情',

    // ---- 工作流页头部 ----
    durationWhole: '整条工作流（规划期 + 各节点）耗时',
    tokenTip:
      '输入 {input} · 输出 {output}　（含规划期调用与每个节点派生的 deep_search 子任务）',
    tokenTotal: 'Token {n}',
    taskIdTip: '工作流任务 ID，点击复制',
    taskIdLabel: '任务 ID',
    openDebugTip: '在调试页打开该工作流任务',
    stopTip: '停止当前工作流，终止所有未完成的子任务',
    stopping: '停止中…',
    retryTip: '重新执行失败及未完成的步骤',
    retrying: '重试中…',
    retryFailed: '重试失败: {msg}',
    stopFailed: '停止失败: {msg}',
    resizeTip: '拖拽调整预览宽度',
    emptyTitle: '尚未规划出工作流',
    emptyDesc:
      '当前对话还没有绑定的工作流任务。回到对话中提出一个复杂任务，Agent 规划出 DAG 后即可在此查看完整流程。',

    // ---- 进度 / 已废弃节点 ----
    stepsDone: '{done}/{total} 步已完成',
    stepsRunning: '{n} 步执行中',
    archivedCount: '{n} 个节点已废弃',
    archivedTipGraph: '{n} 个节点已废弃（失败分支，灰显保留仅供追溯）',
    archivedTipBanner: '{n} 个节点已废弃（失败分支，不再执行，仍可展开查看失败原因）',
    archivedTipSwitcher: '{n} 个节点已废弃（失败分支，不再执行）',

    // ---- DAG 节点卡片 ----
    nodeArchivedTip: '该节点已被废弃，不再执行',
    archiveReasonFallback: '上游节点失败，该分支不再执行',

    // ---- 节点详情面板 ----
    nodeId: '节点 ID',
    nodeDesc: '说明',
    input: '输入',
    deps: '前置依赖',
    noDeps: '无（可直接开始）',
    dependents: '后续步骤',
    subTask: '子任务',
    execDuration: '执行时长',
    durationNode: '该节点（含 deep_search 子任务）耗时',
    tokenUsage: 'Token 消耗',
    tokenInOut: '（输入 {input} · 输出 {output}）',
    planSource: '规划来源',
    output: '执行结果',
    thinkSteps: '思考过程 · {n} 步',
    thinkLive: '思考过程 · 实时',
    liveSuffix: ' · 实时',
    waitingModel: '（等待模型输出…）',

    // ---- 对话页横幅 ----
    bannerTip: '点击查看当前工作流的完整 DAG',
    taskIdValue: '工作流任务 ID：{id}',

    // ---- 结果面板 ----
    result: '结果',
    resultTitle: '工作流结果',
    resultAriaCollapsed: '工作流结果（已折叠）',
    expandResult: '展开工作流结果',
    collapseResult: '收起右侧结果',
    deliverables: '交付文件',
    notFinishedTitle: '工作流尚未完成',
    notFinishedDesc: '当前进度 {done}/{total} 步。完成后这里会自动展开最终结果。',
    finishedTitle: '工作流已完成',
    finishedDesc:
      '但没有文本或文件类产出（可能全部成果已通过文件/工具落盘）。可前往工作流 DAG 页查看各节点详情。',

    // ---- 工作流切换器 ----
    switchFailed: '切换工作流失败: {msg}',
    switching: '切换中…',
    totalWorkflows: '该对话共有 {n} 条工作流',
    workflowCount: '工作流 {n}',
    tipWorkflow: '工作流 {index}：{title}',
    tipStatus: '状态：{status}',
    tipNodes: '节点：{done}/{total} 已完成',
    tipArchived: '，{n} 个已废弃',
    tipAppendable: '可续接（{n} 个节点成果可复用）',
    tipNotAppendable: '不可续接：{reason}',
    noReusableNodes: '无可复用节点',
  },

  en: {
    // ---- 通用 ----
    title: 'Workflow',
    untitledChat: 'Untitled',
    backToChat: 'Back to chat',
    viewFullDag: 'View the full workflow DAG',
    viewDetail: 'View details',

    // ---- 工作流页头部 ----
    durationWhole: 'Whole workflow (planning + all nodes) duration',
    tokenTip:
      'Input {input} · Output {output}　(includes planning calls and deep_search subtasks spawned by each node)',
    tokenTotal: 'Token {n}',
    taskIdTip: 'Workflow task ID — click to copy',
    taskIdLabel: 'Task ID',
    openDebugTip: 'Open this workflow task in the debug page',
    stopTip: 'Stop this workflow and terminate all unfinished subtasks',
    stopping: 'Stopping…',
    retryTip: 'Re-run the failed and unfinished steps',
    retrying: 'Retrying…',
    retryFailed: 'Failed to retry: {msg}',
    stopFailed: 'Failed to stop: {msg}',
    resizeTip: 'Drag to resize the preview panel',
    emptyTitle: 'No workflow planned yet',
    emptyDesc:
      'This conversation has no workflow task bound to it yet. Go back to the chat and ask for a complex task — once the agent plans a DAG, you can inspect the whole flow here.',

    // ---- 进度 / 已废弃节点 ----
    stepsDone: '{done}/{total} steps done',
    stepsRunning: '{n} steps running',
    archivedCount: '{n} nodes abandoned',
    archivedTipGraph: '{n} nodes abandoned (failed branch, kept greyed out for reference only)',
    archivedTipBanner:
      '{n} nodes abandoned (failed branch, no longer executed, still expandable to see why)',
    archivedTipSwitcher: '{n} nodes abandoned (failed branch, no longer executed)',

    // ---- DAG 节点卡片 ----
    nodeArchivedTip: 'This node has been abandoned and will not run',
    archiveReasonFallback: 'An upstream node failed, so this branch will not run',

    // ---- 节点详情面板 ----
    nodeId: 'Node ID',
    nodeDesc: 'Description',
    input: 'Input',
    deps: 'Depends on',
    noDeps: 'None (can start immediately)',
    dependents: 'Next steps',
    subTask: 'Subtask',
    execDuration: 'Duration',
    durationNode: 'Duration of this node (including deep_search subtasks)',
    tokenUsage: 'Tokens',
    tokenInOut: '(input {input} · output {output})',
    planSource: 'Planned by',
    output: 'Result',
    thinkSteps: 'Thinking · {n} steps',
    thinkLive: 'Thinking · live',
    liveSuffix: ' · live',
    waitingModel: '(waiting for model output…)',

    // ---- 对话页横幅 ----
    bannerTip: 'Click to view the full DAG of this workflow',
    taskIdValue: 'Workflow task ID: {id}',

    // ---- 结果面板 ----
    result: 'Results',
    resultTitle: 'Workflow results',
    resultAriaCollapsed: 'Workflow results (collapsed)',
    expandResult: 'Expand workflow results',
    collapseResult: 'Collapse the results panel',
    deliverables: 'Deliverables',
    notFinishedTitle: 'Workflow not finished yet',
    notFinishedDesc:
      'Progress: {done}/{total} steps. The final result shows up here once it completes.',
    finishedTitle: 'Workflow completed',
    finishedDesc:
      'but it produced no text or file output (results may have been written out through tools or files). Open the workflow DAG page to inspect each node.',

    // ---- 工作流切换器 ----
    switchFailed: 'Failed to switch workflow: {msg}',
    switching: 'Switching…',
    totalWorkflows: 'This conversation has {n} workflows',
    workflowCount: 'Workflows {n}',
    tipWorkflow: 'Workflow {index}: {title}',
    tipStatus: 'Status: {status}',
    tipNodes: 'Nodes: {done}/{total} completed',
    tipArchived: ', {n} abandoned',
    tipAppendable: 'Appendable ({n} node results reusable)',
    tipNotAppendable: 'Not appendable: {reason}',
    noReusableNodes: 'no reusable nodes',
  },
};

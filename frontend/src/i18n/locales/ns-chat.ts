// chat 页面/组件的文案。zh 与 en 成对维护（见同目录 index.ts 的说明）。
//
// 复用情况：取消/确认/加载/刷新/复制/已复制/停止/重试/未知 走 common.*，
// 任务状态走 status.*，重命名相关走 sidebar.*，这里只放 chat 页独有的文案。
export const chatNS = {
  zh: {
    // ---- 操作失败提示（alert）----
    unknownError: '未知错误',
    retryFailed: '重试失败: {msg}',
    stopFailed: '停止失败: {msg}',
    regenerateFailed: '重新生成失败: {msg}',
    sendFailed: '发送失败: {msg}',
    switchModelFailed: '切换模型失败: {msg}',
    removeFailed: '移除失败: {msg}',
    addFailed: '添加失败: {msg}',
    uploadPartialFailed: '部分文件上传失败:\n{list}',
    uploadEntry: '{name}: {msg}',

    // ---- 页面级空态 ----
    notFound: '对话不存在',
    emptyTitle: '开始对话',
    emptyDesc: '在下方输入你的问题或任务，Agent 将通过 MCP 工具为你完成',

    // ---- 输入区 ----
    uploading: '上传中…',
    remove: '移除',
    inputPlaceholder: '今天帮你做些什么？Enter 发送，Shift+Enter 换行，可拖入或粘贴文件',
    attachTitle: '添加附件（也可直接拖入或粘贴）',
    attach: '📎 附件',
    newWfOnTitle: '已开启：本条消息会新开一个工作流，不续接已有的工作流（发送后自动关闭）',
    newWfOffTitle: '开启后，本条消息会新开一个工作流。未开启时由模型判断续接还是新开',
    newWfOn: '🆕 新开工作流 ✓',
    newWfOff: '🆕 新开工作流',
    inputHint: '@ 引用对话文件 · # 调用技能与指令',
    charCount: '{n} 字',
    sendTitle: '发送 (Enter)',
    splitterAria: '拖动以调整结果分栏宽度',
    splitterTitle: '拖动调整宽度',

    // ---- 输入框 @ / # 引用菜单 ----
    mentionTitleFile: '@ 引用对话文件',
    mentionTitleSkill: '# 调用技能与指令',
    mentionEmptyFile: '这个对话还没有可引用的文件 —— 先添加附件，或等 Agent 产出文件',
    mentionEmptySkill: '没有匹配的技能或指令',
    mentionGroupFiles: '对话文件',
    mentionGroupSkills: '技能',
    mentionGroupCommands: '指令',
    mentionTagUpload: '我上传的',
    mentionTagOutput: '产物',
    mentionTagEnabled: '已启用',
    mentionTagEnable: '回车启用',
    mentionFooter: '↑↓ 选择 · Enter 确认 · Esc 关闭',
    mentionCmdStop: '停止当前任务',
    mentionCmdStopDesc: '终止正在执行的任务或工作流',
    mentionCmdNewWf: '新开工作流：{state}',
    mentionCmdNewWfOn: '已开启',
    mentionCmdNewWfOff: '已关闭',
    mentionCmdNewWfDesc: '切换下一条消息是否新开工作流',
    mentionCmdClear: '清空输入',
    mentionCmdClearDesc: '清空输入框里的内容',

    // ---- 对话头部 ----
    untitled: '未命名',
    collapsePanelTitle: '收起右侧结果分栏',
    expandPanelTitle: '打开右侧结果分栏',
    collapseResult: '▶ 收起结果',
    expandResult: '◀ 📦 结果',
    viewWorkflowTitle: '查看当前工作流 DAG',
    workflow: '🕸️ 工作流',

    // ---- 模型选择器 ----
    modelSelectorTitle: '选择本对话使用的模型',
    defaultModel: '默认模型',
    defaultSuffix: '（默认）',

    // ---- 单轮任务 ----
    noTextInput: '(无文本输入)',
    attachments: '附件',
    copyInputTitle: '复制这条输入的正文',
    noTextInputTitle: '本轮没有文本输入',
    regenerateRunningTitle: '本轮仍在执行中，结束后才能重新生成',
    regenerateTitle: '以相同输入（含附件）发起新一轮，本轮记录完整保留',
    regenerating: '发起中…',
    regenerate: '重新生成',
    viewWfFromTurnTitle: '查看本轮规划出的工作流流程图与各节点详情',
    viewWorkflow: '查看工作流',
    debugTitle: '在新标签页打开调试视图，查看该任务的所有 turn 与输入输出',
    debug: '🐞 调试',
    stopTaskTitle: '终止该任务',
    retryTitle: '清空本轮结果并重新执行该任务',
    compacting: '正在压缩上下文…',
    callingTool: '正在调用 ',
    callingToolTail: ' 工具…',
    generating: '正在生成',

    // ---- token 徽标提示 ----
    tokenTipTotal: '总计 {n} tokens',
    tokenTipSplit: '输入 {prompt} / 输出 {completion}',
    tokenTipSub: '主循环 {own} + 子任务 {sub}（{pct}%）',

    // ---- Agent 输出与思考过程 ----
    noTextReply: '（本次没有文本回复）',
    outputFiles: '产物文件',
    thinkProcess: '思考过程 · {n} 步',
    inProgress: '进行中',
    planningPhase: '规划期',
    executionPhase: '执行期',
    thinkGroup: '{title} · {n} 步',

    // ---- MCP 挂载条 ----
    addMcpTitle: '添加 MCP 工具',
    addTool: '添加工具',
    noMcpMounted: '未挂载任何 MCP 工具',
    selectMcpServers: '选择要添加的 MCP 服务器',
    noMcpAvailable: '没有可用的 MCP 服务器。',
    goToMcpPage: '前往 MCP 管理页面创建 →',
    selectedCount: '已选 {n} 项',
    confirmAdd: '确认添加',

    // ---- Skill 挂载条 ----
    addSkillTitle: '添加 Skill',
    addSkill: '添加技能',
    noSkillEnabled: '未启用任何 Skill',
    selectSkills: '选择要启用的 Skill',
    noSkillAvailable: '没有可用的 Skill。',
    goToSkillPage: '前往 Skill 管理页面创建 →',
  },

  en: {
    // ---- 操作失败提示（alert）----
    unknownError: 'Unknown error',
    retryFailed: 'Retry failed: {msg}',
    stopFailed: 'Failed to stop: {msg}',
    regenerateFailed: 'Failed to regenerate: {msg}',
    sendFailed: 'Failed to send: {msg}',
    switchModelFailed: 'Failed to switch model: {msg}',
    removeFailed: 'Failed to remove: {msg}',
    addFailed: 'Failed to add: {msg}',
    uploadPartialFailed: 'Some files failed to upload:\n{list}',
    uploadEntry: '{name}: {msg}',

    // ---- 页面级空态 ----
    notFound: 'Conversation not found',
    emptyTitle: 'Start a conversation',
    emptyDesc:
      'Ask a question or describe a task below — the agent will use MCP tools to get it done',

    // ---- 输入区 ----
    uploading: 'Uploading…',
    remove: 'Remove',
    inputPlaceholder:
      'What can I help with today? Enter to send, Shift+Enter for a new line — you can also drag in or paste files',
    attachTitle: 'Add attachments (or just drag them in / paste them)',
    attach: '📎 Attach',
    newWfOnTitle:
      'On: this message starts a new workflow instead of continuing the existing one (turns off automatically after sending)',
    newWfOffTitle:
      'Turn on to start a new workflow for this message. When off, the model decides whether to continue or start fresh',
    newWfOn: '🆕 New workflow ✓',
    newWfOff: '🆕 New workflow',
    inputHint: '@ to reference conversation files · # to invoke skills and commands',
    charCount: '{n} chars',
    sendTitle: 'Send (Enter)',
    splitterAria: 'Drag to resize the result panel',
    splitterTitle: 'Drag to resize',

    // ---- @ / # mention menu in the composer ----
    mentionTitleFile: '@ Reference conversation files',
    mentionTitleSkill: '# Invoke skills and commands',
    mentionEmptyFile: 'No referenceable files in this conversation yet — attach one, or wait for the agent to produce files',
    mentionEmptySkill: 'No matching skill or command',
    mentionGroupFiles: 'Conversation files',
    mentionGroupSkills: 'Skills',
    mentionGroupCommands: 'Commands',
    mentionTagUpload: 'uploaded',
    mentionTagOutput: 'output',
    mentionTagEnabled: 'enabled',
    mentionTagEnable: 'Enter to enable',
    mentionFooter: '↑↓ to move · Enter to confirm · Esc to close',
    mentionCmdStop: 'Stop current task',
    mentionCmdStopDesc: 'Terminate the running task or workflow',
    mentionCmdNewWf: 'Start new workflow: {state}',
    mentionCmdNewWfOn: 'on',
    mentionCmdNewWfOff: 'off',
    mentionCmdNewWfDesc: 'Toggle whether the next message starts a new workflow',
    mentionCmdClear: 'Clear input',
    mentionCmdClearDesc: 'Clear the composer',

    // ---- 对话头部 ----
    untitled: 'Untitled',
    collapsePanelTitle: 'Hide the result panel on the right',
    expandPanelTitle: 'Show the result panel on the right',
    collapseResult: '▶ Hide results',
    expandResult: '◀ 📦 Results',
    viewWorkflowTitle: 'View the DAG of the current workflow',
    workflow: '🕸️ Workflow',

    // ---- 模型选择器 ----
    modelSelectorTitle: 'Choose the model for this conversation',
    defaultModel: 'Default model',
    defaultSuffix: ' (default)',

    // ---- 单轮任务 ----
    noTextInput: '(no text input)',
    attachments: 'Attachments',
    copyInputTitle: 'Copy the text of this input',
    noTextInputTitle: 'This round has no text input',
    regenerateRunningTitle: 'This round is still running — wait for it to finish before regenerating',
    regenerateTitle:
      'Start a new round with the same input (including attachments). This round is kept as is',
    regenerating: 'Starting…',
    regenerate: 'Regenerate',
    viewWfFromTurnTitle: 'View the workflow planned in this round — its graph and per-node details',
    viewWorkflow: 'View workflow',
    debugTitle: 'Open the debug view in a new tab to inspect every turn and its inputs/outputs',
    debug: '🐞 Debug',
    stopTaskTitle: 'Stop this task',
    retryTitle: 'Clear this round and run the task again',
    compacting: 'Compacting context…',
    callingTool: 'Calling ',
    callingToolTail: '…',
    generating: 'Generating',

    // ---- token 徽标提示 ----
    tokenTipTotal: 'Total {n} tokens',
    tokenTipSplit: 'Input {prompt} / Output {completion}',
    tokenTipSub: 'Main loop {own} + sub-tasks {sub} ({pct}%)',

    // ---- Agent 输出与思考过程 ----
    noTextReply: '(no text reply this round)',
    outputFiles: 'Output files',
    thinkProcess: 'Thinking process · {n} steps',
    inProgress: 'Running',
    planningPhase: 'Planning',
    executionPhase: 'Execution',
    thinkGroup: '{title} · {n} steps',

    // ---- MCP 挂载条 ----
    addMcpTitle: 'Add MCP tools',
    addTool: 'Add tools',
    noMcpMounted: 'No MCP tools mounted',
    selectMcpServers: 'Select MCP servers to add',
    noMcpAvailable: 'No MCP servers available. ',
    goToMcpPage: 'Go to the MCP page to create one →',
    selectedCount: '{n} selected',
    confirmAdd: 'Confirm',

    // ---- Skill 挂载条 ----
    addSkillTitle: 'Add skill',
    addSkill: 'Add skills',
    noSkillEnabled: 'No skills enabled',
    selectSkills: 'Select skills to enable',
    noSkillAvailable: 'No skills available. ',
    goToSkillPage: 'Go to the Skills page to create one →',
  },
};

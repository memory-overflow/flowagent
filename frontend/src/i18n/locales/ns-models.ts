// models 页面/组件的文案。zh 与 en 成对维护（见同目录 index.ts 的说明）。
export const modelsNS = {
  zh: {
    title: '模型配置',
    newModel: '+ 新建模型',
    intro:
      '全局模型配置，替代原先写死在 config.yaml 的单一模型。支持 OpenAI 兼容、Gemini、Azure OpenAI(gpt)、Claude 四种协议；对话中可选择使用哪个模型，未指定时回落到默认模型。',

    emptyTitle: '暂无模型配置',
    emptyDesc: '点击右上角「新建模型」添加第一个模型（将自动设为默认）。',

    // ---- 列表表头 ----
    protocol: '协议',
    modelName: '模型名',
    image: '图像',
    context: '上下文',
    updated: '更新时间',

    // ---- 协议下拉选项（专有名词保留原文）----
    protocolOpenai: 'OpenAI 兼容',
    protocolGemini: 'Gemini 网关',
    protocolGpt: 'Azure OpenAI (gpt)',
    protocolClaude: 'Claude (Anthropic)',

    // ---- 图像列 ----
    imageUntested: '未测试',
    imageUntestedTitle: '尚未测试，点「测试」自动探测',
    imageMultimodal: '多模态',
    imageMultimodalTitle: '支持图像理解',
    imageTextOnly: '纯文本',
    imageTextOnlyTitle: '不支持图像理解',

    // ---- 上下文列 ----
    contextTitle: '最大输入 {limit} token；超过 80%（{threshold}）自动压缩历史',
    contextMissing: '未配置',
    contextMissingTitle: '未配置上下文上限，运行时会按模型名匹配内置表，匹配不到则回落到 128K',

    // ---- 状态 / 默认列 ----
    clickEnable: '点击启用',
    clickDisable: '点击禁用',
    defaultLocked: '默认模型不可停用',
    defaultBadge: '默认',
    setDefault: '设为默认',

    // ---- 操作与测试结果 ----
    test: '测试',
    testing: '测试中...',
    connectOk: '连接成功',
    connectFail: '连接失败',
    testTarget: '模型：{name}',
    testingDesc: '正在连通性探测（15s 超时）...',
    testReply: '模型回显：',
    imageSupportLabel: '图像理解',
    imageSupport: '图像理解：{v}',
    imageSupported: '✅ 支持（多模态）',
    imageUnsupported: '❌ 不支持（纯文本）',

    // ---- 表单：标题与字段 ----
    formCreateTitle: '新建模型配置',
    formEditTitle: '编辑模型配置',
    nameRequired: '名称 *',
    namePlaceholder: '例如: GPT-5.6 Sol',
    baseUrlRequired: 'Base URL *',
    apiKey: 'API Key',
    apiKeyClaudePlaceholder: 'sk-tp-...（Token Plan 密钥，勿填 Base URL）',
    modelNameRequired: '模型名 *',
    contextLimitField: '上下文上限（最大输入 token）',
    contextLimitPlaceholder: '如 1048576；留空则按模型名自动识别',
    contextLimitHint:
      '填「最大输入 token」而非总窗口（总窗口通常 = 输入上限 + 输出上限）。对话输入超过此值的 80% 时会自动压缩历史。留空则按模型名匹配内置表，匹配不到回落到 128K。',
    apiVersion: 'API 版本',
    apiVersionPlaceholder: '例如: 2025-04-01-preview',
    supportsImage: '支持图像理解（多模态）',

    // ---- 提示与报错 ----
    errNameRequired: '请输入名称',
    errBaseUrlRequired: '请输入 Base URL',
    errModelRequired: '请输入模型名',
    errTestBaseUrl: '请先填写 Base URL',
    errTestModelName: '请先填写模型名',
    deleteConfirm: '确定删除此模型配置？',
    deleteFailed: '删除失败: {msg}',
    setDefaultFailed: '设为默认失败: {msg}',
    updateFailed: '更新失败: {msg}',
    saveFailed: '保存失败: {msg}',
    testFailed: '测试失败：{msg}',
    testFailResult: '连接失败：{msg}',
    testOkResult: '连接成功；图像理解：{v}',
    testOkResultErr: '连接成功；图像理解：{v}（{err}）',
    visionSupported: '✅ 支持',
    visionUnsupported: '❌ 不支持',
    unsavedConfirm: '表单有未保存的修改，确定放弃吗？',
  },
  en: {
    title: 'Models',
    newModel: '+ New model',
    intro:
      'Global model configuration, replacing the single model previously hardcoded in config.yaml. Supports four protocols: OpenAI-compatible, Gemini, Azure OpenAI (gpt) and Claude. Pick a model per conversation; conversations that do not specify one fall back to the default model.',

    emptyTitle: 'No models configured',
    emptyDesc: 'Click “New model” in the top-right corner to add your first model — it becomes the default automatically.',

    // ---- 列表表头 ----
    protocol: 'Protocol',
    modelName: 'Model name',
    image: 'Image',
    context: 'Context',
    updated: 'Updated',

    // ---- 协议下拉选项（专有名词保留原文）----
    protocolOpenai: 'OpenAI compatible',
    protocolGemini: 'Gemini gateway',
    protocolGpt: 'Azure OpenAI (gpt)',
    protocolClaude: 'Claude (Anthropic)',

    // ---- 图像列 ----
    imageUntested: 'Not tested',
    imageUntestedTitle: 'Not tested yet — click “Test” to detect it automatically',
    imageMultimodal: 'Multimodal',
    imageMultimodalTitle: 'Supports image understanding',
    imageTextOnly: 'Text only',
    imageTextOnlyTitle: 'Does not support image understanding',

    // ---- 上下文列 ----
    contextTitle: 'Max input {limit} tokens; history is compacted automatically past 80% ({threshold})',
    contextMissing: 'Not configured',
    contextMissingTitle:
      'No context limit configured. At runtime it is matched against a built-in table by model name, falling back to 128K when nothing matches.',

    // ---- 状态 / 默认列 ----
    clickEnable: 'Click to enable',
    clickDisable: 'Click to disable',
    defaultLocked: 'The default model cannot be disabled',
    defaultBadge: 'Default',
    setDefault: 'Set as default',

    // ---- 操作与测试结果 ----
    test: 'Test',
    testing: 'Testing...',
    connectOk: 'Connected',
    connectFail: 'Connection failed',
    testTarget: 'Model: {name}',
    testingDesc: 'Probing connectivity (15s timeout)...',
    testReply: 'Model reply: ',
    imageSupportLabel: 'Image understanding',
    imageSupport: 'Image understanding: {v}',
    imageSupported: '✅ Supported (multimodal)',
    imageUnsupported: '❌ Not supported (text only)',

    // ---- 表单：标题与字段 ----
    formCreateTitle: 'New model',
    formEditTitle: 'Edit model',
    nameRequired: 'Name *',
    namePlaceholder: 'e.g. GPT-5.6 Sol',
    baseUrlRequired: 'Base URL *',
    apiKey: 'API Key',
    apiKeyClaudePlaceholder: 'sk-tp-... (Token Plan key — do not put it in Base URL)',
    modelNameRequired: 'Model name *',
    contextLimitField: 'Context limit (max input tokens)',
    contextLimitPlaceholder: 'e.g. 1048576; leave empty to detect from the model name',
    contextLimitHint:
      'Enter the maximum input tokens rather than the total window (the total window is usually the input limit plus the output limit). Conversation history is compacted automatically once the input exceeds 80% of this value. Leave it empty to match against a built-in table by model name, falling back to 128K when nothing matches.',
    apiVersion: 'API version',
    apiVersionPlaceholder: 'e.g. 2025-04-01-preview',
    supportsImage: 'Supports image understanding (multimodal)',

    // ---- 提示与报错 ----
    errNameRequired: 'Please enter a name',
    errBaseUrlRequired: 'Please enter a Base URL',
    errModelRequired: 'Please enter a model name',
    errTestBaseUrl: 'Enter a Base URL first',
    errTestModelName: 'Enter a model name first',
    deleteConfirm: 'Delete this model configuration?',
    deleteFailed: 'Failed to delete: {msg}',
    setDefaultFailed: 'Failed to set as default: {msg}',
    updateFailed: 'Failed to update: {msg}',
    saveFailed: 'Failed to save: {msg}',
    testFailed: 'Test failed: {msg}',
    testFailResult: 'Connection failed: {msg}',
    testOkResult: 'Connected; image understanding: {v}',
    testOkResultErr: 'Connected; image understanding: {v} ({err})',
    visionSupported: '✅ Supported',
    visionUnsupported: '❌ Not supported',
    unsavedConfirm: 'You have unsaved changes. Discard them?',
  },
};

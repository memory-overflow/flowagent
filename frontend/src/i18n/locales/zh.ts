/**
 * 中文词典。
 *
 * 结构按「页面/组件命名空间」分组，key 用点号路径访问（见 ../index.tsx 的 t）。
 * 翻译原则：
 *   - 只收**前端写死**的 UI 文案；后端返回的动态内容（失败原因、LLM 生成的
 *     节点名/描述、文件路径等）不进词典，原样展示。
 *   - 状态名这类「后端给字符串、但前端有枚举」的，由前端按枚举重新映射（见
 *     status.ts），不走词典。
 *   - 占位符用 {name} 形式，t('a.b', { name: 'x' }) 注入。
 */
export const zh = {
  common: {
    save: '保存',
    saving: '保存中…',
    cancel: '取消',
    confirm: '确定',
    delete: '删除',
    deleting: '删除中…',
    edit: '编辑',
    add: '新增',
    create: '创建',
    creating: '创建中…',
    refresh: '刷新',
    loading: '加载中...',
    retry: '重试',
    stop: '停止',
    copy: '复制',
    copied: '已复制',
    close: '关闭',
    enable: '启用',
    disable: '停用',
    enabled: '已启用',
    disabled: '已停用',
    yes: '是',
    no: '否',
    on: '开',
    off: '关',
    all: '全部',
    none: '无',
    search: '搜索',
    name: '名称',
    desc: '描述',
    status: '状态',
    type: '类型',
    action: '操作',
    time: '时间',
    detail: '详情',
    expand: '展开',
    collapse: '折叠',
    more: '更多',
    optional: '选填',
    required: '必填',
    unknown: '未知',
    empty: '(空)',
    total: '共 {n} 项',
    seconds: '{n} 秒',
  },

  // ---- 侧边栏 / 导航 ----
  nav: {
    chats: '对话',
    mcp: 'MCP',
    skills: '技能',
    models: '模型',
    search: '搜索',
    settings: '设置',
    newChat: '新建对话',
    searchPlaceholder: '搜索对话…',
    noConversations: '暂无对话',
    collapseSidebar: '收起侧边栏',
    expandSidebar: '展开侧边栏',
    deleteConfirm: '确定删除该对话？此操作不可恢复。',
  },

  // ---- 欢迎页 ----
  welcome: {
    title: 'Workflow Agent',
    desc:
      '通用工作流 Agent 平台。支持单 Agent 对话和 DAG 工作流编排，通过 MCP 协议接入外部工具，让 AI 完成复杂任务。',
    newChat: '新建对话',
    createFailed: '创建对话失败: {msg}',
  },

  // ---- 首次配置引导（无可用模型时全屏阻断）----
  setup: {
    title: '先配置一个可用模型',
    desc:
      '还没有配置任何可用模型。Workflow Agent 需要至少一个「已启用」的模型才能跑对话和工作流，请先添加一个。',
    step1: '选择接入协议：OpenAI 兼容 / Gemini / Azure OpenAI / Claude',
    step2: '填写 Base URL、API Key 和模型名',
    step3: '点「测试」确认能连通，然后保存',
    addModel: '添加模型',
    hint:
      '第一个添加的模型会自动成为默认模型。之后可以在左侧「模型」页随时增删改、切换默认。',
  },

  // ---- 设置页：外观与语言（新增） ----
  preferences: {
    cardTitle: '外观与语言',
    cardDesc: '界面语言与配色主题，保存在本机浏览器，不影响其他设备。',
    language: '界面语言',
    languageHint: '切换后立即生效，无需刷新。',
    langZh: '中文',
    langEn: 'English',
    theme: '主题',
    themeHint: '跟随系统会随操作系统的深浅色设置自动切换。',
    themeLight: '浅色',
    themeDark: '深色',
    themeSystem: '跟随系统',
    themeSystemResolved: '（当前为{theme}）',
    themeLightName: '浅色',
    themeDarkName: '深色',
  },

  // ---- 设置页：数据库连接 ----
  settings: {
    title: '系统设置',
    introLead:
      '数据库连接。云上 MySQL 每次查询都要跨公网往返、延迟很高，本地调试可切到本机 MySQL 或 sqlite（纯本地文件，无网络往返）。配置保存在服务端',
    introTail:
      '，不写进 conf.yaml，密码也不随代码提交。保存后立即生效，无需重启（scheduler 会在 2 秒内自动跟随）。',
    activeDesc: '当前进程在用：',
    matches: '（与下面这份设置一致）',
    mismatch: '（与下面这份设置不一致 —— 保存后会立即切换到这里这份）',
    dbType: '数据库类型',
    sqliteOption: 'SQLite（本地文件，零依赖）',
    host: '地址',
    port: '端口',
    user: '用户名',
    password: '密码',
    dbname: '数据库名',
    charset: '字符集',
    dbPath: '数据库文件路径',
    pathHint: '目录不存在会自动创建。首次启动时自动建表。',
    testConn: '测试连接',
    testing: '测试中...',
    saved: '已保存并生效',
    saveFailed: '保存失败: {msg}',
    testFailed: '测试失败: {msg}',
    connectOkLabel: '连接成功',
    connectOk: '✅ 连接成功',
    connectFail: '❌ 连接失败',
  },

  // ---- 任务状态：前端按枚举重新映射，不直接用后端返回的中文 ----
  status: {
    pending: '排队中',
    running: '执行中',
    completed: '已完成',
    failed: '失败',
    cancelled: '已终止',
    partialFailed: '部分失败',
    abandoned: '已废弃',
  },

  // ---- 侧边栏各页辅助区 ----
  sidebar: {
    newShort: '+ 新建',
    createFailed: '创建失败: {msg}',
    deleteFailed: '删除失败: {msg}',
    renameFailed: '修改标题失败: {msg}',
    untitled: '未命名对话',
    emptyHint: '暂无对话，点击"新建"开始',
    rename: '重命名',
    runningTitle: '有任务或工作流正在执行中',
    failedTitle: '有任务或工作流执行失败',
    mcpTitle: 'MCP 工具',
    mcpHintTitle: 'MCP 服务器管理',
    mcpHintDesc:
      '在右侧面板创建、编辑、测试 MCP 服务器。在每个对话的输入框上方可挂载需要使用的工具。',
    skillsTitle: 'Skill 技能',
    skillsHintTitle: 'Skill 管理',
    skillsHintDesc:
      '在右侧面板创建、编辑、导入 Skill。指令型注入 system prompt，可执行型作为工具被模型调用；每个对话可单独启用。',
    modelsTitle: '模型配置',
    modelsHintTitle: '全局模型配置',
    modelsHintDesc:
      '在右侧面板增删改模型。支持 OpenAI 兼容 / Gemini / Azure OpenAI(gpt) / Claude 四种协议；对话中可选模型，未指定时用默认模型。',
    searchTitle: '检索引擎',
    searchHintTitle: '全局检索引擎配置',
    searchHintDesc:
      '在右侧面板切换 deep_search 使用的检索引擎。默认百度（零 key）；切博查 / Tavily / Serper 等需填 API Key，检索失败最终降级到百度。',
  },

  // ---- 通用组件 ----
  errorBoundary: {
    title: '页面渲染出错',
    desc: '请刷新页面重试；如反复出现，请把下面错误信息发给开发者。',
    reload: '刷新页面',
    recover: '尝试恢复',
  },

  duration: {
    runningTip: '{label} {elapsed}　开始于 {start}，进行中（含排队与等待）',
    doneTip: '{label} {elapsed}　{start} → {end}（墙钟，含排队与等待）',
  },

  files: {
    preview: '预览',
    previewX: '预览 {name}',
    open: '打开',
    opening: '打开中…',
    openX: '打开 {name}',
    openLocal: '打开本地文件',
    openFailed: '打开失败: {msg}',
    collapseAll: '收起',
    expandAll: '展开全部 {n} 个文件',
    loading: '加载中…',
    readFailed: '内容读取失败，请打开本地文件查看。',
    tooLarge: '文件过大，暂不支持在线预览，请打开本地文件查看。',
    unsupported: '该类型暂不支持浏览器内预览，可用下方按钮打开本地文件。',
    closePreview: '关闭预览',
  },

  toolCalls: {
    tool: '工具：',
    args: '参数: ',
    collapseAll: '收起工具调用',
    expandAll: '展开全部 {n} 条工具调用（还有 {hidden} 条）',
  },

  context: {
    title: '上下文用量',
    pillTip: '上下文已使用 {percent}% · {used} / {limit}',
    used: '已使用 {used} / {limit}',
    triggerLine: '压缩触发线 {n}（上限的 80%）',
    source: '来源：turn #{seq}（{phase}）',
    providerTok: '{n} tok 为 provider 实测输入',
    noRecord: '暂无调用记录',
    block: {
      system: '系统提示词',
      systemDesc: 'agent 人格 / 工具指引 / 规划上下文（不可压缩）',
      tools: '工具定义',
      toolsDesc: '工具 JSON Schema，每轮全量重发（不可压缩）',
      wfSummary: '工作流摘要',
      wfSummaryDesc: '历史工作流的折叠摘要，本身已是压缩产物（不可压缩）',
      compacted: '压缩历史摘要',
      compactedDesc: '上下文压缩生成的滚动工作记录',
      history: '历史消息',
      historyDesc: '尚未压缩的历史 task 交互明细',
      current: '本轮输入与当前任务',
      currentDesc: '本轮用户输入 + 当前任务已发生的交互（永不压缩）',
    },
  },

  // __APPEND_MARKER__
};

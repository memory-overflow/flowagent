import axios from 'axios';
import type {
  CommonResponse,
  ConversationInfo,
  ConversationDetail,
  FileInfo,
  TaskDetail,
  MCPServerConfig,
  SkillConfig,
  SkillRef,
  ModelConfig,
  ModelStatus,
  SearchConfig,
  SearchTestResult,
  DBConfig,
  DBConfigUpdateResult,
  DBTestResult,
  WorkflowListItem,
} from '../types/api';

const http = axios.create({
  baseURL: '/api/v1',
  timeout: 30000,
  headers: { 'Content-Type': 'application/json' },
});

// 请求拦截器：把界面语言带给后端，后端据此返回 *_display 展示字段。
// 只影响「给用户看的文案」；给 LLM 的 prompt 后端始终用中文原文。
// 语言值与 i18n/index.tsx 的 STORAGE_KEY 保持一致。
http.interceptors.request.use((config) => {
  try {
    const lang = localStorage.getItem('flowagent.lang');
    if (lang === 'en' || lang === 'zh') {
      config.headers['X-Lang'] = lang;
    }
  } catch {
    /* localStorage 不可用就不带，后端默认中文 */
  }
  return config;
});

http.interceptors.response.use(
  (res) => res,
  (err) => {
    const msg = err.response?.data?.msg || err.message || '网络错误';
    return Promise.reject(new Error(msg));
  }
);

// ============================================================
// 对话接口
// ============================================================

/** 创建对话 */
export function createConversation(title?: string, mcpServerIds?: string[]) {
  return http.post<CommonResponse<ConversationInfo>>('/conversations/create', {
    title,
    mcp_server_ids: mcpServerIds,
  });
}

/**
 * 上传附件到对话工作区。
 *
 * 走 multipart 真上传（后端落盘到 workspace/<chat_id>/inputs/），
 * 返回的 FileInfo 可直接放进 sendMessage 的 inputFiles。
 *
 * 注意不要用 URL.createObjectURL 造 FileInfo —— blob URL 只在浏览器内存里有效，
 * 后端与 agent 都拿不到文件内容。
 */
export function uploadFile(chatId: string, file: File) {
  const form = new FormData();
  form.append('chat_id', chatId);
  form.append('file', file);
  return http.post<CommonResponse<FileInfo>>('/files/upload', form, {
    // 让浏览器自动补 multipart boundary，不能手写 Content-Type
    headers: { 'Content-Type': undefined as unknown as string },
    // 上传大文件比普通请求慢，单独放宽超时
    timeout: 120000,
  });
}

/** 拼工作区文件的下载地址（后端已在 FileInfo.url 给出，此处供兜底/手工构造） */
export function buildFileDownloadURL(chatId: string, path: string, inline = false) {
  const q = new URLSearchParams({ chat_id: chatId, path });
  if (inline) q.set('inline', '1');
  return `/api/v1/files/download?${q.toString()}`;
}

/**
 * 用系统默认应用在本地打开工作区文件（本地 agent 场景，替代浏览器下载）。
 *
 * FileInfo.url 形如 /api/v1/files/download?chat_id=..&path=..，其中已包含
 * 打开所需的 chat_id 与 path，直接替换成 open 端点即可，无需额外入参。
 */
export function openLocalFile(url: string) {
  const openPath = url.replace('/api/v1/files/download', '/files/open');
  return http.get<CommonResponse<null>>(openPath);
}

/** 在文件管理器中定位文件（打开所在目录并选中）。 */
export function revealFile(url: string) {
  const revealPath = url.replace('/api/v1/files/download', '/files/reveal');
  return http.get<CommonResponse<null>>(revealPath);
}

/** 打开当前对话的工作空间目录。 */
export function openWorkspace(chatId: string) {
  return http.get<CommonResponse<null>>(`/files/open-workspace?chat_id=${encodeURIComponent(chatId)}`);
}

/** 发送消息。inputFiles 为 uploadFile 返回的 FileInfo 列表（已落盘工作区） */
/**
 * 发送一条消息，创建对话任务。
 *
 * forceNewWorkflow 是给用户的显式入口：一个对话可以有多条工作流，
 * 规划期默认由模型在候选清单里选「续接已有工作流」还是「新开工作流」，
 * 而用户最清楚本轮任务和之前那堆 DAG 有没有关系 —— 置 true 时规划期
 * 无条件新开，用户判断优先于模型判断。仅对本次任务生效，不落库。
 */
export function sendMessage(
  chatId: string,
  inputText?: string,
  inputFiles?: FileInfo[],
  forceNewWorkflow?: boolean,
) {
  return http.post<CommonResponse<{ task_id: string }>>('/conversations/chat', {
    chat_id: chatId,
    input_text: inputText || '',
    input_files: inputFiles || [],
    // 只在显式开启时带上，避免默认 false 淹没请求体语义
    ...(forceNewWorkflow ? { force_new_workflow: true } : {}),
  });
}

// ============================================================
// 工作流接口（v1.4：一个对话可绑定多条工作流）
// ============================================================

/** 拉取一个对话下的全部工作流 + 当前激活 ID（切换器数据源） */
export function listChatWorkflows(chatId: string) {
  return http.post<
    CommonResponse<{
      chat_id: string;
      active_workflow_task_id: string;
      list: WorkflowListItem[];
      total: number;
    }>
  >('/workflows/list', { chat_id: chatId });
}

/**
 * 切换对话「当前查看」的工作流。
 *
 * 纯指针操作：只改 conversation.workflow_task_id，不动 DAG、不改任务状态。
 * taskId 传空串表示取消激活（前端收起 DAG 面板），不删除任何数据。
 */
export function setActiveWorkflow(chatId: string, taskId: string) {
  return http.post<CommonResponse<{ chat_id: string; active_workflow_task_id: string }>>(
    '/workflows/set-active',
    { chat_id: chatId, task_id: taskId },
  );
}

/** 对话列表 (瀑布流分页) */
export function listConversations(lastTs?: string, keyword?: string) {
  return http.post<CommonResponse<{ list: ConversationInfo[]; total: number; has_more: boolean }>>(
    '/conversations/list',
    { last_ts: lastTs, keyword }
  );
}

/** 对话详情 */
export function getConversationDetail(chatId: string) {
  return http.post<CommonResponse<ConversationDetail>>('/conversations/detail', { chat_id: chatId });
}

// ============================================================
// 上下文用量（输入框右下角指示器 + 分块明细）
// ============================================================

/** 一次 LLM 调用输入上下文的分块 token 占用（后端组装消息时实测写入 turn 快照） */
export interface ContextUsageInfo {
  chat_id: string;
  /** 对话当前所选模型名 */
  model_name?: string;
  /** 模型最大输入 token 上限（口径=最大输入，非总窗口） */
  context_limit: number;
  /** 压缩触发线 / 目标线（token），前端画刻度用 */
  trigger: number;
  target: number;
  /** 是否有可展示的分块数据（新对话或老数据为 false） */
  has_data: boolean;
  blocks?: {
    system_prompt: number;
    tools: number;
    workflow_summary: number;
    compacted: number;
    detail: number;
    user_and_current: number;
    /** 各块之和 = 组装时的估算输入总量 */
    total: number;
  };
  /** 数据来源 turn 的元信息 */
  turn?: {
    task_id: string;
    turn_seq: number;
    phase: number;
    phase_name: string;
    model?: string;
    status_name: string;
    create_ts: number;
    /** provider 回报的真实输入 token（与 blocks.total 的 tiktoken 估算对照） */
    prompt_tokens: number;
  };
}

/** 查询对话上下文用量：最近一次 LLM 调用的输入分块明细 + 模型上限/水位 */
export function getContextUsage(chatId: string) {
  return http.post<CommonResponse<ContextUsageInfo>>('/conversations/context-usage', {
    chat_id: chatId,
  });
}

/** 删除对话 */
export function deleteConversation(chatId: string) {
  return http.post<CommonResponse<null>>('/conversations/delete', { chat_id: chatId });
}

/** 切换对话使用的模型（modelId 传空串表示恢复默认模型） */
export function updateConversationModel(chatId: string, modelId: string) {
  return http.post<CommonResponse<null>>('/conversations/update-model', {
    chat_id: chatId,
    model_id: modelId,
  });
}

/** 修改对话标题 */
export function updateConversationTitle(chatId: string, title: string) {
  return http.post<CommonResponse<null>>('/conversations/update-title', {
    chat_id: chatId,
    title,
  });
}

// ============================================================
// 对话框 MCP 管理
// ============================================================

export function listConversationMCPServers(chatId: string) {
  return http.post<CommonResponse<{ items: Array<{ server_id: string; name: string; enabled: boolean }> }>>(
    '/conversations/mcp/list',
    { chat_id: chatId }
  );
}

export function addConversationMCP(chatId: string, serverIds: string[]) {
  return http.post<CommonResponse<null>>('/conversations/mcp/add', { chat_id: chatId, server_ids: serverIds });
}

export function removeConversationMCP(chatId: string, serverIds: string[]) {
  return http.post<CommonResponse<null>>('/conversations/mcp/remove', { chat_id: chatId, server_ids: serverIds });
}

// ============================================================
// 对话框 Skill 管理
// ============================================================

export function listConversationSkills(chatId: string) {
  return http.post<CommonResponse<{ items: SkillRef[] }>>('/conversations/skills/list', { chat_id: chatId });
}

export function addConversationSkill(chatId: string, skillIds: string[]) {
  return http.post<CommonResponse<null>>('/conversations/skills/add', { chat_id: chatId, skill_ids: skillIds });
}

export function removeConversationSkill(chatId: string, skillIds: string[]) {
  return http.post<CommonResponse<null>>('/conversations/skills/remove', { chat_id: chatId, skill_ids: skillIds });
}

// ============================================================
// 任务接口
// ============================================================

export function getTaskDetail(taskId: string) {
  return http.post<CommonResponse<TaskDetail>>('/tasks/detail', { task_id: taskId });
}

export function stopTask(taskId: string) {
  return http.post<CommonResponse<null>>('/tasks/stop', { task_id: taskId });
}

/** 重试失败/终止的任务（工作流任务重置 DAG、对话任务从头重新生成） */
export function retryTask(taskId: string) {
  return http.post<CommonResponse<null>>('/tasks/retry', { task_id: taskId });
}

// ============================================================
// 调试相关：拉取任务的所有 turn
// ============================================================

/** 单条 turn 的调试视图 —— 与后端 TurnDebugView 对应 */
export interface TurnDebugView {
  id: number;
  task_id: string;
  turn_seq: number;
  chat_id: string;
  parent_task_id?: string;
  /** 调用类型：2-functional_call 3-tool_call 4-workflow */
  turn_type: number;
  turn_type_name: string;
  /** 语义阶段：1-规划期 2-执行期。与 turn_type 正交，两者需同时展示 */
  phase: number;
  phase_name: string;
  stage: number;
  stage_name: string;
  status: number;
  status_name: string;
  round: number;
  model?: string;
  tool_name?: string;
  server_name?: string;
  total_tokens: number;
  /**
   * 输入 / 输出 token 拆分。
   * 输入侧（prompt）才是上下文水位的度量 —— 系统提示、历史消息、工具 schema 全在里面；
   * 输出侧只反映回答长度。分开看才能判断「这一轮是上下文撑大了，还是回答写长了」。
   */
  prompt_tokens: number;
  completion_tokens: number;
  input?: unknown;
  input_raw?: string;
  output?: unknown;
  output_raw?: string;
  error_msg?: string;
  start_ts: number;
  end_ts: number;
  time_cost_ms: number;
  /**
   * 由该 turn 派生的子任务 task_id（典型：deep_search 子任务）。
   * 调试页把它放在 turn 行尾部作为子任务调试入口；
   * 老数据无映射时为空，按钮不显示。
   */
  sub_task_id?: string;
}

/** 任务的调试摘要 */
export interface TaskDebugSummary {
  task_id: string;
  chat_id: string;
  parent_task_id?: string;
  task_type: number;
  task_type_name: string;
  task_status: number;
  status_name: string;
  stage: number;
  stage_name: string;
  /** 任务当前的语义阶段（1-规划期 2-执行期） */
  phase: number;
  phase_name: string;
  turn_count: number;
  progress: number;
  create_ts: number;
  start_ts: number;
  end_ts: number;
  /**
   * 含子任务的总量（自身 turn + 所有后代子任务）。
   *
   * deep_search 这类工具会派生独立子任务去做多轮「搜索 → 抓正文」，
   * 消耗全记在子任务名下，只按自身 task_id 聚合会严重低估真实开销。
   */
  total_tokens: number;
  /** 本任务自身 turn 的消耗（不含子任务） */
  own_tokens: number;
  /** 所有后代子任务的消耗之和 */
  sub_task_tokens: number;
  /** 其中由 deep_search 子任务消耗的部分（sub_task_tokens 的子集，通常是大头） */
  deep_search_tokens: number;
  /** 输入 / 输出拆分（含子任务）。prompt 是上下文水位的度量对象 */
  prompt_tokens: number;
  completion_tokens: number;
  input_text: string;
  output_text?: string;
  failed_reason?: { code: number; message: string } | null;
}

export function listTaskTurns(taskId: string) {
  return http.post<CommonResponse<{
    task: TaskDebugSummary;
    turns: TurnDebugView[];
    /** 未映射到任何 turn 的子任务（旧数据无 parent_turn_id），退回顶部列表兜底展示 */
    unmapped_sub_tasks?: SubTaskSummary[];
  }>>(
    '/tasks/turns',
    { task_id: taskId },
  );
}

/** 子任务摘要 —— 父任务页里渲染子任务卡片用，字段与 TaskDebugSummary 对齐 */
export interface SubTaskSummary {
  task_id: string;
  chat_id: string;
  parent_task_id?: string;
  task_type: number;
  task_type_name: string;
  task_status: number;
  status_name: string;
  stage: number;
  stage_name: string;
  phase: number;
  phase_name: string;
  turn_count: number;
  progress: number;
  create_ts: number;
  start_ts: number;
  end_ts: number;
  total_tokens: number;
  /** 本任务自身 turn 的消耗（不含后代） */
  own_tokens: number;
  /** 后代子任务的消耗之和 */
  sub_task_tokens: number;
  /** 其中 deep_search 子任务消耗的部分 */
  deep_search_tokens: number;
  /** 输入 / 输出拆分 */
  prompt_tokens: number;
  completion_tokens: number;
  input_text: string;
  output_text?: string;
  failed_reason?: { code: number; message: string } | null;
  /** 子任务类型标记：deep_search 子任务会带 "deep_search"，普通 workflow 节点子任务为空 */
  sub_task_kind?: string;
}

/** 调试页用：拉取指定父任务的所有子任务摘要（用于在父任务页列出 deep_search / 节点子任务并跳转） */
export function listSubTasks(parentTaskId: string) {
  return http.post<CommonResponse<{ parent_task_id: string; list: SubTaskSummary[]; total: number }>>(
    '/tasks/sub-tasks',
    { parent_task_id: parentTaskId },
  );
}

// ============================================================
// 模型配置管理
// ============================================================

export function listModels() {
  return http.post<CommonResponse<{ list: ModelConfig[] }>>('/models/list', {});
}

// getModelStatus 查询是否存在可用模型。
// 应用入口据此决定要不要弹「必须先配置模型」的引导。
export function getModelStatus() {
  return http.post<CommonResponse<ModelStatus>>('/models/status', {});
}

export function createModel(config: {
  name: string;
  protocol: string;
  base_url: string;
  api_key: string;
  model: string;
  api_version?: string;
  enabled?: boolean;
  supports_image?: boolean;
  context_limit?: number;
}) {
  return http.post<CommonResponse<ModelConfig>>('/models/create', config);
}

export function updateModel(modelId: string, config: Partial<{
  name: string;
  protocol: string;
  base_url: string;
  api_key: string;
  model: string;
  api_version: string;
  enabled: boolean;
  supports_image: boolean;
  context_limit: number;
}>) {
  return http.post<CommonResponse<ModelConfig>>('/models/update', { id: modelId, ...config });
}

export function deleteModel(modelId: string) {
  return http.post<CommonResponse<null>>('/models/delete', { id: modelId });
}

export function setDefaultModel(modelId: string) {
  return http.post<CommonResponse<ModelConfig>>('/models/set-default', { id: modelId });
}

// TestModelResult 模型测试结果。
export interface TestModelResult {
  success: boolean;
  error_msg?: string;
  reply?: string;
  supports_image?: boolean;
  vision_error_msg?: string;
}

// testModel 测试模型：传 id 测已保存配置，或传直连字段测未保存的表单输入。
export function testModel(params: {
  id?: string;
  protocol?: string;
  base_url?: string;
  api_key?: string;
  model?: string;
  api_version?: string;
}) {
  return http.post<CommonResponse<TestModelResult>>('/models/test', params);
}

// ============================================================
// 系统设置 —— 数据库连接
//
// 配置保存在服务端的 config/db_config.json（不进 conf.yaml，密码也不随代码提交），
// 保存后立即热生效（scheduler 由设置文件 watcher 在 2s 内跟随）。
// ============================================================

export function getDBConfig() {
  return http.post<CommonResponse<DBConfig>>('/settings/db/get', {});
}

export function updateDBConfig(params: {
  driver: string;
  host?: string;
  port?: number;
  user?: string;
  password?: string;
  dbname?: string;
  charset?: string;
  path?: string;
}) {
  return http.post<CommonResponse<DBConfigUpdateResult>>('/settings/db/update', params);
}

/** 用页面上的参数试连（不落盘、不影响当前进程） */
export function testDBConfig(params: {
  driver: string;
  host?: string;
  port?: number;
  user?: string;
  password?: string;
  dbname?: string;
  charset?: string;
  path?: string;
}) {
  return http.post<CommonResponse<DBTestResult>>('/settings/db/test', params);
}

// ============================================================
// 检索引擎配置管理
// ============================================================

export function getSearchConfig() {
  return http.post<CommonResponse<SearchConfig>>('/search-config/get', {});
}

export function updateSearchConfig(params: {
  engine: string;
  bocha_key?: string;
  tavily_key?: string;
  serper_key?: string;
  tencent_wsa_key?: string;
  baidu_qianfan_key?: string;
}) {
  return http.post<CommonResponse<SearchConfig>>('/search-config/update', params);
}

// testSearch 检索连通性测试：用指定引擎跑一次真实搜索；需 key 引擎未带 key 时回落 DB。
export function testSearch(params: {
  engine: string;
  query: string;
  bocha_key?: string;
  tavily_key?: string;
  serper_key?: string;
  tencent_wsa_key?: string;
  baidu_qianfan_key?: string;
}) {
  return http.post<CommonResponse<SearchTestResult>>('/search-config/test', params);
}

// ============================================================
// MCP 服务器管理
// ============================================================

export function listMCPServers() {
  return http.post<CommonResponse<{ list: MCPServerConfig[] }>>('/mcp/servers/list', {});
}

export function getMCPServerDetail(serverId: string) {
  return http.post<CommonResponse<MCPServerConfig>>('/mcp/servers/detail', { server_id: serverId });
}

export function createMCPServer(config: Omit<MCPServerConfig, 'id' | 'create_ts' | 'update_ts' | 'status'>) {
  return http.post<CommonResponse<MCPServerConfig>>('/mcp/servers/create', config);
}

export function updateMCPServer(serverId: string, config: Partial<MCPServerConfig>) {
  return http.post<CommonResponse<MCPServerConfig>>('/mcp/servers/update', { server_id: serverId, ...config });
}

export function deleteMCPServer(serverId: string) {
  return http.post<CommonResponse<null>>('/mcp/servers/delete', { server_id: serverId });
}

export function testMCPServer(serverId: string) {
  return http.post<
    CommonResponse<{
      success: boolean;
      error_msg: string;
      tools?: Array<{ name: string; description: string; input_schema: string }>;
    }>
  >('/mcp/servers/test', { server_id: serverId });
}

// ============================================================
// 内置 MCP 清单
// ============================================================

export interface BuiltinMCPItem {
  key: string;
  name: string;
  description: string;
  /** 按界面语言折算的展示值（en 有翻译用翻译，否则回落中文）；列表渲染用这个 */
  name_display?: string;
  description_display?: string;
  transport: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
  already_imported: boolean;
}

export function listBuiltinMCP() {
  return http.post<CommonResponse<{ dir: string; list: BuiltinMCPItem[] }>>(
    '/mcp/servers/list-builtins',
    {}
  );
}

export function importBuiltinMCP(keys: string[], overwrite: boolean) {
  return http.post<
    CommonResponse<{ imported: string[]; updated: string[]; skipped: string[] }>
  >('/mcp/servers/import-builtins', { keys, overwrite });
}

// ============================================================
// Skill 管理
// ============================================================

export function listSkills() {
  return http.post<CommonResponse<{ list: SkillConfig[] }>>('/skills/list', {});
}

export function getSkillDetail(skillId: string) {
  return http.post<CommonResponse<SkillConfig>>('/skills/detail', { skill_id: skillId });
}

export function createSkill(config: Omit<SkillConfig, 'id' | 'create_ts' | 'update_ts'>) {
  return http.post<CommonResponse<SkillConfig>>('/skills/create', config);
}

export function updateSkill(skillId: string, config: Partial<SkillConfig>) {
  return http.post<CommonResponse<SkillConfig>>('/skills/update', { skill_id: skillId, ...config });
}

export function deleteSkill(skillId: string) {
  return http.post<CommonResponse<null>>('/skills/delete', { skill_id: skillId });
}

// ============================================================
// 内置 Skill 清单
// ============================================================

export interface BuiltinSkillItem {
  key: string;
  name: string;
  description: string;
  /** 按界面语言折算的展示值（en 有翻译用翻译，否则回落中文）；列表渲染用这个 */
  name_display?: string;
  description_display?: string;
  type: number;
  content?: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  already_imported: boolean;
}

export function listBuiltinSkill() {
  return http.post<CommonResponse<{ dir: string; list: BuiltinSkillItem[] }>>(
    '/skills/list-builtins',
    {}
  );
}

export function importBuiltinSkill(keys: string[], overwrite: boolean) {
  return http.post<
    CommonResponse<{ imported: string[]; updated: string[]; skipped: string[] }>
  >('/skills/import-builtins', { keys, overwrite });
}

export interface SkillZipImportItem {
  skill: SkillConfig;
  key: string;
  action: 'created' | 'updated' | 'skipped';
  files_in_zip: number;
  scripts_dir: string;
}

export interface SkillZipImportResult {
  skills: SkillZipImportItem[];
  total: number;
  total_files: number;
}

/** zip 整体导入 skill 包：支持 skill.json 单包 / SKILL.md 单包 / SKILL.md 集合包 */
export function importSkillZip(file: File, overwrite: boolean) {
  const form = new FormData();
  form.append('file', file);
  form.append('overwrite', String(overwrite));
  return http.post<CommonResponse<SkillZipImportResult>>('/skills/import-zip', form, {
    headers: { 'Content-Type': undefined as unknown as string },
    timeout: 120000,
  });
}

// ============================================================
// 任务流式订阅（SSE）
// ============================================================

/** 流式工具调用分片，与后端 llm.ToolCallDelta 对应 */
export interface ToolCallDelta {
  index: number;
  id?: string;
  name?: string;
  args?: string;
}

/** 后端 SSE 推送的一条事件，与 stream.Event 结构对应 */
export interface StreamEvent {
  type: 'snapshot' | 'turn_done' | 'task_done' | 'tool_calling' | 'delta' | 'compacting';
  task_id: string;
  turn_id?: number;
  turn_seq?: number;
  data?: {
    text?: string;
    tool_call?: ToolCallDelta;
    /** tool_calling 事件携带：正在被调用的工具名 */
    tool_name?: string;
    /** compacting 事件携带：start 开始压缩 / done 压缩结束 / progress 分块进度 */
    phase?: 'start' | 'done' | 'progress';
    /** compacting 事件携带：当前输入 token 总量 */
    tokens?: number;
    /** compacting 事件携带：当前模型的上下文上限 */
    limit?: number;
    /** compacting 事件携带（phase=progress）：分块压缩进度，如「第 3/7 段」 */
    stage?: string;
    [k: string]: unknown;
  };
}

/**
 * 订阅某个任务的 SSE 流，返回关闭函数。
 * EventSource 只支持 GET，后端 /tasks/stream 正是 GET，天然契合。
 *
 * 默认行为：收到 task_done 后会主动 es.close()。
 * 原因：服务端 TaskStream 在推送 task_done 后会 return 关连接，
 * 而原生 EventSource 在连接被关时会按 ≈3s 默认 retry 自动重连——这会形成
 * 「服务端 task_done → close → 浏览器重连 → 服务端再次 task_done → …」
 * 的死循环（每 3 秒一次 stream 请求，伴随 /turns 轮询放大成截图里的风暴）。
 * 任务结束后没理由继续订阅，所以一次性关闭掉。
 */
export function subscribeTaskStream(
  taskId: string,
  handlers: {
    /** delta 增量文本；ev 携带 turn_id/turn_seq 供按 turn 关联（调试页实时输出用） */
    onDelta?: (text: string, ev: StreamEvent) => void;
    /** 工具调用分片增量；ev 携带 turn_id/turn_seq，供调试页展示完整 raw 流式输出 */
    onToolCall?: (tc: ToolCallDelta, ev: StreamEvent) => void;
    /** 工具调用开始执行（turn 已建、尚未完成）；ev.data.tool_name 为工具名 */
    onToolCalling?: (ev: StreamEvent) => void;
    /**
     * 正在压缩上下文。ev.data.phase 为 start/done。
     * 压缩会阻塞本轮 LLM 调用数秒到数十秒，不给提示前端看起来就是卡住了。
     */
    onCompacting?: (ev: StreamEvent) => void;
    onTurnDone?: (ev: StreamEvent) => void;
    onTaskDone?: (ev: StreamEvent) => void;
    onSnapshot?: (ev: StreamEvent) => void;
    /**
     * 连接出错或被服务端关闭时触发，已默认不会自动重连。
     * 业务侧可在里面做一次性兜底刷新，然后视情况决定是否再起一个订阅。
     */
    onError?: (ev: Event) => void;
  },
): () => void {
  const es = new EventSource(`/api/v1/tasks/stream?task_id=${encodeURIComponent(taskId)}`);

  const parse = (e: MessageEvent): StreamEvent => {
    try {
      return JSON.parse(e.data) as StreamEvent;
    } catch {
      return { type: 'delta', task_id: taskId };
    }
  };

  es.addEventListener('delta', (e) => {
    const ev = parse(e as MessageEvent);
    const text = ev.data?.text;
    if (text) handlers.onDelta?.(text, ev);
    const tc = ev.data?.tool_call;
    if (tc) handlers.onToolCall?.(tc, ev);
  });
  es.addEventListener('turn_done', (e) => handlers.onTurnDone?.(parse(e as MessageEvent)));
  es.addEventListener('tool_calling', (e) => handlers.onToolCalling?.(parse(e as MessageEvent)));
  es.addEventListener('compacting', (e) => handlers.onCompacting?.(parse(e as MessageEvent)));
  es.addEventListener('snapshot', (e) => handlers.onSnapshot?.(parse(e as MessageEvent)));

  // task_done 后立即关闭 ES：避免浏览器自动重连 + 服务端反复返回 task_done 的死循环。
  // 注意 es.close() 会触发 onerror，但 readyState=2，浏览器不会再重连。
  es.addEventListener('task_done', (e) => {
    const ev = parse(e as MessageEvent);
    handlers.onTaskDone?.(ev);
    es.close();
  });

  // 其它错误（含服务端异常断开、网络抖动）统一走 onError，业务侧自行兜底。
  es.addEventListener('error', (e) => {
    // readyState === EventSource.CLOSED（2）说明连接被永久关闭，浏览器不再自动重连。
    // 为何 closed 是 "永久"：因为服务端 close 后，浏览器 EventSource 在 modern 里
    // 仍然会按默认 retry 重连——只是 retry 后会再次拿到 task_done + close → 死循环。
    // 因此这里主动 close 一次确保 readyState=CLOSE，彻底阻断裂链。
    if (es.readyState === EventSource.CLOSED) {
      try { es.close(); } catch { /* ES 已关，无视 */ }
    }
    handlers.onError?.(e);
  });

  return () => es.close();
}

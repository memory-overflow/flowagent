// ============================================================
// 通用类型 — 对应 proto 定义
// ============================================================

export type FileSource = 'local' | 'remote';

export enum FileType {
  IMAGE = 1,
  VIDEO = 2,
  TEXT = 3,
  OTHER = 4,
}

export interface FileInfo {
  source: FileSource;
  name: string;
  /** 相对工作区的路径，如 inputs/spec.md、outputs/article.md */
  path?: string;
  /** 下载地址（后端拼好的 /api/v1/files/download?...） */
  url?: string;
  size?: number;
  mime_type?: string;
  file_type?: FileType;
}

// ============================================================
// 任务相关
// ============================================================

export enum TaskStatus {
  PENDING = 1,      // 排队中
  RUNNING = 2,      // 进行中
  COMPLETED = 3,    // 已完成
  FAILED = 4,       // 任务失败
  CANCELLED = 5,    // 已终止（用户主动停止）
  PARTIAL_FAILED = 6, // 部分失败
  /**
   * 已废弃。**仅用于 DAG 节点**，不是任务级状态。
   * 上游失败导致本节点永远不会执行，已被后续规划划为历史。
   */
  ABANDONED = 7,
}

export enum TaskType {
  /** 对话任务：用户每发一条消息产生一条 */
  CONVERSATION = 1,
  /**
   * 工作流任务：一段 DAG。v1.4 起**一个对话可以有多条**，
   * 全集即 chat_id + task_type=2 的 task 行。
   */
  WORKFLOW = 2,
  /** 单 agent 子任务：工作流节点展开后的执行单元 */
  WORKFLOW_SUB_TASK = 3,
}

export interface TaskInputData {
  input_text: string;
  input_files: FileInfo[];
}

export interface ToolCallRecord {
  tool_name: string;
  server_name: string;
  /** 调用入参（JSON 字符串） */
  input: string;
  /** 工具返回内容 */
  output: string;
  /** 执行状态：1-执行中 2-成功 3-失败 */
  status: number;
  /** 耗时，如 "120ms" */
  time_cost: string;
  /**
   * 模型在工具调用 schema 的 summary 参数里写的一句话说明。
   * 优先作为 ToolCalls 块默认展示，让用户清楚「这次调用在做什么」。
   * 为空时前端降级为 server_name/tool_name 拼接。
   */
  summary?: string;
}

/** 一条思考过程展示单元：summary 默认展示、detail 点击展开 */
export interface ThinkStep {
  /** 默认展示的一句话说明（带 #序号 + 耗时前缀） */
  summary: string;
  /** 点击展开后的完整描述（工具名、入参、返回等）；为空表示无需展开 */
  detail?: string;
  /** 该步仍在执行中（turn 状态 running），前端据此显示「进行中」标记 */
  running?: boolean;
}

/** 分层思考过程：规划期 / 执行期，前端据此分组折叠展示 */
export interface ThinkLayers {
  // 后端用 omitempty 序列化，某一层为空时会缺字段，故这里置为可选，
  // 消费方一律用 `?? []` 兜底，避免 `undefined.length` 崩溃。
  planning?: ThinkStep[];
  execution?: ThinkStep[];
}

export interface AgentOutput {
  reply_text: string;
  output_files: FileInfo[];
  tool_calls: ToolCallRecord[];
  think_process: string[];
  think_layers?: ThinkLayers;
  /**
   * 本轮规划出的目标工作流任务 ID（仅规划移交工作流的对话回复携带）。
   * 前端据此渲染「查看工作流」跳转按钮；同时它是「该轮回复的状态
   * 已绑定到工作流节点」的标记 —— 此类消息不应显示对话任务重试按钮。
   */
  workflow_task_id?: string;
}

/**
 * token 消耗。工作流级与节点级通用，口径一致：**含子任务** ——
 * 节点内部派生的 deep_search 会再多一层子任务，消耗记在那一层名下，
 * 只看自身会严重低估。
 */
export interface TokenUsage {
  total_tokens: number;
  prompt_tokens: number;
  completion_tokens: number;
}

/**
 * 一段执行的时间信息。工作流级与节点级通用，口径一致：墙钟耗时，**含子任务** ——
 * 节点派生的 deep_search 子任务（孙层）会撑开节点的结束时间，与 token 同口径。
 */
export interface WorkflowDuration {
  /** 耗时毫秒。运行中任务的终点是当前时间，每次轮询都会变大 */
  duration_ms: number;
  /** 开始时间毫秒（优先任务真正开始执行，未调度时取创建时间） */
  start_ts: number;
  /** 结束时间毫秒；0 表示尚未结束 */
  end_ts: number;
  /** 是否仍在进行中。前端据此决定要不要本地秒跳 */
  running: boolean;
}

export interface WorkflowNode {
  node_id: string;
  /** 节点名称。注意后端字段是 name，不是 node_name */
  name: string;
  description: string;
  depends_on: string[] | null;
  /** 该节点对应的子任务 ID，尚未派发时为空 */
  task_id?: string;
  status: TaskStatus;
  input?: TaskInputData;
  output?: AgentOutput | null;
  /** 提出该节点的对话任务 ID —— 用于回溯「哪一轮规划出来的」 */
  source_task_id?: string;
  create_ts?: number;
  /**
   * 是否已被归档：失败分支被后续规划划为历史，不再参与调度与成败/进度判定，
   * 但仍留在 DAG 里供追溯。前端应灰显 + 虚线边框，而不是把它删掉。
   *
   * 与 status 正交：status 表达执行结果，archived 表达「本轮及以后都不再管它」。
   * 进度分母、节点完成数一律排除 archived 节点。
   */
  archived?: boolean;
  /** 归档原因（tooltip 展示），如「上游节点失败，分支已废弃」 */
  archive_reason?: string;
  /**
   * 该节点子树的 token 消耗（节点子任务自身 + 它派生的 deep_search 子任务）。
   * 节点尚未派发（无 task_id）时缺失。
   */
  token_usage?: TokenUsage;
  /**
   * 该节点的执行时长（节点子任务自身 + 它派生的 deep_search 子任务）。
   * 节点尚未派发（无 task_id）时缺失。
   */
  duration?: WorkflowDuration;
}

/** 任务级 workflow 输出（task_type = WORKFLOW 的 task_output） */
export interface WorkflowOutput {
  plan_summary: string;
  nodes: WorkflowNode[];
  /** 整段工作流的产物清单（各子任务 output_files 的并集） */
  output_files?: FileInfo[];
}

/**
 * 对话「当前激活」的那条工作流的完整 DAG 视图。
 * 由 GetConversationDetail 的 chat_info.workflow 返回，一条工作流都没有时缺失。
 *
 * v1.4 起一个对话可有多条工作流，本结构是其中被激活的那一条 ——
 * 全集看 chat_info.workflow_list，切换用 POST /workflows/set-active。
 */
export interface ConversationWorkflow {
  /** 工作流任务 ID */
  task_id: string;
  /** 工作流短标题（模型生成），切换器用它区分多条工作流 */
  title?: string;
  task_status: TaskStatus;
  status_name: string;
  stage: number;
  stage_name: string;
  progress: number;
  plan_summary: string;
  nodes: WorkflowNode[];
  /** 整段工作流的产物清单（各子任务 output_files 的并集，恒为数组） */
  output_files?: FileInfo[];
  create_ts: number;
  update_ts: number;
  failed_reason?: FailedReason | null;
  /**
   * 整条工作流的 token 消耗合计 = 规划期调用 + 各节点子树（含节点的
   * deep_search 子任务）。规划阶段还没烧 token 时为 0。
   */
  token_usage?: TokenUsage;
  /**
   * 整条工作流的执行时长（规划期 + 各节点执行的墙钟时间）。
   * 多轮追加需求时，这段墙钟里也包含等待用户补充需求的时间。
   */
  duration?: WorkflowDuration;
}

/**
 * 工作流列表项（切换器用）—— 比 ConversationWorkflow 轻，
 * 只带「够用户区分和选择」的信息，不含完整节点。
 */
export interface WorkflowListItem {
  task_id: string;
  /** 1-based 展示序号，按创建时间升序，稳定不随状态跳变 */
  index: number;
  /** 工作流短标题；模型没给出时后端回落为「工作流 N」 */
  title: string;
  task_status: TaskStatus;
  status_name: string;
  progress: number;
  /** 是否为当前激活的工作流（前端默认高亮） */
  active: boolean;
  /** 节点总数（含已归档） */
  node_total: number;
  /** 未归档节点数 —— 真正参与调度与进度分母的那些 */
  node_active: number;
  node_done: number;
  node_failed: number;
  /** 已归档节点数（失败节点及其后代） */
  node_archived: number;
  /** 是否可作为下一轮规划的续接对象 */
  can_append: boolean;
  /** 不可续接的原因，可直接展示（可续接时为空） */
  append_reason?: string;
  /** 可复用节点数：续接时能直接吃到的历史成果 */
  reusable_count: number;
  plan_summary?: string;
  create_ts: number;
  update_ts: number;
}

export interface FailedReason {
  code: number;
  message: string;
}

export interface TaskDetail {
  task_id: string;
  chat_id: string;
  parent_task_id: string;
  task_type: TaskType;
  task_input: TaskInputData;
  task_status: TaskStatus;
  progress: number;
  failed_reason: FailedReason | null;
  create_ts: number;
  start_ts: number;
  end_ts: number;
  time_cost: string;
  /** 总耗时毫秒数，从任务创建算起（含 planning 与各阶段调度等待） */
  time_cost_ms: number;
  /**
   * token 消耗（含子任务）。
   *
   * deep_search 会派生独立子任务做多轮「搜索 → 抓正文」，消耗全记在子任务
   * 名下 —— 实测一次带检索的对话，子任务能占到总量的 85%+。所以这里的
   * total 是「自身 + 所有子任务」，并把 sub_task_tokens 单独给出。
   *
   * 老后端不返回这些字段，故全部可选，前端用 `?? 0` 兜底。
   */
  total_tokens?: number;
  /** 本任务自身 turn 的消耗（不含子任务） */
  own_tokens?: number;
  /** 子任务消耗合计（典型来源：deep_search 检索子任务） */
  sub_task_tokens?: number;
  /** 输入 token（含子任务）。上下文水位看的是这一项，不是 total */
  prompt_tokens?: number;
  /** 输出 token（含子任务） */
  completion_tokens?: number;
  agent_output: AgentOutput | null;
  workflow_output: WorkflowOutput | null;
}

// ============================================================
// 对话相关
// ============================================================

export interface MCPRef {
  server_id: string;
  name: string;
  enabled: boolean;
}

export interface SkillRef {
  skill_id: string;
  name: string;
  enabled: boolean;
}

export interface ConversationInfo {
  chat_id: string;
  title: string;
  status: ConversationStatus;
  create_user: string;
  create_ts: number;
  update_ts: number;
  task_count?: number;
  mcp_servers?: MCPRef[];
  skills?: SkillRef[];
  /**
   * 当前激活的工作流任务 ID。
   * v1.4 起一个对话可有多条工作流，本字段只表示默认展示的那条 ——
   * 全集看 workflow_list。一条都没有时缺失。
   */
  workflow_task_id?: string;
  /** 同 workflow_task_id，语义更明确的别名 */
  active_workflow_task_id?: string;
  /** 该对话下全部工作流（切换器数据源），一条都没有时缺失 */
  workflow_list?: WorkflowListItem[];
  /** 当前激活那条工作流的完整 DAG（含节点），一条都没有时缺失 */
  workflow?: ConversationWorkflow;
  /** 当前选用的模型配置 ID，为空表示用默认模型 */
  model_id?: string;
  /** 是否有正在执行中的任务或工作流（排队中 / 进行中） */
  has_running_task?: boolean;
  /** 是否有失败的任务或工作流（失败 / 部分失败） */
  has_failed_task?: boolean;
}

export enum ConversationStatus {
  ACTIVE = 1,
  ARCHIVED = 2,
  DELETED = 3,
}

export interface ConversationDetail {
  chat_info: ConversationInfo;
  tasks: TaskDetail[];
}

// ============================================================
// MCP 服务器相关
// ============================================================

export enum MCPServerStatus {
  ACTIVE = 1,
  INACTIVE = 2,
  ERROR = 3,
}

// transport 字段约定为字符串: "stdio" | "sse" | "streamable_http"（与后端/proto 一致）
export type MCPTransport = 'stdio' | 'sse' | 'streamable_http';

export const MCPTransportLabel: Record<MCPTransport, string> = {
  stdio: 'stdio',
  sse: 'SSE',
  streamable_http: 'Streamable HTTP',
};

export interface MCPServerConfig {
  id?: string;
  name: string;
  transport: MCPTransport;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
  description: string;
  status: MCPServerStatus;
  create_ts?: number;
  update_ts?: number;
  /** 按界面语言折算的展示值（en 有翻译用翻译，否则回落中文）；列表渲染用这个，编辑回填用 name/description */
  name_display?: string;
  description_display?: string;
}

// ============================================================
// Skill 相关
// ============================================================

export enum SkillType {
  PROMPT = 1,   // 指令型：注入 system prompt
  COMMAND = 2,  // 可执行型：暴露为工具
}

export interface SkillConfig {
  id?: string;
  name: string;
  description: string;
  type: SkillType;
  content?: string;
  command?: string;
  // 后端列表返回 JSON 字符串，创建/更新时传数组/对象，这里用联合类型兼容
  args?: string[] | string;
  env?: Record<string, string> | string;
  create_ts?: number;
  update_ts?: number;
  /** 按界面语言折算的展示值（en 有翻译用翻译，否则回落中文）；列表渲染用这个，编辑回填用 name/description */
  name_display?: string;
  description_display?: string;
}

// ============================================================
// 模型配置相关
// ============================================================

// 模型接入协议：openai（OpenAI 兼容）/ gemini（Gemini 网关）/ gpt（Azure OpenAI）/ claude（Anthropic）
export type ModelProtocol = 'openai' | 'gemini' | 'gpt' | 'claude';

export const ModelProtocolLabel: Record<ModelProtocol, string> = {
  openai: 'OpenAI',
  gemini: 'Gemini',
  gpt: 'GPT (Azure)',
  claude: 'Claude',
};

export interface ModelConfig {
  id: string;
  name: string;
  protocol: ModelProtocol;
  base_url: string;
  api_key: string;
  model: string;
  api_version?: string;
  is_default: boolean;
  enabled: boolean;
  /** 是否支持图像理解（多模态），由「测试」带图探测回填 */
  supports_image?: boolean;
  /**
   * 最大输入 token 数（上下文上限）。
   * 上下文管理据此判定水位：超过 80% 触发历史压缩。
   * 0 / 未配置时后端回落到按模型名查内置表，再回落到保守默认值。
   */
  context_limit?: number;
  create_ts?: number;
  update_ts?: number;
}

/**
 * 「是否有可用模型」的轻量状态。
 *
 * 刻意不含模型明细：应用入口就要问这个状态（早于用户进入模型页），
 * 带明细等于把 api_key 无条件透给所有会话。
 */
export interface ModelStatus {
  /** 是否存在可用模型（enabled = true） */
  has_available: boolean;
  /** 模型配置总数（含已停用） */
  total: number;
  /** 可用（已启用）模型数 */
  available_count: number;
}

// ============================================================
// 检索引擎配置相关
// ============================================================

// 检索引擎：baidu（零 key 默认）/ ddg（零 key，大陆不可达）/ bocha / tavily / serper（需 key）
// + 国内云厂商独立搜索 API：tencent_wsa（腾讯云联网搜索）/ baidu_qianfan（百度千帆搜索）
export type SearchEngine =
  | 'baidu'
  | 'ddg'
  | 'bocha'
  | 'tavily'
  | 'serper'
  | 'tencent_wsa'
  | 'baidu_qianfan';

export const SearchEngineLabel: Record<SearchEngine, string> = {
  baidu: '百度',
  ddg: 'DuckDuckGo',
  bocha: '博查 BochaAI',
  tavily: 'Tavily',
  serper: 'Serper',
  tencent_wsa: '腾讯云联网搜索',
  baidu_qianfan: '百度千帆搜索',
};

/** 需 key 引擎的申请链接；零 key 引擎为空串 */
export const SearchEngineKeyURL: Record<SearchEngine, string> = {
  baidu: '',
  ddg: '',
  bocha: 'https://open.bochaai.com',
  tavily: 'https://tavily.com',
  serper: 'https://serper.dev',
  tencent_wsa: 'https://console.cloud.tencent.com/wsapi/index',
  baidu_qianfan: 'https://console.bce.baidu.com/qianfan/overview',
};

export interface SearchConfig {
  engine: SearchEngine;
  bocha_key?: string;
  tavily_key?: string;
  serper_key?: string;
  tencent_wsa_key?: string;
  baidu_qianfan_key?: string;
  update_ts?: number;
}

/** 检索连通性测试结果 */
export interface SearchTestResult {
  success: boolean;
  error_msg?: string;
  results?: string;
}

// ============================================================
// 系统设置 —— 数据库连接
// ============================================================

export type DBDriver = 'mysql' | 'sqlite';

/** 数据库连接配置（系统设置页编辑的对象） */
export interface DBConfig {
  driver: DBDriver;
  /** MySQL 地址 */
  host: string;
  port: number;
  user: string;
  /** 明文返回，便于本地编辑 */
  password: string;
  dbname: string;
  charset: string;
  /** sqlite 文件路径 */
  path: string;
  /** 设置文件落盘位置（config/db_config.json） */
  settings_path: string;
  /** 当前进程生效来源是否就是设置文件（false = 走 conf.yaml，或改完未重启） */
  from_settings: boolean;
  /** 当前进程实际在用的库（脱敏） */
  active_desc: string;
}

/** 保存结果 */
export interface DBConfigUpdateResult {
  ok: boolean;
  /** 兼容旧响应结构，恒为 false（保存即热生效，无需重启） */
  require_restart: boolean;
  msg: string;
  item?: DBConfig;
}

/** 连通性测试结果 */
export interface DBTestResult {
  success: boolean;
  error_msg?: string;
  desc?: string;
}

// ============================================================
// API 通用响应
// ============================================================

export interface CommonResponse<T> {
  code: number;
  msg: string;
  req_id: string;
  data: T;
}

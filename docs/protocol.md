# Workflow Agent 前后端交互协议 v1.8

## 版本变更

### v1.8（工作流执行时长可观测）

1. `conversations/detail` 的 `chat_info.workflow` 新增 `duration`
   （`duration_ms` / `start_ts` / `end_ts` / `running`），为整条工作流的墙钟耗时。
2. `workflow.nodes[]` 每个节点新增同结构的 `duration`，口径与 token 一致 ——
   **该节点子树**：取节点子任务与其 deep_search 孙任务的 min(开始) / max(结束)。
   节点尚未派发（无 `task_id`）时该字段缺失。
3. 终点判定：优先 `end_time`；未终态任务取「当前时间」（`running=true`、
   `end_ts=0`，前端本地秒跳）；终态却无 `end_time` 的历史脏数据退回 `updated_time`，
   避免耗时无限上涨。起点优先 `start_time`，未调度时退回 `created_time`
   （排队时间同样属于用户感知的等待）。
4. 前端：工作流详情页顶部展示 `⏱ 总时长`，节点详情面板展示「执行时长」，
   两者 hover 给出具体起止时刻。

### v1.7（工作流 token 消耗可观测）

1. `conversations/detail` 的 `chat_info.workflow` 新增 `token_usage`
   （`total_tokens` / `prompt_tokens` / `completion_tokens`），为整条工作流的
   合计：工作流任务规划期的调用 + 各节点子树 + 未登记进 DAG 的游离直接子任务（差值补齐）。
2. `workflow.nodes[]` 每个节点新增同结构的 `token_usage`，口径为**该节点子树** ——
   即节点子任务自身 + 它派生的 deep_search 子任务。只看节点自身会严重低估，
   因为 deep_search 的多轮「搜索 → 抓正文」消耗记在孙任务名下。
   节点尚未派发（无 `task_id`）时该字段缺失。
3. 前端：工作流详情页顶部展示整条工作流的 token 合计，节点详情面板展示该节点消耗
   （均带输入/输出拆分的 tooltip 与附文）。

### v1.6（主 agent 网页抓取受控化）

1. 主 agent（对话任务 / DAG 节点子任务）的工具集新增框架级 `fetch`（此前仅
   deep_search 子任务可见）。原因是实测发现没有正当入口时，模型会退回
   `run_command` + `curl` 自行抓网页：抓取次数更多、无 Markdown 转换与长度
   上限、且完全不可观测（单个调研对话中 run_command 115 次有 110 次是 curl）。
2. 配套两道护栏：每任务抓取配额（默认 8 次，超配额返回引导文本而非报错，
   引导模型改用 deep_search）、单次抓取 `max_length` 上限（20000 字符）。
   deep_search 子任务内的 fetch 不受此配额约束。
3. system prompt（执行期与规划期）新增「联网获取信息」规则：联网调研优先
   deep_search，仅明确 URL 需原文才用 fetch，禁止用 run_command 跑 curl/wget 抓网页。

### v1.5（上下文用量可观测）

1. 每次 LLM 调用的输入上下文按五块模型分块计量（system_prompt / tools /
   工作流摘要 / 已压缩摘要 / 未压缩历史详情 + 本轮输入与当前任务），
   随 turn input 快照落 `context_blocks` 字段（见 1.12 数据来源说明）。
2. 新增上下文用量查询接口（见 1.12）：前端在对话输入栏展示水位指示器，
   调试页在每个 LLM turn 卡片展示分块占比。

### v1.4（多工作流）

一个对话不再只绑定一条工作流，主要变化：

1. **一个对话可有多条工作流**。每条是一段相对独立的 DAG，全集以
   `chat_id + task_type=2` 的 task 行为准（不新增关联表）。
2. **规划期决定「续接」还是「新开」**。模型在候选清单里选，用户也可通过
   chat 接口的 `force_new_workflow` 显式要求新开（用户判断优先）。
3. **失败的工作流可以续接**。锚点限定为失败节点的上游已完成节点，
   失败节点及其全部后代被归档（灰显保留，不再参与调度与成败/进度判定）。
4. **用户主动停止的工作流不可续接** —— 停止说明规划本身有问题，无利用价值。
5. 新增 `WorkflowNode.archived` / `archive_reason` 与 `TaskStatus=7 已废弃`。
6. 新增工作流列表与激活态切换接口（见 1.10 / 1.11），前端据此做切换器。

v1.3 前端兼容：`workflow_task_id` / `workflow` 字段仍在，语义收敛为
「当前激活的那一条」。

## 约定

- 所有接口统一响应结构：`{ "code": 0, "msg": "ok", "req_id": "xxx", "data": {...} }`
  - `code`: 0 表示成功，非 0 为错误
  - `msg`: 错误码不为 0 时的错误描述
  - `req_id`: 请求唯一标识，用于日志追踪
  - `data`: 具体业务数据
- 时间戳统一为 Unix 毫秒时间戳，类型 `int64`
- 协议均为 HTTP/HTTPS，Content-Type: `application/json`
- MCP 相关内容均指 [Model Context Protocol](https://modelcontextprotocol.io/)

---

## 一、对话模块

### 1.1 对话消息结构定义

#### FileInfo — 文件信息

支持本地文件和远程文件两种模式，方便本地/远程部署场景。

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| source | 是 | string | 文件来源：`local` 本地文件，`remote` 远程文件 |
| name | 是 | string | 文件名 |
| path | 条件必选 | string | `source=local` 时必填，文件在服务器上的绝对路径 |
| url | 条件必选 | string | `source=remote` 时必填，文件访问 URL |
| size | 否 | int64 | 文件大小（字节） |
| mime_type | 否 | string | 文件 MIME 类型，如 `image/png`, `application/pdf` |
| file_type | 是 | int | 文件类型：1-图片，2-视频，3-文本，4-其他 |

**示例：**

```json
// 本地文件
{ "source": "local",  "name": "report.pdf", "path": "/data/files/report.pdf", "size": 2048000, "mime_type": "application/pdf", "file_type": 4 }

// 远程文件（图片）
{ "source": "remote", "name": "chart.png", "url":  "https://cdn.example.com/files/chart.png", "size": 512000, "mime_type": "image/png", "file_type": 1 }
```

#### TaskInput — 任务输入

| input_text | 否 | string | 用户输入的文本内容 |
| input_files | 否 | array of FileInfo | 用户上传的文件列表（通过 file_type 区分图片/视频/文本等） |
| force_new_workflow | 否 | bool | 仅对话任务：用户显式要求「新开一个工作流」完成本轮任务，见 1.3 |

#### TaskStatus — 任务状态

| 值 | 含义 | 描述 |
|----|------|------|
| 1 | QUEUED | 排队中 |
| 2 | RUNNING | 执行中 |
| 3 | COMPLETED | 已完成 |
| 4 | FAILED | 失败 |
| 5 | STOPPED | 用户主动终止 |
| 6 | PARTIAL_FAILED | 部分失败（工作流中部分节点失败） |
| 7 | ABANDONED | 已废弃。**仅用于 DAG 节点**，不用于任务：上游失败导致本节点永远不会执行 |

> 任务级「已终态」= 3/4/5/6；节点级额外把 7 也算终态（已被划为历史，不再调度）。

#### ToolCallRecord — MCP 工具调用记录

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| tool_name | 是 | string | 调用的工具名 |
| server_name | 是 | string | 所属 MCP 服务器名称 |
| input | 是 | string | 工具调用入参（JSON 字符串） |
| output | 是 | string | 工具调用结果（JSON 字符串） |
| status | 是 | int | 1-成功，2-失败 |
| time_cost | 是 | string | 耗时，如 "3.2s" |

#### TaskType — 任务类型

| 值 | 含义 | 描述 |
|----|------|------|
| 1 | CONVERSATION | 对话任务：用户发的每条消息（取值沿用历史的「单 agent 任务」） |
| 2 | WORKFLOW | 工作流任务：一段 DAG。**一个对话可有多条**，全集即切换器数据源 |
| 3 | WORKFLOW_SUB_TASK | 单 agent 子任务：工作流节点展开后的执行单元，通过 `parent_task_id` 挂回工作流 |

#### AgentOutput — 单 agent 任务输出

对应 `task_type=1`，agent 直接对话并返回。

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| reply_text | 是 | string | 模型回复的文本内容 |
| output_files | 否 | array of FileInfo | 模型输出的文件列表（通过 file_type 区分图片/视频/文本等） |
| tool_calls | 否 | array of ToolCallRecord | 本次任务中所有 MCP 工具调用记录 |
| think_process | 否 | array of string | 模型思考过程（可选展示） |

#### TokenUsage — token 消耗

工作流级与节点级通用，**口径一致：含子任务**。

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| total_tokens | 是 | int64 | 总消耗（输入 + 输出） |
| prompt_tokens | 是 | int64 | 输入侧合计。上下文水位看的就是这一列 |
| completion_tokens | 是 | int64 | 输出侧合计 |

> 为什么必须含子任务：节点内部调用 deep_search 时，真正的消耗（多轮
> 「检索 → 抓正文」，每轮都把整页正文塞进上下文）记在 deep_search 派生
> 的孙任务名下。只按节点子任务自身聚合会严重低估 —— 实测带检索的任务
> 子任务能占到总量的 85%+。

#### WorkflowDuration — 执行时长

工作流级与节点级通用，**口径一致：含子任务**，且是墙钟耗时（v1.8）。

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| duration_ms | 是 | int64 | 耗时毫秒。运行中的任务以当前时间为终点，每次轮询都会变大 |
| start_ts | 是 | int64 | 开始时间（毫秒）。优先任务真正开始执行，未调度时取创建时间 |
| end_ts | 是 | int64 | 结束时间（毫秒）。**0 表示尚未结束** |
| running | 是 | bool | 是否仍在进行中，前端据此决定是否本地秒跳 |

> 时长是墙钟而不是「净执行时间」：工作流多轮追加需求时，等待用户补充需求
> 的那段时间也包含在内。前端 tooltip 会明确标注这一点，避免被误读成
> 「agent 跑了 30 分钟」。

#### WorkflowNode — 工作流节点

DAG 中的一个节点，代表一个由父 agent 规划的子任务。最终会被调度为独立的 Task 执行。

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| node_id | 是 | string | 节点 ID（工作流内唯一） |
| name | 是 | string | 节点名称 |
| description | 是 | string | 该子任务要做什么 |
| input | 是 | TaskInput | 子任务的输入（父 agent 规划时填充） |
| depends_on | 是 | array of string | 依赖的前置节点 ID（拓扑排序依据） |
| task_id | 否 | string | 调度后关联的子任务 ID（执行后回填） |
| status | 否 | int | 节点执行状态（同 TaskStatus，执行后回填） |
| output | 否 | AgentOutput | 节点执行结果（子任务完成后回填） |
| source_task_id | 否 | string | 提出该节点的对话任务 ID，前端据此把节点关联回「哪一轮规划的」 |
| create_ts | 否 | int64 | 节点加入工作流的时间戳（毫秒） |
| archived | 否 | bool | 是否已被归档（失败分支被后续规划划为历史，前端灰显 + 虚线边框展示） |
| archive_reason | 否 | string | 归档原因，如「上游节点失败，分支已废弃」 |
| token_usage | 否 | TokenUsage | 该节点子树的 token 消耗（v1.7）。无 `task_id` 时不返回 |
| duration | 否 | WorkflowDuration | 该节点子树的执行时长（v1.8）。无 `task_id` 时不返回 |

> `archived` 与 `status` 正交：`status` 表达执行结果，`archived` 表达
> 「已被划为历史，本轮及以后都不再调度」。已归档节点仍在 `nodes` 里返回，
> 但**不计入**进度分母与成败判定 —— 否则历史失败节点会让续接后的工作流
> 一启动就被判失败。前端应按 `archived` 灰显，而不是把它们从 DAG 里删掉。

#### WorkflowOutput — 工作流 agent 任务输出

对应 `task_type=2`，agent 分析需求后产的 DAG 执行计划。

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| nodes | 是 | array of WorkflowNode | DAG 所有节点（节点列表 + depends_on 构成完整 DAG），含已归档节点 |
| plan_summary | 是 | string | 规划摘要（agent 对整体方案的文本说明） |
| tool_calls | 否 | array of ToolCallRecord | 规划阶段的 MCP 工具调用 |
| think_process | 否 | array of string | 规划阶段的思考过程 |
| output_files | 否 | array of FileInfo | 整段工作流的产物清单（各子任务 output_files 按 path 去重的并集） |
| title | 否 | string | 工作流短标题（模型生成，8~16 字），切换器按它区分多条工作流 |
| version | 否 | int64 | DAG 版本号（乐观锁）。读侧以 task 表的 `dag_version` 列为权威 |

#### TaskDetail — 任务详情

使用 `task_type` 区分任务类型，输出结构按类型选择 `agent_output` 或 `workflow_output`。

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| task_id | 是 | string | 任务 ID |
| chat_id | 是 | string | 所属对话 ID |
| parent_task_id | 否 | string | 父任务 ID（工作流子任务时有值） |
| task_type | 是 | int | 任务类型，见 TaskType |
| task_input | 是 | TaskInput | 任务输入 |
| task_status | 是 | int | 任务状态，见 TaskStatus（1~6；7 仅用于 DAG 节点） |
| progress | 是 | float | 任务进度百分比，范围 0.0 ~ 100.0 |
| failed_reason | 是 | string | 失败原因（成功时为空） |
| create_ts | 是 | int64 | 任务创建时间戳（毫秒） |
| start_ts | 否 | int64 | 任务开始执行时间戳（毫秒） |
| end_ts | 否 | int64 | 任务结束时间戳（毫秒） |
| time_cost | 是 | string | 任务耗时，如 "15.3s" |
| agent_output | 条件 | AgentOutput | task_type=1 时返回 |
| workflow_output | 条件 | WorkflowOutput | task_type=2 时返回 |

#### ConversationMCPServer — 对话关联的 MCP 服务器

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| server_id | 是 | string | MCP 服务器 ID |
| name | 是 | string | MCP 服务器名称 |
| enabled | 是 | bool | 是否启用 |

#### ConversationInfo — 对话摘要

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |
| title | 是 | string | 对话标题（创建时指定或自动生成） |
| status | 是 | int | 对话状态：1-进行中，2-已完成，3-已归档 |
| create_user | 是 | string | 创建人 |
| create_ts | 是 | int64 | 创建时间戳（毫秒） |
| update_ts | 是 | int64 | 最后更新时间戳（毫秒） |
| task_count | 是 | int | 对话中的任务数 |
| preview | 否 | string | 对话内容摘要（最后一条回复的前 N 字） |
| mcp_servers | 是 | array of ConversationMCPServer | 该对话关联的 MCP 服务器列表 |
| workflow_task_id | 否 | string | **当前激活**的工作流任务 ID（v1.4 起一个对话可有多条，本字段只表示默认展示的那条） |
| has_running_task | 是 | bool | 是否有排队中 / 执行中的任务或工作流 |
| has_failed_task | 是 | bool | 是否有失败 / 部分失败的任务或工作流 |

#### WorkflowListItem — 工作流列表项（切换器用）

比完整 DAG 轻，只带「够用户区分和选择」的信息。

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| task_id | 是 | string | 工作流任务 ID |
| index | 是 | int | 1-based 展示序号（按创建时间升序，稳定不随状态跳变） |
| title | 是 | string | 工作流短标题；模型没给出时回落到「工作流 N」 |
| task_status | 是 | int | 工作流任务状态，见 TaskStatus |
| status_name | 是 | string | 状态中文名 |
| progress | 是 | float | 进度百分比（分母只算未归档节点） |
| active | 是 | bool | 是否为当前激活的工作流（前端默认高亮） |
| node_total | 是 | int | 节点总数（含已归档） |
| node_active | 是 | int | 未归档节点数 |
| node_done | 是 | int | 已完成且未归档的节点数 |
| node_failed | 是 | int | 失败 / 部分失败且未归档的节点数 |
| node_archived | 是 | int | 已归档节点数（失败节点及其后代） |
| can_append | 是 | bool | 是否可作为下一轮规划的续接对象 |
| append_reason | 否 | string | 不可续接的原因，可直接展示（可续接时为空） |
| reusable_count | 是 | int | 可复用节点数：续接时能直接吃到的历史成果 |
| plan_summary | 否 | string | 规划摘要（tooltip 用） |
| create_ts | 是 | int64 | 创建时间戳（毫秒） |
| update_ts | 是 | int64 | 最后更新时间戳（毫秒） |

**可续接性（`can_append`）规则：**

| 工作流状态 | can_append | 锚点范围 |
|-----------|-----------|---------|
| 用户主动停止（5） | `false` | — 停止说明规划有问题，只能新开工作流 |
| 成功（3）/ 执行中（2）/ 排队中（1） | `true` | 全部未归档节点 |
| 失败（4）/ 部分失败（6） | `true`（有可复用节点时） | 仅**已完成**节点（= 失败节点的上游） |
| 有节点但一个都复用不了 | `false` | — 续接没意义，引导新开 |

---

### 1.2 创建对话

```
POST /api/v1/conversations/create
```

**说明**：创建一个新对话。创建时可指定对话标题和初始关联的 MCP 服务器。

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| title | 否 | string | 对话标题，不传则自动生成（如取首条消息摘要） |
| mcp_server_ids | 否 | array of string | 初始关联的 MCP 服务器 ID 列表 |

**请求示例：**

```json
{
  "title": "财报分析助手",
  "mcp_server_ids": ["mcp_001", "mcp_002"]
}
```

**响应 data：** 创建成功后的完整 `ConversationInfo`

---

### 1.3 发送消息（创建对话任务）

```
POST /api/v1/conversations/chat
```

**说明**：在已有对话中发送一条消息，创建异步任务执行。`chat_id` 为必填（需先通过 1.2 创建对话）。任务使用对话级别已配置的 MCP 服务器。任务类型由 agent 自动规划决定，调用方无需指定。

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |
| input_text | 否 | string | 用户输入的文本 |
| input_files | 否 | array of FileInfo | 输入的文件列表（通过 file_type 区分图片/视频/文本等） |
| model_id | 否 | string | 切换对话使用的模型；非空则持久化到 `conversation.model_id`，为空沿用当前模型 |
| force_new_workflow | 否 | bool | 用户显式要求「新开一个工作流」完成本轮任务 |

> `force_new_workflow`：一个对话可有多条工作流，规划期默认由模型在候选清单里
> 选「续接已有工作流」还是「新开工作流」。这个开关是给用户的显式入口 ——
> 用户最清楚本轮任务和之前那堆 DAG 有没有关系，所以**用户判断优先于模型判断**，
> 置 `true` 时规划期无条件新开。仅对本次任务生效，不落库。

**请求示例：**

```json
{
  "chat_id": "chat_abc123",
  "input_text": "帮我分析这份财报，并生成一份总结 PDF",
  "force_new_workflow": false,
  "input_files": [
    {
      "source": "local",
      "name": "2025年报.pdf",
      "path": "/data/uploads/report.pdf",
      "size": 2048000,
      "mime_type": "application/pdf",
      "file_type": 4
    }
  ]
}
```

**响应 data 结构：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| task_id | 是 | string | 任务 ID |

---

### 1.4 对话 MCP 管理

#### 1.4.1 查看对话 MCP 列表

```
POST /api/v1/conversations/mcp/list
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |

**响应 data：** `array of ConversationMCPServer`

#### 1.4.2 为对话添加 MCP 服务器

```
POST /api/v1/conversations/mcp/add
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |
| server_ids | 是 | array of string | 要添加的 MCP 服务器 ID 列表（去重，已存在的忽略） |

#### 1.4.3 从对话移除 MCP 服务器

```
POST /api/v1/conversations/mcp/remove
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |
| server_ids | 是 | array of string | 要移除的 MCP 服务器 ID 列表 |

---

### 1.5 获取任务详情

```
POST /api/v1/tasks/detail
```

**说明**：获取异步任务的执行结果。前端可使用轮询或在 WebSocket 收到通知后拉取。

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| task_id | 是 | string | 任务 ID |

**响应 data 结构：** `TaskDetail`（见 1.1 节）

---

### 1.6 停止任务

```
POST /api/v1/tasks/stop
```

**说明**：停止一个非终态任务，并**级联停止其整棵子树**里仍在排队/运行中的
后代任务（不限任务类型）。

级联必须覆盖孙层及更深：工作流节点子任务在执行中调 `deep_search` 时，
搜索子任务挂在节点子任务之下（孙层），只停直接子任务会让搜索在工作流停止后
继续被调度执行。同理，对话任务（single_agent）直接派发的 `deep_search`
子任务也在级联范围内。

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| task_id | 是 | string | 任务 ID |

---

### 1.7 拉取对话列表

```
POST /api/v1/conversations/list
```

**说明**：拉取用户的对话列表，使用瀑布流分页。

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| last_ts | 否 | int64 | 上一页最后一条的 `update_ts`，首次请求不传 |
| keyword | 否 | string | 对话标题模糊搜索 |
| page_size | 否 | int | 每页数量，默认 20 |

**响应 data 结构：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| list | 是 | array of ConversationInfo | 对话列表 |
| total | 是 | int | 总数量 |
| has_more | 是 | bool | 是否还有更多数据 |

---

### 1.8 获取对话详情

```
POST /api/v1/conversations/detail
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |

**响应 data 结构：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_info | 是 | ConversationInfo | 对话摘要信息（含工作流相关字段，见下） |
| tasks | 是 | array of TaskDetail | 该对话下所有**对话任务**详情（工作流任务不在其中） |

**`chat_info` 中的工作流字段：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| workflow_list | 否 | array of WorkflowListItem | 该对话下全部工作流（按创建时间升序），切换器数据源 |
| active_workflow_task_id | 否 | string | 当前激活的工作流任务 ID |
| workflow | 否 | object | 当前激活那条工作流的完整 DAG 视图 |
| workflow_task_id | 否 | string | 同 `active_workflow_task_id`，v1.3 兼容字段 |

> 该对话一条工作流都没有时，这四个字段都不出现，前端据此不渲染 DAG 面板。
> 只有 `workflow` 带完整节点；`workflow_list` 里的项只有统计信息，
> 切换到哪条再单独调 `workflows/list` 或重新拉详情。

**`workflow` 结构：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| task_id | 是 | string | 工作流任务 ID |
| task_status | 是 | int | 工作流任务状态 |
| status_name | 是 | string | 状态中文名 |
| stage / stage_name | 是 | int / string | 执行阶段 |
| progress | 是 | float | 进度百分比（分母只算未归档节点） |
| plan_summary | 是 | string | 规划摘要 |
| nodes | 是 | array of WorkflowNode | DAG 全部节点（恒为数组，无节点时为 `[]`） |
| output_files | 是 | array of FileInfo | 整段工作流的产物清单（恒为数组） |
| create_ts / update_ts | 是 | int64 | 创建 / 更新时间戳（毫秒） |
| failed_reason | 是 | string | 失败原因（成功时为空） |

---

### 1.9 删除对话

```
POST /api/v1/conversations/delete
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |

---

### 1.10 拉取对话的工作流列表

```
POST /api/v1/workflows/list
```

**说明**：一个对话可以有多条工作流，本接口返回全集 + 当前激活的 ID，
供前端切换器渲染。与 1.8 返回的 `chat_info.workflow_list` 是同一份数据 ——
切换工作流后只想局部刷新时调本接口，不必重拉整个对话详情。

> 路由挂在 `/api/v1/workflows` 下而非 `/conversations` 下，是因为列表/切换
> 操作的对象是工作流本身；归属仍由 `chat_id` 强校验。

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |

**响应 data 结构：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |
| active_workflow_task_id | 否 | string | 当前激活的工作流任务 ID（一条都没有时为空） |
| list | 是 | array of WorkflowListItem | 工作流列表，按创建时间升序 |
| total | 是 | int | 工作流条数 |

**响应示例：**

```json
{
  "code": 0,
  "msg": "ok",
  "data": {
    "chat_id": "chat_abc123",
    "active_workflow_task_id": "wf_002",
    "total": 2,
    "list": [
      {
        "task_id": "wf_001", "index": 1, "title": "竞品资料搜集",
        "task_status": 3, "status_name": "已完成", "progress": 100,
        "active": false, "node_total": 4, "node_active": 4,
        "node_done": 4, "node_failed": 0, "node_archived": 0,
        "can_append": true, "reusable_count": 4, "create_ts": 1750000000000
      },
      {
        "task_id": "wf_002", "index": 2, "title": "数据清洗与建模",
        "task_status": 6, "status_name": "部分失败", "progress": 40,
        "active": true, "node_total": 5, "node_active": 3,
        "node_done": 2, "node_failed": 1, "node_archived": 2,
        "can_append": true, "reusable_count": 2, "create_ts": 1750003600000
      }
    ]
  }
}
```

> `wf_002` 有 2 个节点已归档（失败节点及其后代）：它们仍在 DAG 里灰显，
> 但不计入 `node_active` 与进度分母。它仍可续接，锚点是那 2 个已完成节点。

---

### 1.11 切换当前激活的工作流

```
POST /api/v1/workflows/set-active
```

**说明**：切换用户当前**查看**的工作流。纯指针操作 —— 只改
`conversation.workflow_task_id`，不动 DAG、不改任务状态、不影响正在执行的调度。
用户可以随时切回任意一条历史工作流，包括已被别的任务续接过的。

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |
| task_id | 否 | string | 目标工作流任务 ID；**传空串表示取消激活**（前端收起 DAG 面板），不删除任何数据 |

**校验**：`task_id` 非空时后端会校验该任务存在、`chat_id` 匹配且
`task_type=2`，否则返回 400（防止挂别家对话的工作流或误传子任务 ID）。

**响应 data 结构：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |
| active_workflow_task_id | 是 | string | 切换后的激活 ID（取消激活时为空串） |

### 1.12 查询对话上下文用量

```
POST /api/v1/conversations/context-usage
```

**说明**：返回该对话**最近一次 LLM 调用**真实送出的输入分块明细，
以及当前所选模型的上下文上限与压缩水位线。数据读取自 turn input 快照的
`context_blocks` 字段（后端组装消息序列时用 tiktoken 实测计量后写入，
与上下文压缩模块的五块模型一致），**不是**前端或接口层的重新估算。
新对话尚无带分块明细的 LLM 调用时 `has_data=false`，`blocks` / `turn` 缺省。

只统计顶层对话任务（`parent_task_id` 为空）的 LLM 调用；
工作流节点 / deep_search 子任务的调用不混入对话水位。

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |

**响应 data 结构：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| chat_id | 是 | string | 对话 ID |
| model_name | 否 | string | 对话当前所选模型名 |
| context_limit | 是 | int | 模型最大输入 token 上限（口径=最大输入，非总窗口；漏配按内置表/128K 兜底） |
| trigger | 是 | int | 压缩触发线（上限的 80%），前端画刻度用 |
| target | 是 | int | 压缩目标线（上限的 30%） |
| has_data | 是 | bool | 是否有可展示的分块数据 |
| blocks | 否 | object | 分块明细，见下 |
| turn | 否 | object | 数据来源 turn 的元信息：`task_id` / `turn_seq` / `phase_name` / `model` / `status_name` / `create_ts` / `prompt_tokens`（provider 实测输入，可与 blocks.total 的估算对照） |

**blocks 结构**（六块之和 = total）：

| 参数 | 类型 | 描述 |
|------|------|------|
| system_prompt | int | 块1：agent 人格 / 工具指引 / 规划上下文（不可压缩） |
| tools | int | 块2：工具 JSON Schema，每轮全量重发（不可压缩） |
| workflow_summary | int | 块3：历史工作流折叠摘要（本身已是压缩产物，不可压缩） |
| compacted | int | 块4：上下文压缩生成的滚动历史摘要 |
| detail | int | 块5：尚未压缩的历史 task 交互明细 |
| user_and_current | int | 本轮用户输入 + 当前任务已发生的交互（永不压缩） |
| total | int | 六块之和 = 组装时的估算输入总量 |

---

## 二、MCP 管理模块

### 2.1 MCP 服务器结构定义

#### MCPServerConfig — MCP 服务器配置

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| id | 是 | string | 服务器 ID |
| name | 是 | string | 服务器名称，用于展示 |
| description | 否 | string | 服务器描述 |
| transport | 是 | string | 传输协议：`stdio` / `sse` / `streamable_http` |
| command | 否 | string | stdio 模式下的启动命令 |
| args | 否 | array of string | stdio 模式下的启动参数 |
| env | 否 | map[string]string | 环境变量 |
| url | 否 | string | sse / streamable_http 模式下的服务地址 |
| headers | 否 | map[string]string | HTTP 模式下的请求头 |
| status | 是 | int | 连接状态：1-已连接，2-未连接，3-连接失败 |
| tools_count | 是 | int | 该服务器提供的工具数量 |
| create_ts | 是 | int64 | 创建时间戳（毫秒） |
| update_ts | 是 | int64 | 更新时间戳（毫秒） |

---

### 2.2 获取 MCP 服务器列表

```
POST /api/v1/mcp/servers/list
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| keyword | 否 | string | 名称或描述模糊搜索 |

**响应 data 结构：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| list | 是 | array of MCPServerConfig | 服务器列表 |

---

### 2.3 获取 MCP 服务器详情

```
POST /api/v1/mcp/servers/detail
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| server_id | 是 | string | 服务器 ID |

---

### 2.4 创建 MCP 服务器

```
POST /api/v1/mcp/servers/create
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| name | 是 | string | 服务器名称 |
| description | 否 | string | 服务器描述 |
| transport | 是 | string | `stdio` / `sse` / `streamable_http` |
| command | 否 | string | stdio 命令 |
| args | 否 | array of string | stdio 参数 |
| env | 否 | map[string]string | 环境变量 |
| url | 否 | string | HTTP 地址 |
| headers | 否 | map[string]string | HTTP 请求头 |

**响应 data：** 创建成功后的完整 `MCPServerConfig`

---

### 2.5 更新 MCP 服务器

```
POST /api/v1/mcp/servers/update
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| server_id | 是 | string | 服务器 ID |
| name | 否 | string | 服务器名称 |
| description | 否 | string | 服务器描述 |
| transport | 否 | string | 传输协议 |
| command | 否 | string | stdio 命令 |
| args | 否 | array of string | stdio 参数 |
| env | 否 | map[string]string | 环境变量 |
| url | 否 | string | HTTP 地址 |
| headers | 否 | map[string]string | HTTP 请求头 |

---

### 2.6 删除 MCP 服务器

```
POST /api/v1/mcp/servers/delete
```

**请求体：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| server_id | 是 | string | 服务器 ID |

---

### 2.7 测试 MCP 服务器连接

```
POST /api/v1/mcp/servers/test
```

**说明**：尝试连接 MCP 服务器并获取其提供的工具列表，用于验证配置是否正确。

**请求体：** 同创建接口的请求体，或传入已有的 `server_id`

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| server_id | 否 | string | 已有服务器 ID（测试已保存的配置） |
| name | 否 | string | 以下字段与创建接口相同（测试新配置） |
| ... | | | |

**响应 data 结构：**

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| success | 是 | bool | 连接是否成功 |
| error_msg | 否 | string | 失败时的错误信息 |
| tools | 否 | array of MCPToolInfo | 连接成功后返回的工具列表 |

#### MCPToolInfo

| 参数 | 必选 | 类型 | 描述 |
|------|------|------|------|
| name | 是 | string | 工具名称 |
| description | 是 | string | 工具描述 |
| input_schema | 是 | object | 工具的 JSON Schema 入参定义 |

---

## 三、WebSocket 事件

前端可通过 WebSocket 接收实时推送（替代部分轮询场景）。

**连接地址：** `ws://<host>/api/v1/ws?token=<auth_token>`

### 事件类型

| 事件名 | 方向 | 描述 |
|--------|------|------|
| `task.status_changed` | 服务端→客户端 | 任务状态变更通知 |
| `task.progress` | 服务端→客户端 | 任务进度更新 |
| `chat.updated` | 服务端→客户端 | 对话更新（新消息到达等） |

### task.status_changed 数据结构

```json
{
  "event": "task.status_changed",
  "data": {
    "task_id": "task_xyz789",
    "chat_id": "chat_abc123",
    "task_status": 3,
    "progress": 100.0
  }
}
```

### task.progress 数据结构

```json
{
  "event": "task.progress",
  "data": {
    "task_id": "task_xyz789",
    "progress": 65.5
  }
}
```

---

## 四、通用错误码

| code | 描述 |
|------|------|
| 0 | 成功 |
| 1001 | 参数错误 |
| 1002 | 资源不存在 |
| 1003 | 无权限 |
| 1004 | 重复操作 |
| 2001 | 任务不存在 |
| 2002 | 任务已终止，不可操作 |
| 3001 | MCP 服务器不存在 |
| 3002 | MCP 连接失败 |
| 3003 | MCP 工具调用失败 |
| 5000 | 服务内部错误 |

---

## 附录 A：典型前端交互流程

### 新建对话并发送消息（单 agent 模式）

```
1. POST /api/v1/conversations/create  { title: "财报分析", mcp_server_ids: [...] }
   → { chat_id: "chat_001" }

2. POST /api/v1/conversations/chat  { chat_id: "chat_001", input_text: "分析这份年报" }
   → { task_id: "task_001", task_type: 1 }

3. 轮询 POST /api/v1/tasks/detail { task_id: "task_001" }
   或通过 WebSocket 接收通知
   → task_type=1, task_status=3 时读取 agent_output
```

### 规划期的三种处理模式

每条用户消息（对话任务）先进规划期，规划器调用 `submit_plan` 给出 `mode`：

| mode | 含义 | 对话任务后续 |
|------|------|--------------|
| `direct_reply` | 无需任何执行动作即可作答（闲聊、常识问答、基于上下文的解释、短文本改写等） | `submit_plan.reply` 直接落库为 `agent_output.reply_text`，**当轮完成**，不进执行期 |
| `single_agent` | 需要动手执行，但单个 Agent 循环即可完成 | 切执行期，继续 LLM ↔ 工具循环产出答案 |
| `workflow` | 需拆解为多步骤 DAG | 节点并入工作流任务，对话任务回复执行流程概述后完成 |

- `direct_reply` 的 `reply` 为空时视为协议违规，降级 `single_agent`。
- 前端无需区分：三种模式最终都以 `task_status=3` + `agent_output` 呈现；
  思考过程（`think_layers.planning`）会显示「任务规划：无需执行动作，直接回复」。
- 后续轮次的对话历史会把 `direct_reply` 的回复还原成普通 assistant 消息。

### 工作流模式 — 从规划到执行

```
1. POST /api/v1/conversations/chat
   { chat_id: "chat_001", input_text: "对比分析腾讯阿里财报，生成对比报告" }
   → { task_id: "task_001" }            // 这是一条对话任务（task_type=1）

2. 规划期由模型决定「新开 / 续接」工作流：
   → 判定为 workflow 时，后端创建（或选中）一条 task_type=2 的工作流任务 wf_001，
     并把 conversation.workflow_task_id 指向它（= 当前激活）
   → chat_info.workflow_list 随之多出一项，前端切换器自动出现「工作流 1」

3. 轮询对话详情 / 工作流任务：
   → task_status=2 (进行中) 时，workflow.nodes 已部分返回（规划完成）
       node_A: "获取腾讯财报"  depends_on: []
       node_B: "获取阿里财报"  depends_on: []
       node_C: "生成对比报告"   depends_on: ["node_A", "node_B"]
   → 调度器按拓扑顺序派发子任务: node_A(task_sub_1) 和 node_B(task_sub_2) 并行
   → 两个完成后派发 node_C(task_sub_3)
   → 全部完成后 wf_001 status=3，每个节点的 status 和 output 都已回填

4. 前端也可以直接轮询子任务:
   POST /api/v1/tasks/detail { task_id: "task_sub_1" }
   → parent_task_id: "wf_001", task_type: 3, agent_output: {...}
```

### 多轮对话下的工作流（v1.4）

```
第 1 轮："帮我搜集竞品资料"        → 模型新开 wf_001（3 节点，跑完）
第 2 轮："再补充一下海外竞品"      → 模型判定与 wf_001 相关 → 在 wf_001 上追加 2 个节点
第 3 轮："今天天气怎么样"          → 模型判定与 wf_001 无关 → 新开 wf_002（或走 single_agent）
                                    用户也可以点「新开工作流」开关强制走这条路

切换查看：
   POST /api/v1/workflows/list       { chat_id } → 拿到 wf_001 / wf_002 的列表
   POST /api/v1/workflows/set-active { chat_id, task_id: "wf_001" }
   POST /api/v1/conversations/detail { chat_id } → chat_info.workflow 变成 wf_001 的 DAG

失败工作流续接：
   wf_002 的 node_C 失败 → node_C 及其后代 node_D 被归档（archived=true，灰显）
   → wf_002 仍可续接，锚点只有 node_A / node_B（已完成的上游）
   → 新规划的节点只能 depends_on node_A / node_B，挂到 node_C 上会被后端拒绝并让模型重规划

用户主动停止：
   用户停止 wf_002 → 该工作流 can_append=false，后续只能新开工作流
```

### 为已有对话追加 MCP 工具

```
1. POST /api/v1/conversations/mcp/add  { chat_id: "chat_001", server_ids: ["mcp_003"] }

2. 后续该对话的所有消息都会带上新添加的 MCP 工具
```

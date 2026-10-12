# 核心 API 参考（作为核心 Agent 引擎接入）

> 本文是「方式二：把 flowagent 当作你项目的 agent 内核，通过 HTTP API 驱动」的完整协议说明。
> 涵盖核心接口的**输入/输出协议**与**使用方法**，包含可直接运行的 curl 示例。
> 若想了解内部实现，请看 [技术架构与实现理念](architecture.md)。

---

## 1. 概述

flowagent 通过一组 REST API 暴露「对话 → 规划 → 执行」的完整能力。核心交互模型是**异步任务**：

```
① 创建对话（得到 chat_id）
② 发消息（异步创建任务，立即返回 task_id）
③ 轮询 / SSE 流式获取任务结果（task_status=3 时读 agent_output / workflow_output）
```

**服务地址**：`http://<host>:8080`（默认端口，见 `config/config.yaml` 的 `server.port`）。

**任务分两种产出形态**，由 `task_type` 决定：

| task_type | 含义 | 结果字段 |
|---|---|---|
| 1 | 对话任务（单 agent 直接处理，理解 → 工具调用 → 回复） | `agent_output` |
| 2 | 工作流任务（先规划 DAG，再并行执行子任务） | `workflow_output` |
| 3 | 单 Agent 子任务（工作流节点 / deep_search 的执行单元） | `agent_output` |

> 一般情况下你只需要关心「发消息后拿到的 task_id → 轮询它的 `task_status` → 读 `agent_output` 或跳转到对应工作流」。任务类型由引擎自动判定，调用方无需指定。

---

## 2. 最小接入（三步）

```bash
BASE=http://localhost:8080/api/v1

# ① 创建对话
curl -s -X POST $BASE/conversations/create \
  -H 'Content-Type: application/json' \
  -d '{"title":"我的任务"}'
# → {"code":0,"msg":"ok","data":{"chat_id":"<CHAT_ID>", ...}}

# ② 发送消息（异步，返回 task_id）
curl -s -X POST $BASE/conversations/chat \
  -H 'Content-Type: application/json' \
  -d '{"chat_id":"<CHAT_ID>","input_text":"调研 X 并生成报告"}'
# → {"code":0,"msg":"ok","data":{"task_id":"<TASK_ID>"}}

# ③ 轮询任务详情（直到 task_status 为终态）
curl -s -X POST $BASE/tasks/detail \
  -H 'Content-Type: application/json' \
  -d '{"task_id":"<TASK_ID>"}'
```

或者用 SSE 流式接收执行过程（见 [§6 流式](#6-sse-流式接收)）。

---

## 3. 通用约定

### 3.1 统一响应结构

所有接口统一返回 JSON 信封：

```json
{ "code": 0, "msg": "ok", "req_id": "xxx", "data": { ... } }
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `code` | int | 0 表示成功，非 0 为错误（见 §7 错误码） |
| `msg` | string | `code != 0` 时的错误描述 |
| `req_id` | string | 请求唯一标识，用于日志追踪 |
| `data` | object | 具体业务数据 |

- 除 SSE 与文件上传/下载外，所有接口均为 `POST`，`Content-Type: application/json`。
- 时间戳统一为 Unix **毫秒**（`int64`）。

### 3.2 FileInfo — 文件信息

支持本地文件（`source=local`）与远程文件（`source=remote`）两种模式：

| 参数 | 必选 | 类型 | 说明 |
|---|---|---|---|
| `source` | 是 | string | `local` 本地文件 / `remote` 远程文件 |
| `name` | 是 | string | 文件名 |
| `path` | 条件 | string | `source=local` 时必填，服务器上的绝对路径 |
| `url` | 条件 | string | `source=remote` 时必填，文件访问 URL |
| `size` | 否 | int64 | 文件大小（字节） |
| `mime_type` | 否 | string | 如 `image/png`、`application/pdf` |
| `file_type` | 是 | int | 1-图片，2-视频，3-文本，4-其他 |

```json
{ "source": "remote", "name": "chart.png", "url": "https://cdn.example.com/chart.png",
  "size": 512000, "mime_type": "image/png", "file_type": 1 }
```

### 3.3 TaskStatus — 任务状态

| 值 | 含义 |
|---|---|
| 1 | `queued` 排队中 |
| 2 | `running` 执行中 |
| 3 | `completed` 已完成 |
| 4 | `failed` 失败 |
| 5 | `stopped` 用户主动终止 |
| 6 | `partial_failed` 部分失败（工作流中部分节点失败） |

> 任务级「终态」= 3/4/5/6。轮询时以这些值为停止条件。

---

## 4. 核心数据结构

### 4.1 TaskDetail — 任务详情（`tasks/detail` 返回）

| 字段 | 类型 | 说明 |
|---|---|---|
| `task_id` | string | 任务 ID |
| `chat_id` | string | 所属对话 ID |
| `parent_task_id` | string | 父任务 ID（工作流子任务时有值） |
| `task_type` | int | 1/2/3，见 §1 |
| `task_input` | object | 任务输入（`input_text` + `input_files`） |
| `task_status` | int | 见 §3.3 |
| `progress` | float | 进度 0.0~100.0 |
| `failed_reason` | object | 失败原因（成功时为空） |
| `create_ts` / `start_ts` / `end_ts` | int64 | 创建/开始/结束时间戳（毫秒） |
| `time_cost` | string | 耗时，如 `"15.3s"` |
| `agent_output` | object | `task_type=1` 时返回（见 §4.2） |
| `workflow_output` | object | `task_type=2` 时返回（见 §4.3） |

### 4.2 AgentOutput — 单 agent 任务输出

| 字段 | 类型 | 说明 |
|---|---|---|
| `reply_text` | string | 模型回复的文本内容 |
| `output_files` | array | 模型输出的文件列表（FileInfo） |
| `tool_calls` | array | 本次任务中所有 MCP 工具调用记录 |
| `think_process` | array | 思考过程（可读文本） |

**ToolCallRecord** 结构：

| 字段 | 类型 | 说明 |
|---|---|---|
| `tool_name` | string | 调用的工具名 |
| `server_name` | string | 所属 MCP 服务器名 |
| `input` | string | 工具入参（JSON 字符串） |
| `output` | string | 工具结果（JSON 字符串） |
| `status` | int | 1-成功，2-失败 |
| `time_cost` | string | 耗时 |
| `summary` | string | 模型对本次调用的一句话说明 |

### 4.3 WorkflowOutput — 工作流任务输出

| 字段 | 类型 | 说明 |
|---|---|---|
| `nodes` | array | DAG 全部节点（含已归档节点） |
| `plan_summary` | string | 规划摘要（整体方案的文本说明） |
| `tool_calls` | array | 规划阶段的工具调用 |
| `think_process` | array | 规划阶段的思考过程 |
| `output_files` | array | 整段工作流的产物清单（各子任务 output_files 按 path 去重并集） |
| `title` | string | 工作流短标题（8~16 字） |

**WorkflowNode（`nodes[]`）** 结构：

| 字段 | 类型 | 说明 |
|---|---|---|
| `node_id` | string | 节点 ID（工作流内唯一） |
| `name` | string | 节点名称 |
| `description` | string | 该子任务要做什么 |
| `input` | object | 子任务的输入 |
| `depends_on` | array | 依赖的前置节点 ID（拓扑排序依据） |
| `task_id` | string | 调度后关联的子任务 ID（执行后回填） |
| `status` | int | 节点执行状态（TaskStatus） |
| `output` | object | 节点执行结果（子任务完成后回填，AgentOutput） |
| `archived` | bool | 是否已归档（失败分支，灰显、不参与调度与统计） |

### 4.4 ConversationInfo — 对话摘要

| 字段 | 类型 | 说明 |
|---|---|---|
| `chat_id` | string | 对话 ID |
| `title` | string | 对话标题 |
| `status` | int | 1-进行中，2-已完成，3-已归档 |
| `create_user` | string | 创建人 |
| `create_ts` / `update_ts` | int64 | 创建/更新时间戳 |
| `task_count` | int | 任务数 |
| `preview` | string | 最后一条回复的前 N 字 |
| `mcp_servers` | array | 关联的 MCP 服务器列表 |
| `workflow_task_id` | string | 当前激活的工作流任务 ID |
| `has_running_task` / `has_failed_task` | bool | 是否有执行中/失败的任务或工作流 |

---

## 5. 接口详解

### 5.1 对话模块

#### 5.1.1 创建对话

```
POST /api/v1/conversations/create
```

| 参数 | 必选 | 类型 | 说明 |
|---|---|---|---|
| `title` | 否 | string | 标题，不传则自动生成 |
| `mcp_server_ids` | 否 | array | 初始关联的 MCP 服务器 ID 列表 |

响应 `data`：完整 `ConversationInfo`。

```bash
curl -X POST $BASE/conversations/create -H 'Content-Type: application/json' \
  -d '{"title":"财报分析助手","mcp_server_ids":["mcp_001"]}'
```

#### 5.1.2 发送消息

```
POST /api/v1/conversations/chat
```

| 参数 | 必选 | 类型 | 说明 |
|---|---|---|---|
| `chat_id` | 是 | string | 对话 ID（需先创建） |
| `input_text` | 否 | string | 用户输入的文本 |
| `input_files` | 否 | array | 输入文件列表（FileInfo） |
| `model_id` | 否 | string | 切换对话使用的模型；非空则持久化，为空沿用当前模型 |
| `force_new_workflow` | 否 | bool | 用户显式要求「新开一个工作流」（跳过续接判定） |

响应 `data`：`{ "task_id": "<TASK_ID>" }`。

```bash
curl -X POST $BASE/conversations/chat -H 'Content-Type: application/json' \
  -d '{"chat_id":"<CHAT_ID>","input_text":"对比分析两家公司财报，生成对比报告"}'
```

#### 5.1.3 对话列表 / 详情 / 删除

```
POST /api/v1/conversations/list     # 瀑布流分页：{last_ts?, keyword?, page_size?} → {list, total, has_more}
POST /api/v1/conversations/detail   # {chat_id} → {chat_info, tasks}
POST /api/v1/conversations/delete   # {chat_id}
POST /api/v1/conversations/update-model  # {chat_id, model_id}
POST /api/v1/conversations/update-title  # {chat_id, title}
```

`detail` 的 `tasks` 是该对话下所有**对话任务**（task_type=1）详情，工作流任务不在其中（见 §5.3）。

#### 5.1.4 对话级 MCP / Skill 绑定

```
POST /api/v1/conversations/mcp/list      # {chat_id}
POST /api/v1/conversations/mcp/add       # {chat_id, server_ids[]}
POST /api/v1/conversations/mcp/remove    # {chat_id, server_ids[]}

POST /api/v1/conversations/skills/list   # {chat_id}
POST /api/v1/conversations/skills/add    # {chat_id, skill_ids[]}
POST /api/v1/conversations/skills/remove # {chat_id, skill_ids[]}
```

对话级绑定后，该对话后续所有消息都会带上这些 MCP 工具 / Skill。

### 5.2 任务模块

#### 5.2.1 获取任务详情

```
POST /api/v1/tasks/detail      # {task_id} → TaskDetail
```

#### 5.2.2 停止任务

```
POST /api/v1/tasks/stop        # {task_id}
```

停止一个非终态任务，并**级联停止其整棵子树**里仍在排队/运行中的后代任务（含 deep_search 孙层子任务）。

#### 5.2.3 重试任务

```
POST /api/v1/tasks/retry       # {task_id}
```

按任务类型分派：工作流任务保留已完成节点、重置失败节点及其下游；对话任务从失败发生的 stage 断点续跑。仅失败/终止态可重试。

#### 5.2.4 调试接口（可选）

```
POST /api/v1/tasks/turns       # {task_id} → 完整 turn 列表（调试页用）
POST /api/v1/tasks/sub-tasks   # {parent_task_id} → 子任务摘要列表（deep_search / 节点子任务）
```

### 5.3 工作流模块（一对话多工作流）

一个对话可有多条工作流。切分与切换：

```
POST /api/v1/workflows/list        # {chat_id} → {chat_id, active_workflow_task_id, list[], total}
POST /api/v1/workflows/set-active  # {chat_id, task_id?} → 切换当前查看的工作流（纯指针，不删数据）
```

`list` 返回的每项（`WorkflowListItem`）携带切换器所需统计信息：`task_id` / `title` / `task_status` / `progress` / `node_total` / `node_done` / `node_failed` / `node_archived` / `can_append` 等。

`set-active` 的 `task_id` 传空串表示取消激活（收起 DAG 面板）。

**失败工作流续接**：失败节点的下游会被归档（`archived=true`，灰显），续接时新节点的 `depends_on` 只能引用已完成的上游节点。

### 5.4 模型 / MCP / Skill / 文件 / 设置（管理面）

这些属于「配置管理面」，接入时按需调用：

| 模块 | 端点 | 说明 |
|---|---|---|
| 模型 | `POST /models/status` / `list` / `create` / `update` / `delete` / `set-default` / `test` | 全局模型配置 |
| MCP | `POST /mcp/servers/list` / `detail` / `create` / `update` / `delete` / `test` / `list-builtins` / `import-builtins` | MCP 服务器 CRUD、连接测试、内置清单导入 |
| Skill | `POST /skills/list` / `detail` / `create` / `update` / `delete` / `list-builtins` / `import-builtins` / `import-zip` | Skill CRUD、内置导入、zip 导入 |
| 文件 | `POST /files/upload`（multipart）、`GET /files/download`、`GET /files/open` | 工作区文件上传/下载/本地打开 |
| 设置 | `POST /settings/db/get` / `update` / `test`；`POST /search-config/get` / `update` / `test` | 数据库连接、检索引擎配置 |

---

## 6. SSE 流式接收

```
GET /api/v1/tasks/stream?task_id=<TASK_ID>
```

以 Server-Sent Events 推送任务执行过程（`text/event-stream`）。时序：

1. 校验任务存在，建立 SSE 连接；
2. 先推一条 `snapshot`（已完成的全部 turn，保证新连接立即看到历史）；
3. 订阅增量事件 `turn_done` / `task_done`；
4. 任务已终态 → 推 `task_done` 后关闭；否则心跳（`: ping`）保活。

每条事件形如：

```
event: turn_done
data: {"type":"turn_done","task_id":"...","turn_id":123,"turn_seq":3}
```

| 事件 | 说明 |
|---|---|
| `snapshot` | 连接建立时的全量 turn 快照 |
| `turn_done` | 一轮 LLM 调用 / 工具调用落库结束 |
| `task_done` | 任务到达终态 |
| `tool_calling` | 某工具调用开始执行 |
| `compacting` | 正在压缩上下文（会阻塞数秒到数十秒） |

> SSE 是「尽力而为」的实时通道，不保证不丢事件。前端在断连/缓冲溢出时应降级回轮询 `tasks/detail` 兜底。

用 curl 演示：

```bash
curl -N "http://localhost:8080/api/v1/tasks/stream?task_id=<TASK_ID>"
```

---

## 7. 错误码

| code | 描述 |
|---|---|
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

> HTTP 层校验失败（如缺少必填参数）时可能直接返回 400/404 状态码，`code` 字段随状态码一并给出，`msg` 为具体原因。

---

## 8. 端到端示例

### 8.1 单 agent 模式（简单问答 / 直接执行）

```bash
BASE=http://localhost:8080/api/v1

CHAT_ID=$(curl -s -X POST $BASE/conversations/create \
  -H 'Content-Type: application/json' -d '{"title":"资料整理"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["chat_id"])')

TASK_ID=$(curl -s -X POST $BASE/conversations/chat \
  -H 'Content-Type: application/json' \
  -d "{\"chat_id\":\"$CHAT_ID\",\"input_text\":\"把下面这份资料整理成要点\"}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["task_id"])')

# 轮询直到终态
curl -s -X POST $BASE/tasks/detail -H 'Content-Type: application/json' \
  -d "{\"task_id\":\"$TASK_ID\"}"
# → task_type=1, task_status=3 时，读 data.agent_output.reply_text
```

### 8.2 工作流模式（多步骤任务）

```bash
# 发一个需要拆解的任务
TASK_ID=$(curl -s -X POST $BASE/conversations/chat \
  -H 'Content-Type: application/json' \
  -d "{\"chat_id\":\"$CHAT_ID\",\"input_text\":\"对比分析腾讯和阿里财报，生成对比报告\"}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["task_id"])')

# 轮询对话详情，拿到工作流 DAG
curl -s -X POST $BASE/conversations/detail -H 'Content-Type: application/json' \
  -d "{\"chat_id\":\"$CHAT_ID\"}"
# → chat_info.workflow_task_id 指向工作流任务
# → 调 tasks/detail 拿 workflow_output.nodes（DAG 节点 + depends_on）
#    node_A"获取腾讯财报"、node_B"获取阿里财报" 并行，node_C"生成对比报告" 依赖两者
```

### 8.3 附带文件输入

```bash
curl -s -X POST $BASE/conversations/chat -H 'Content-Type: application/json' \
  -d '{
    "chat_id":"<CHAT_ID>",
    "input_text":"分析这份年报",
    "input_files":[{
      "source":"local","name":"2025年报.pdf","path":"/data/uploads/report.pdf",
      "size":2048000,"mime_type":"application/pdf","file_type":4
    }]
  }'
```

---

## 9. 相关文档

- [技术架构与实现理念](architecture.md)：内部实现分模块详解。
- [`api/v1/workflow_agent.proto`](../api/v1/workflow_agent.proto)：协议定义（proto 格式，gRPC 视角）。

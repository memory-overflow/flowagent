# flowagent

A conversation-driven workflow orchestration framework — turn natural-language conversations into parallel DAG task graphs, executed with built-in MCP tools, skills, and context engineering.

**flowagent 是一个对话驱动的全局工作流编排框架**：把自然语言对话规划成一张 DAG 子任务图，并行调度执行，前序节点的输出自动注入后续节点。Go 后端 + React 前端，内置 MCP 工具与 skill 生态。

## Why flowagent

1. **好用好接入的开源 agent 框架缺失**。目前没有一个好用的开源 agent 框架可供企业或个人直接接入——要么自己从零造轮子，要么只能绑定某家云厂商的云上 agent。flowagent 想做的，就是一个**好接入、功能完善的开源 agent 框架**。

2. **ReAct 串行太慢，DAG 并行加速长程任务**。现有 agent 大多是 ReAct 框架，规划出任务后只能一步步串行执行，长任务耗时长。flowagent 把任务规划成 **DAG**，让无依赖的子任务**并行执行**，显著加速长程任务的落地。

## Features

- **对话 → DAG 规划**：把一段自然语言需求规划成一张有向无环图（DAG），每个节点是一个子任务，`depends_on` 表达依赖关系。
- **并行子任务调度**：无依赖的子任务并行执行，上游节点的输出自动注入下游节点。
- **上下文工程**：压缩（compaction）、索引、召回（recall），维持长会话智能体的一致性。
- **MCP 工具协议**：通过 Model Context Protocol 接入外部工具，内置 stdio / SSE / streamable_http 三种传输。
- **内置 skill 系统**：自有一等 skill（docx / pptx / shell），并打包第三方 skill（pdfkit-py、minimax-xlsx）。
- **一对话多工作流**：一个 chat 可绑定多条工作流，节点支持增量更新与版本化。
- **真流式 + SSE**：真正的流式输出，通过 SSE 实时推送任务进度与结果。

## Architecture

```
┌──────────────┐   HTTP/SSE    ┌──────────────────┐
│   React 前端  │ ◄───────────► │   agent-server    │
│    (Vite)    │               │  (Gin, :8080)     │
└──────────────┘               └────────┬─────────┘
                                        │ HTTP 内部回调
                               ┌────────▼─────────┐
                               │  agent-scheduler   │
                               │  (Stage 调度器)    │
                               └────────┬─────────┘
                                        │
                        ┌───────────────┼───────────────┐
                        ▼               ▼               ▼
                     MySQL/SQLite     MCP 工具        Skills
```

- **agent-server**（`cmd/server`）：HTTP API、SSE 流式、对话/模型/工作流/MCP/skill 管理。
- **agent-scheduler**（`cmd/scheduler`）：独立进程，扫描任务表，驱动 DAG 子任务的规划与执行。
- **frontend**（`frontend/`）：React 18 + Vite，DAG 渲染基于 `@xyflow/react`。

## 两种使用方式

### 方式一：使用自带前端（开箱即用）

flowagent 自带一个完整的 Web UI，包含对话、工作流 DAG 可视化、模型配置、MCP/skill 管理等功能。

```bash
# 1. 准备配置
cp config/config.example.yaml config/config.yaml
# 编辑 config/config.yaml，填入数据库与模型信息（详见 Quick Start）

# 2. 启动后端（两个进程）
./bin/agent-server -config config/config.yaml
./bin/agent-scheduler -config config/config.yaml

# 3. 启动前端
cd frontend && npm install && npm run dev
```

打开前端地址即可对话、查看 DAG 工作流。

### 方式二：作为核心 agent 引擎（API 接入）

把 flowagent 当作你项目的「agent 内核」，通过 HTTP API 驱动。最小接入流程三步：

```bash
# ① 创建对话
curl -X POST http://localhost:8080/api/v1/conversations/create \
  -H 'Content-Type: application/json' \
  -d '{"title":"我的任务"}'
# → {"code":0,"data":{"chat_id":"<CHAT_ID>", ...}}

# ② 发送消息（异步创建任务，返回 task_id）
curl -X POST http://localhost:8080/api/v1/conversations/chat \
  -H 'Content-Type: application/json' \
  -d '{"chat_id":"<CHAT_ID>","input_text":"帮我调研一下 X 并生成报告"}'
# → {"code":0,"data":{"task_id":"<TASK_ID>"}}

# ③ 流式接收结果（SSE）或轮询任务详情
curl -N "http://localhost:8080/api/v1/tasks/stream?task_id=<TASK_ID>"
curl -X POST http://localhost:8080/api/v1/tasks/detail \
  -H 'Content-Type: application/json' \
  -d '{"task_id":"<TASK_ID>"}'
```

任务分两种类型（`task_type`）：

| 类型 | 含义 | 输出结构 |
|---|---|---|
| `SINGLE_AGENT` | 单 agent 直接处理（理解 → MCP 工具调用 → 返回） | `agent_output` |
| `WORKFLOW_AGENT` | 先规划 DAG，再并行执行子任务 | `workflow_output`（含 `nodes` 依赖图） |

## API 概览

所有接口位于 `/api/v1` 下，统一 JSON 包装 `{"code":0,"msg":"...","data":...}`。除 SSE 与文件上传/下载外均为 `POST`。

| 模块 | 端点 | 说明 |
|---|---|---|
| 对话 | `POST /conversations/create` | 创建对话 |
| | `POST /conversations/chat` | 发消息，返回 `task_id` |
| | `POST /conversations/list` / `detail` / `delete` | 列表 / 详情 / 删除 |
| | `POST /conversations/mcp/*` · `/skills/*` | 对话级 MCP / skill 绑定 |
| 任务 | `POST /tasks/detail` / `stop` / `retry` / `turns` | 任务详情 / 停止 / 重试 / 轮次 |
| | `GET /tasks/stream` | SSE 流式输出 |
| 模型 | `POST /models/list` / `create` / `update` / `delete` / `set-default` / `test` / `status` | 全局模型配置 |
| MCP | `POST /mcp/servers/*` | MCP 服务器增删改查、连接测试、内置清单导入 |
| Skill | `POST /skills/*` | skill 增删改查、内置清单导入、zip 导入 |
| 文件 | `POST /files/upload` · `GET /files/download` / `open` | 工作区文件上传 / 下载 / 本地打开 |
| 设置 | `POST /settings/db/*` · `POST /search-config/*` | 数据库连接、检索引擎配置 |

完整的数据结构定义见 [`api/v1/workflow_agent.proto`](api/v1/workflow_agent.proto)。

## Quick Start

### 依赖

- Go 1.25+
- Node.js 18+（仅方式一的前端需要）
- MySQL 或 SQLite（开箱建议 SQLite 零依赖）

### 后端

```bash
cp config/config.example.yaml config/config.yaml
# 编辑 config/config.yaml，配置 database 与 llm（provider/base_url/api_key/model）

go build -trimpath -o bin/agent-server ./cmd/server
go build -trimpath -o bin/agent-scheduler ./cmd/scheduler
# 或一键 ./build.sh

./bin/agent-server -config config/config.yaml
./bin/agent-scheduler -config config/config.yaml
```

> 数据库驱动可在前端「系统设置」里热切换（写入 `config/db_config.json`）。用 SQLite 时无需任何外部依赖。

### 前端

```bash
cd frontend
npm install
npm run dev        # 开发模式
npm run build      # 生产构建到 dist/
```

## 目录结构

```
flowagent/
├── cmd/
│   ├── server/        # HTTP API 服务入口
│   └── scheduler/     # Stage 调度器入口（独立进程）
├── handler/           # HTTP 路由与处理器
├── dao/               # 数据访问层
├── model/             # 数据模型
├── schedule/          # 调度核心：DAG 规划、子任务执行、上下文压缩
├── pkg/
│   ├── llm/           # LLM 客户端（gemini / openai / azure / claude）
│   ├── mcp/           # MCP 工具协议
│   ├── skill/         # skill 系统
│   ├── stream/        # SSE 与跨进程事件转发
│   └── workspace/     # 工作区与文件工具（沙箱约束）
├── mcp/builtins/      # 内置 MCP 清单
├── skills/            # 内置 skill
├── api/v1/            # API 定义（proto）
├── frontend/          # React 前端
├── config/            # 配置示例（example）
└── docs/              # 文档
```

## License

[Apache License 2.0](LICENSE)。

第三方组件许可详见 [NOTICE](NOTICE)。其中 `skills/pdfkit-py` 依赖 PyMuPDF（AGPL-3.0），该目录不受本仓库 Apache-2.0 覆盖，启用该 skill 需自行遵守 AGPL 条款。

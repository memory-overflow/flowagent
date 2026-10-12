# flowagent 技术架构与实现理念

> 本文面向想要深入理解 flowagent 内部实现、或准备在其上二次开发/贡献代码的读者。
> 它从「为什么这么设计」出发，自顶向下把整个 agent 引擎拆成模块讲清楚。
> 如果只想把 flowagent 当 agent 内核接入使用，请看 [核心 API 参考](api.md)。

---

## 1. 设计目标

flowagent 是一个**对话驱动的全局工作流编排框架**：把一段自然语言需求规划成一张有向无环图（DAG），并行调度子任务执行，上游节点的输出自动注入下游节点。

它想解决两个问题：

1. **好接入、功能完善的开源 agent 框架缺失**。要么自己从零造轮子，要么绑定某家云厂商的托管 agent。flowagent 想成为那个「开箱即用、可插拔」的框架。
2. **ReAct 串行太慢，DAG 并行加速长程任务**。传统 ReAct 框架规划出步骤后只能一步步串行执行，长任务耗时高。flowagent 把任务规划成 DAG，让无依赖的子任务并行执行。

由此派生出一系列贯穿始终的设计原则，后文各模块都能看到它们的身影：

- **无状态、以数据库为中心**：任务的全部状态落在 DB，进程内不持有易失状态。任何时刻崩溃/重启/多副本接管都能无损恢复。
- **状态机动态流转**：任务在调度阶段（stage）之间流转的方向不是硬编码的，而是由执行器写入「决策 hint」，状态机实时算出下一步。
- **协议先行**：前后端交互、规划器与执行器之间都用显式的结构化协议（`submit_plan` 等）约束，而非靠 prompt 约定。
- **安全与可观测优先**：工具执行有沙箱与危险命令拦截，每个任务保留完整执行轨迹（turn 级 token、上下文分块、工具调用记录），并内置 pprof。

---

## 2. 总体架构

### 2.1 三个进程

flowagent 由三个独立的 Go 二进制构成，职责严格分离：

| 进程 | 入口 | 职责 |
|---|---|---|
| `agent-server` | `cmd/server` | HTTP API 服务：对话/任务/模型/MCP/Skill 的增删改查，文件上传下载，SSE 转发。IO 密集。 |
| `agent-scheduler` | `cmd/scheduler` | Stage 调度器：扫描 `task` 表，驱动各阶段执行（LLM 调用、工具调用、DAG 编排、上下文压缩）。CPU/LLM 密集。 |
| `agent-supervisor` | `cmd/supervisor` | 进程守护：托管上面两个子进程、异常退出自动拉起，并提供独立的 `:9090` 逃生端点（`POST /restart` / `GET /health`）。 |

**为什么把 server 与 scheduler 拆开**：

- 二者可独立扩缩容（scheduler 是 LLM/CPU 密集，server 是 IO 密集）；
- 单方发布不影响另一方；
- 调度器崩溃不会拖垮 API。

**server 与 scheduler 之间如何通信**：它们不直接 RPC，而是通过两样东西解耦——

1. **共享数据库**：server 写任务（`task`/`task_temp` 表），scheduler 扫描并推进；这是唯一的「任务队列」。
2. **HTTP 内部回调**：scheduler 侧执行器通过 `stream.HTTPForwarder` 把事件异步 POST 到 server 的 `/internal/events`，再由 server 进程内的 SSE 订阅者转发给前端（见 [§6 事件与流式](#6-事件与流式)）。

`supervisor` 存在的意义：server 卡死时 `:8080` 上任何接口都不可达，所以「逃生重启」必须挂在独立端口（默认 `127.0.0.1:9090`）的一个极简进程上。

### 2.2 一次请求的生命周期

```
用户发送消息
   │
   ▼
server:  POST /conversations/chat  → 在 DB 创建一条 Task（stage=functional_call_wait, phase=planning）
   │
   ▼
scheduler: functional_call 调度器扫描到该 task
   │  规划期（phase=planning）：规划器人格 + submit_plan 协议工具
   │  └─ 可先调真实工具调研，再 submit_plan(mode=direct_reply / single_agent / workflow)
   │
   ├─ direct_reply → 直接落库回复，当轮收口
   ├─ single_agent → 切执行期，functional_call ↔ tool_call 循环产出答案
   └─ workflow     → 节点并入对话绑定的工作流任务，由 workflow 调度器展开成子任务并行执行
```

三种「任务类型」各有独立生命周期，这是理解 flowagent 的关键（详见 [§3 核心抽象](#3-核心抽象)）。

---

## 3. 核心抽象

### 3.1 三种任务类型（TaskType）

| 值 | 类型 | 生命周期 |
|---|---|---|
| 1 | `conversation` 对话任务 | 用户每发一条消息产生一条。走 `planning → functional_call ↔ tool_call` 的单 Agent 循环，产出 `AgentOutputData`（一段回复）。 |
| 2 | `workflow` 工作流任务 | 一个对话可以有多条。每条承载一张相对独立的 DAG（存于 `task_output`）。执行时把节点展开为一批子任务按 `depends_on` 推进。 |
| 3 | `workflow_subtask` 单 Agent 子任务 | 工作流节点展开出的执行单元（`parent_task_id` 指向工作流任务），或 `deep_search` 派生的搜索子任务。复用 functional_call 循环，但 prompt 携带工作流信息与上游输出。 |

> 对话级工作流语义：`conversation.workflow_task_id` 只指向「当前激活」的那一条；全集按 `chat_id + task_type=2` 查。每轮规划由模型决定「续接已有工作流（append）」还是「新开一条（new）」。

### 3.2 两个正交的阶段维度

这是最容易混淆、也最关键的一对概念：

- **TaskStage（调度执行阶段）**：任务在调度器里流转到哪一步，对应 `task.stage` 字段。状态机（§3.3）驱动它流转。
- **TaskPhase（语义阶段）**：任务处于「规划期」还是「执行期」，对应 `task.phase` 字段。

```
Stage:  functional_call_wait ⇄ functional_call_doing   （LLM 调用阶段）
        tool_call_wait      ⇄ tool_call_doing          （工具调用阶段）
        workflow_wait       ⇄ workflow_doing           （DAG 编排阶段，仅工作流任务）
        finished                                       （终态）

Phase:  planning（规划期） / executing（执行期）
```

**规划期与执行期共享 `functional_call` 这一个 stage**，由 `phase` 决定执行器拼装哪种 prompt。这样设计的原因是：规划本身也需要调工具（查资料、看现状）才能拆得准；若把「规划」做成一个不能调工具的独立 stage，就得为它复制一套 `functional_call ↔ tool_call` 循环。降级为 functional_call 内的一个 phase，两者天然共享同一套循环。

### 3.3 状态机（StageTransition）

`model.TaskStage.Transit(action, hint)` 是调度系统的状态机核心，`stage` 和对外 `status` 都动态计算、不写死：

| 当前阶段 | 动作 | 下一阶段 | 对外状态 |
|---|---|---|---|
| `xxx_wait` | `schedule` | `xxx_doing` | RUNNING |
| `functional_call_doing` | `success` | `finished` / `functional_call_wait` / `tool_call_wait` | 见 hint |
| `tool_call_doing` | `success` | `functional_call_wait`（loop） | RUNNING |
| 任意阶段 | `failed` | 保留当前阶段 | FAILED |
| `xxx_doing` | `stop` | `xxx_wait` | STOPPED |

`functional_call_doing` 成功后去哪，取决于执行器写入的 `StageTransitionHint`：

- `PlanDelegated` → 规划判定为 workflow / direct_reply，当轮收口 `finished`；
- `PlanSubmitted` → 提交了规划，切执行期继续 `functional_call_wait`；
- `NeedToolCall` 且未超轮次 → `tool_call_wait`；
- 否则 → `finished`。

`hint` 是纯内存态，但它**可以从 `task_turn` 表重建**（`StageTransitionStore.resolveHint`）。这正是无状态设计的关键：进程重启或多副本接管时，丢失的内存决策能从持久化的执行记录里恢复，状态机不会走默认分支把任务误判。

---

## 4. 代码目录结构

```
flowagent/
├── cmd/
│   ├── server/        # HTTP API 服务入口
│   ├── scheduler/     # Stage 调度器入口（独立进程）
│   └── supervisor/    # 进程守护 + 逃生端点
├── handler/           # HTTP 路由与处理器（router.go 集中注册路由）
├── dao/               # 数据访问层（各表的 DAO + DB 初始化/热切换）
├── model/             # 数据模型（Task/Stage 状态机、协议结构体、枚举）
├── schedule/          # 调度核心
│   ├── stage_scheduler.go     # 各 stage 调度器工厂
│   ├── container/             # StageTaskContainer（动态状态流转容器）
│   └── actuator/              # 各阶段执行器（规划/执行/工具调用/工作流编排/上下文工程）
├── pkg/
│   ├── llm/           # LLM 客户端抽象（gemini/openai/azure/claude 子包）
│   ├── llmreg/        # 模型配置 → llm.Client 的解析器（含 TTL 缓存）
│   ├── contextmgr/    # 上下文水位度量与压缩决策（token 上限表）
│   ├── mcp/           # MCP 工具协议（连接/ListTools/CallTool + Registry）
│   ├── skill/         # Skill 系统（prompt 型 + command 型 + 安全拦截）
│   ├── stream/        # 进程内事件 Hub + 跨进程 HTTP 转发
│   └── workspace/     # 工作区管理 + 内置 native 工具（沙箱约束）
├── mcp/builtins/      # 内置 MCP 清单（fetch/playwright/filesystem/...）
├── skills/            # 内置 skill（docx/pptx/shell/pdfkit-py/minimax-xlsx）
├── api/v1/            # API 定义（proto）
├── frontend/          # React 前端（对话/工作流 DAG/调试/模型/MCP/Skill 页）
├── config/            # 配置示例（config.example.yaml）
└── docs/              # 文档
```

下面按「自顶向下的调用关系」分模块展开。

---

## 5. 分模块详解

### 5.1 调度核心（`schedule/`）

调度核心基于开源任务调度框架 [light-task-scheduler](https://github.com/memory-overflow/light-task-scheduler)，但做了关键改造：**动态状态流转**。

框架的抽象是「容器（TaskContainer）+ 执行器（TaskActuator）+ 调度器（TaskScheduler）」三元组。flowagent 的映射：

- **`StageTaskContainer`**（`schedule/container`）：实现 `TaskContainer` 接口。每个调度器实例只绑定一个负责扫描的 `wait` 阶段（`scanWaitStage`），`doing` 阶段由 `wait.ToDoingStage()` 推导，**下一阶段由状态机动态计算**，不在构造时硬编码。
- **`XXXActuator`**（`schedule/actuator`）：实现 `TaskActuator`，执行具体业务逻辑。
- **`StageScheduler`**（`schedule/stage_scheduler.go`）：工厂，为每个 stage 创建一个 `light-task-scheduler` 实例。

**`StageTransitionStore`**（`schedule/container`）是执行器与容器之间的决策传递通道：执行器执行完写入 `hint`，容器在 `ToSuccessStatus` 时读取 `hint` 调状态机算出下一阶段。它是纯内存的，但丢失后能从 `task_turn` 表重建（§3.3）。

每个调度器实例的规格（在 `NewStageScheduler` 里声明）：

| 调度器 | 扫描阶段 | 任务类型 | 超时 | 并发 | 重试 |
|---|---|---|---|---|---|
| `functional_call` | functional_call_wait | conversation | 15min | 10 | 3 |
| `functional_call_subtask` | functional_call_wait | workflow_subtask | 15min | 10 | 3 |
| `tool_call` | tool_call_wait | 不限 | 15min | 10 | 0 |
| `workflow` | workflow_wait | workflow | 2h | 10 | 0 |

几个值得注意的设计细节：

- **按 task_type 拆分 functional_call**：对话任务与子任务共享同一个 stage，但子任务常被用来「一次性生成完整大文件」（整份 HTML 动画等），单次 completion 可能远超对话任务，所以拆成两个调度器实例赋予不同超时。
- **超时是「单次进入 doing 阶段」的看门狗**，不是任务生命周期：任务每流转一次 `functional_call ↔ tool_call` 都会重设 `stage_start_time`，所以 15min 约束的是「单次 LLM/工具调用」的耗时上限。
- **乐观锁流转**：`updateStage` 用 `WHERE stage = fromStage` 的前置条件做 CAS，避免两个调度器对同一任务并发写 stage 导致「拉回上一阶段」。
- **启动自愈**：`recoverStaleRunningTasks` 把上次进程遗留的 doing+running 任务回退到 wait+queued，立即重新调度，而不是干等看门狗判超时失败；`ReconcileOrphanTasks` 把「祖先已终态、自己还在 running」的孤儿任务收口成 stopped。

### 5.2 functional_call 执行器（Agent 循环核心）

`schedule/actuator/functional_call_actuator.go` 是 agent loop 的核心：调 LLM，让它决定「继续调工具」还是「收口」。

单次执行流程（规划期与执行期共用骨架）：

1. 从 `task_turn` 重建对话历史（含此前所有 LLM 输出与工具结果）；
2. 加载对话绑定的 MCP / Skill 工具清单；
3. 调 LLM；
4. 记录 turn，按结果写状态机 hint：
   - 规划期提交了规划 → `PlanSubmitted=true` → 回到 functional_call（已切执行期）；
   - LLM 返回 tool_calls → `NeedToolCall=true` → 流转到 tool_call；
   - LLM 返回纯文本 → `NeedToolCall=false` → 流转到 finished。

执行期（`runExecutingLoop`）内建了四道「熔断/收口」防线，防止 agent 空转或死循环：

| 防线 | 触发条件 | 行为 |
|---|---|---|
| 轮次预算耗尽 | `round >= max_round`（默认 100） | 调用 LLM 前**先摘掉全部工具**，逼模型直接写总结 |
| 工具调用总次数预算 | 跨轮累计调用数超预算（仅 deep_search 子任务设） | 强制收口 |
| 连续失败熔断 | 同一「工具+参数」指纹连续失败 3 次 | 强制收口 |
| 死循环检测 | 连续 3 轮请求完全相同的工具调用 | 强制收口 |
| 连续无效熔断 | 检索类工具连续 4 次返回为空/过短 | 强制收口 |

**为什么「轮次耗尽」要在调用前摘工具而非调完再收口**：模型在工具可用的上下文里通常只回 tool_call、不带正文，直接收口会把「我还要去核实 xx」这句过程描述当成最终答案落库——任务看似成功、结论却是废的。摘掉工具后模型没有工具可调，只能输出真正的总结。

**幂等跳过**（`functionalCallIdempotentSkip`）：turn 的写入与任务的状态流转不在同一事务里，若进程在「LLM turn 已落库、但状态流转」之间崩溃，重启后重进执行器会把已调过的 LLM 再调一遍。这里利用 `task_turn` 作为持久化执行记录，识别「上一次已执行完」并跳过。

### 5.3 规划期（`planning_phase.go`）

规划期与执行期共享 functional_call 阶段，差异只在 prompt 与收口方式：

- prompt：规划器人格 + 对话已累积 workflow + 可用工具能力清单；
- 工具：真实工具 + `submit_plan` 协议工具；
- 收口：模型调 `submit_plan` → 执行器拦截 → 落库 plan → 按 mode 分流。

`submit_plan` 之后按 mode 分三条路（`finishPlanning`）：

| mode | 含义 | 对话任务后续 |
|---|---|---|
| `direct_reply` | 无需执行即可作答（闲聊、常识问答等） | `reply` 直接落库为 `agent_output.reply_text`，当轮完成 |
| `single_agent` | 需要动手，但单 Agent 循环即可 | 切执行期继续循环 |
| `workflow` | 需拆解为多步骤 DAG | 节点并入工作流任务，对话任务回复流程概述后收口 |

规划期允许调真实工具做调研（查目录、搜资料、读文件），这是「拆得准」的前提。为防规划期把全部预算耗在调研上，它有独立的轮次上限（`planningMaxToolCallRound = 4`），达到后仍未 submit_plan 则强制降级切执行期；输出节点校验不过则带反馈重规划（最多 `planningMaxReplanRounds` 次）。

规划期还沉淀出 `WorkflowPlanContext`：规划阶段查到的结论与产物，收口时注入 DAG 的每个节点，避免节点从零重搜（历史教训：一次选型调研里规划期已产出 58KB 结论，工作流却又跑了 1011 次 web_search）。

### 5.4 工作流编排（`workflow_actuator.go` + `workflow_binding.go`）

`WorkflowActuator` 负责把工作流任务（DAG 存 `task_output`）展开为一批单 Agent 子任务，按 `depends_on` 推进。

**关键设计：无状态**。它不持有内存 `flowContext`，而是在每次 `GetAsyncTaskStatus` 轮询时从 DB 重建 DAG 与子任务终态。原因：DAG 本身已持久化、子任务各有独立行；无状态天然支持多副本与重启恢复；对话级 DAG 规模小，每 1s 重建开销可忽略。

推进逻辑：无依赖（或依赖已全部完成）的节点先创建子任务（`Stage=functional_call_wait, Phase=executing`），子任务完成后继续启动下游节点，直到全部完成或任一失败。全部完成后可选地对绑定节点做一次 LLM 结果汇总（`summarizeWorkflowResults`），写回 `workflow_summary`。

`workflow_binding.go` 处理对话级工作流的绑定与增量扩展：

- 规划判定 `new` → 新建一条工作流任务并设为激活；
- 规划判定 `append` → 在指定工作流上追加节点，按当前状态调整（执行中不改，其他回到等待执行）；
- 失败工作流续接时，锚点只限「已完成节点」，失败节点及其全部后代被剪枝归档（`pruneAbandonedBranch`，标 `TaskStatusAbandoned`）；
- 用户主动停止的工作流不可续接。

**`TaskStatusAbandoned`（=7）是节点级专属终态**：该节点所在分支已被后续规划剪掉，永不执行、也不参与工作流成败与进度统计。它与 `Archived`（归档）正交：前者表达「被剪枝」，后者表达「已被划为历史」。

### 5.5 deep_search（`deep_search.go`）

`deep_search` 是「联网搜索」的框架级内置工具。它**不自己抓网页**，而是把检索目标交给一个专门的搜索子任务：

```
主对话任务（tool_call 阶段）
  └─ 调 deep_search(query, max_rounds)
      └─ 创建 TaskTypeWorkflowSubTask（ParentTaskID = 主对话任务）
          └─ 子任务走 functional_call ↔ tool_call 循环，用 fetch 抓网页
          └─ 收口后，把 task_output 里的汇总结论读回来
      └─ 同步轮询子任务终态，把结论作为工具结果返回
```

为什么用子任务而不是在主循环里直接 fetch：

- 搜索会反复抓取超长网页文本，直接放主上下文会迅速撑爆；
- 子任务的 turn 天然落库，整个「模型 + fetch」循环每一步都能在调试页查看；
- 子任务检索轮次受限（默认 4 轮），最后一轮摘掉全部工具逼模型总结。

### 5.6 上下文工程（`context_compact.go` / `context_recall.go` / `agent_history.go` / `pkg/contextmgr`）

#### 历史重建（`agent_history.go`）

agent loop 每次进入 functional_call 都要拿到「截至目前的完整对话历史」。历史**不放在内存里，而是每次从 `task_turn` 表重建**（多副本/重启后任务仍要能继续推进）。范围是「整个对话」而非单个 task——用户连续追问时模型必须看到之前几轮。

重建规则很精细（按时间升序遍历该 chat 的所有 turn）：

- 进入新 task 时插入该 task 的 user 输入；
- functional_call turn 只有成功才入历史（失败调用没产出）；
- 规划期的调用额外过滤 `submit_plan` 协议工具（执行期没这个工具，回放会 400）；
- tool_call turn 成功失败都入（失败必须回喂，否则模型重复请求同一工具）。

同一 task 执行期间，「本 task 之前的历史」（before 段）恒定不变，所以做了进程内缓存，只有 current 段随工具调用增长。

#### 上下文压缩（`context_compact.go`）

模型输入被拆成五块，按「可否压缩」分两类：

| 块 | 可否压缩 |
|---|---|
| 1. system_prompt | 不可压 |
| 2. tools（工具 JSON Schema，每轮全量重发） | 不可压 |
| 3. 工作流摘要（本身已是压缩结果） | 不可压 |
| 4. 已压缩历史摘要（context_summary 表，滚动更新） | 可压 |
| 5. 未压缩历史详情 | 可压 |

压缩流程：拼装后用 tiktoken 计总量 → 超过上限 80%（`TriggerRatio`）进入压缩循环 → 每轮把「块4+块5 的前 80%」交给 LLM 重写成新摘要（≤上限 10%）→ 重算水位 → 直到降到 30%（`TargetRatio`）以下。

三条硬约束（错一条就会出线上问题）：

- **task 原子性**：一个 task 的所有 turn 要么全压、要么全不压，半压会切断 assistant tool_calls 与 tool 结果的配对，provider 直接判 400；
- **当前 task 近况永不压缩**：它还在推进中，最近几轮是模型下一步决策的直接依据；
- **必须能退出**：不可压部分本身可能就超过 30%，此时降级到「压到 80% 以下即通过」。

水位线定义在 `pkg/contextmgr`，它只回答「有多少 token / 上限是多少 / 该不该压」，不负责「怎么压」（那需要 DAO 与 LLM 客户端，属于 actuator）。`ResolveContextLimit` 内置了主流模型的输入上限表（混元/Gemini/OpenAI/Claude/GLM，按「最大输入」口径逐个核对官方文档），漏配时按模型名前缀查表兜底。

**延后重排（defer）**：一个对话允许用户连发多个问题（每个问题一个 task，并行推进）。压缩要吃掉「更早的历史 task」，而那些 task 若还在跑，压进去的内容是不完整的。于是压缩触发时必须等前置任务收尾——但原地阻塞会占并发槽、还有看门狗超时风险，所以改成「延后重排 + 指数退避」（`delay_until` + `defer_count`），把等待交给调度器。

#### 历史取回（`context_recall.go`）

压缩是**有损且不可逆**的。`recall_context` 内置工具是它的逆动作：压缩产物旁挂了句柄（历史摘要带 `task_id`、当前 task 摘要带被压掉的 `turn_id`、工作流摘要每个节点带 `task_id`），模型拿句柄换回原文。

两种模式：`turn_id` 模式直接查 `task_turn` 返回原文（零 LLM）；`task_id` 模式加载整段交互，放得下就原样返回，放不下才按 `query` 做 map-reduce 提取。

### 5.7 工具体系（`pkg/mcp` + `pkg/skill` + `pkg/workspace`）

工具来源有三类，统一抽象成 `ToolEntry`（在 `schedule/actuator/tool_provider.go` 聚合）：

| 来源 | 命名空间 | 执行方式 | 默认可用 |
|---|---|---|---|
| 内置 native 工具 | 无前缀（`write_file` 等） | 当前进程内 Go 原生执行 | 是 |
| MCP 工具 | `<server>__<tool>` | MCP 协议（握手 + 子进程） | 按对话绑定 |
| Skill 工具 | `skill__<name>` | 本地 shell 子进程 | 按对话绑定 |

**MCP（`pkg/mcp`）**：封装 MCP 客户端，支持 `stdio` / `sse` / `streamable_http` 三种传输。连接是「用完即走」的短连接模式（无状态、无需处理重连）。`Registry` 按对话缓存工具清单（默认 30min TTL），并解决跨 server 工具重名问题（对外名字加 `<server>__` 前缀，`<server>__<tool>` 反查回真实 server）。

**Skill（`pkg/skill`）**：本地能力包。prompt 型直接注入 system prompt，command 型用本地 shell 子进程执行（stdin 传输入、stdout 取结果）。执行前有危险删除命令拦截（`DetectDangerousCommand`），并自动激活项目级 venv。

**内置 native 工具（`pkg/workspace`）**：补上第三类能力——Go 原生实现的文件/命令/时间工具，默认对全部对话可用：

```
write_file / read_file / list_dir / create_dir / file_exists /
run_command / get_current_time / deep_search / recall_context
（analyze_image 为条件注入：仅多模态模型支持时注入）
```

这些工具被路径沙箱强制约束在「工作区根目录/<任务ID>/」之下（`Manager.Resolve` 拒绝绝对路径、`..` 越界、Windows 盘符），`run_command` 复用 skill 的危险命令拦截，并自己起进程组 + select 等超时 + kill 整组（避免 `exec.CommandContext` 只杀直接子进程、孙进程残留的问题）。

**工具聚合（`tool_provider.go`）**：functional_call 与 tool_call 共用同一份工具加载逻辑，保证「暴露给 LLM 的名字」与「反查时用的名字」始终一致。`ToolSet` 自带 lookup 索引，不依赖 Registry 内部缓存状态（tool_call 可能在另一个调度器副本上执行）。

### 5.8 LLM 抽象（`pkg/llm` + `pkg/llmreg`）

`pkg/llm` 提供统一的 LLM 调用中间层：上层只依赖 `llm.Client` 接口，与具体 provider 解耦。

```go
type Client interface {
    Chat(ctx, messages, opts) (string, error)
    ChatWithTools(ctx, messages, tools, choice, opts) (*ChatResult, error)
    Name() string
}
```

请求/响应统一使用 `Message` + `Options` 结构，规避各家协议差异。四个 provider 子包实现：`openai` / `azureopenai`（gpt 协议）/ `gemini` / `claude`。流式能力独立成 `StreamClient` 接口（可选能力），provider 支持则走流式低延迟路径，不支持则退回阻塞式。

`pkg/llmreg` 提供「模型配置 → llm.Client」的解析器 `Resolver`：

- 按对话的 `model_id` 动态选模型，空/缺省回退默认模型，再回退 `config.LLM`；
- 带 **TTL 缓存**（10s）：server 与 scheduler 是独立进程，模型 CRUD 发生在 server 而 Resolver 跑在 scheduler，跨进程没有失效通知，永久缓存会让配置改动不生效。TTL 保证改动在窗口内自动传播，无需重启。

### 5.9 事件与流式（`pkg/stream`）

`stream.Hub` 是进程内事件广播中心（SSE 的数据源）。执行器/容器在任务推进的关键节点发布事件，handler 层的 SSE 连接按 `taskID` 订阅转发给前端。

事件类型：`snapshot`（连接建立时下发全量 turn 快照）、`turn_done`、`task_done`、`tool_calling`、`compacting`（压缩中提示，压缩会阻塞本轮 LLM 调用数秒到数十秒）、`delta`（流式文本，预留）。

它是纯内存的**尽力而为**通道：不做持久化、不保证投递，断连/重启/缓冲溢出都会丢事件，最终一致性由前端轮询兜底。事件负载刻意保持轻量（只带定位信息），完整数据由前端收到事件后回拉。

**跨进程转发**：server 与 scheduler 分离，scheduler 侧的事件通过 `HTTPForwarder` 异步回调到 server 的 `/internal/events`，再送达 server 进程里的 SSE 订阅者。回调地址与 token 由 `stream.callback_url` / `stream.callback_token` 配置。

### 5.10 数据层（`model/` + `dao/`）

**双表设计**：`task`（终态任务表）与 `task_temp`（临时态任务表，嵌入 Task、表名不同）。调度器高频扫描 `task_temp`（`stage=? AND status=?`），任务到达终态后 `task_temp` 行被物理删除（`purgeTaskTemp`），只留 `task` 表供查询。复合索引 `stage_status_created` / `chat_status_created` 保证 1s 轮询不退化成全表扫描。

核心表：`conversation`、`task`/`task_temp`、`task_turn`（执行轨迹/turn 快照）、`mcp_server`、`conversation_mcp`、`skill`、`conversation_skill`、`model_config`、`search_config`、`context_summary`、`task_context_summary`。

`dao` 层支持 SQLite（零依赖，开箱即用）与 MySQL，并支持**热切换**：系统设置里改库后不重启直接换连接池（server 侧 `dao.Reload` + migrate；scheduler 侧 2s 轮询设置文件跟随）。

### 5.11 HTTP 层（`handler/`）

`router.go` 集中注册全部路由，统一走 `Wrap` 包装（JSON 绑定 + 统一响应包装）。文件上传/下载、SSE、zip 导入等因走 multipart / 文件流 / EventSource 而绕过 `Wrap`。

一个值得注意的点：`/internal/events` 是 scheduler → server 的内部回调端点（不对外），用 `stream.callback_token` 鉴权（为空则不校验，仅限内网/本地开发）。

### 5.12 前端（`frontend/`）

React 18 + Vite。页面：`ChatPage`（对话 + DAG 可视化）、`WorkflowPage`（工作流详情）、`DebugPage`（turn 级调试/上下文分块/token 树）、`ModelsPage`、`MCPServersPage`、`SkillsPage`、`SettingsPage`、`SearchConfigPage`。

关键组件：`WorkflowGraph`（DAG 渲染）、`WorkflowSwitcher`（多工作流切换）、`ContextUsage`（上下文水位指示器）、`ToolCalls`（工具调用记录折叠）、`Markdown`（回复渲染）。

### 5.13 可观测性

flowagent 把「可调试」当成一等公民：

- **turn 级执行轨迹**：每次 LLM 调用与工具调用各落一条 `task_turn`，含输入/输出快照、token（输入/输出拆分）、耗时、失败原因、来源模型、所属 stage/phase；
- **上下文分块计量**：每次 LLM 调用按五块模型计量输入构成（`context_blocks`），调试页据此渲染「各模块占比」，对话页据此显示水位；
- **token 树统计**：以「任务树」为单位聚合（自身 + 全部后代子任务），一眼看出 deep_search 子任务烧了多少 token；
- **pprof**：server 与 scheduler 各自内置 `/debug/pprof/`（block / mutex profiling），默认只绑 loopback。

---

## 6. 关键设计决策速览

| 决策 | 选择 | 理由 |
|---|---|---|
| 状态存放 | 数据库为中心，无内存态 | 多副本、崩溃恢复天然成立 |
| 阶段流转 | 状态机 + hint 动态决策 | 不硬编码路径，执行器决定去哪 |
| 规划 | 降级为 functional_call 的 phase | 规划也需调工具，复用同一套循环 |
| 工作流编排 | 无状态、每轮 DB 重建 | 无需维护 flowCache 的一致性 |
| 历史重建 | 每次从 task_turn 重建 | 跨进程/重启后任务仍可继续 |
| 压缩粒度 | task 原子性 + 延后重排 | 避免半压切坏配对、避免压到未收尾任务 |
| 工具沙箱 | 路径约束 + 危险命令拦截 | 防止越界写系统目录、误删关键文件 |
| 熔断 | 四道防线（轮次/预算/连续失败/死循环/无效） | 防 agent 空转与检索发散 |
| 模型选择 | Resolver + TTL 缓存 | 跨进程配置改动无需重启 |

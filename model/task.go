package model

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

// ============================================================
// TaskStatus — 任务状态枚举
// ============================================================

type TaskStatus int

const (
	TaskStatusQueued        TaskStatus = 1 // 排队中
	TaskStatusRunning       TaskStatus = 2 // 进行中
	TaskStatusCompleted     TaskStatus = 3 // 已完成
	TaskStatusFailed        TaskStatus = 4 // 任务失败
	TaskStatusStopped       TaskStatus = 5 // 已终止
	TaskStatusPartialFailed TaskStatus = 6 // 部分失败
	// TaskStatusAbandoned 已废弃。仅用于工作流 DAG 节点，不是任务级状态。
	//
	// 语义：该节点所在分支已被后续规划剪掉，永不执行、也不参与工作流成败
	// 与进度统计。典型来源：工作流里某个节点失败后，其全部下游节点依赖
	// 永远无法满足（停在 queued），在被续接规划时必须显式标废弃而不是
	// 继续等 —— 否则工作流重新排队后会永远跑不完（见 workflow_binding.go
	// 的 pruneAbandonedBranch）。
	TaskStatusAbandoned TaskStatus = 7
)

// IsNodeTerminal 判断工作流节点状态是否为终态（不再被调度、不再参与成败判定）。
//
// 与任务级 IsTerminal 分开：节点多了 Abandoned 这一「被剪枝」的终态，
// 而任务级没有这个概念。两处语义不同，不要合并成一个函数。
func IsNodeTerminal(s TaskStatus) bool {
	return s == TaskStatusCompleted ||
		s == TaskStatusFailed ||
		s == TaskStatusStopped ||
		s == TaskStatusPartialFailed ||
		s == TaskStatusAbandoned
}

// TaskType — 任务类型枚举
//
// 三种任务的生命周期各不相同，不能混在一条调度链上：
//
//	对话任务：用户每发一条消息产生一条，走 planning → functional_call ↔ tool_call
//	          的单 Agent 循环，产出 AgentOutputData（一段回复）。
//	工作流任务：一个对话可以有多条（见 Conversation.WorkflowTaskID —— 它指向
//	          当前激活的那一条）。每条承载一个相对独立的 DAG，后续轮次规划时
//	          由模型决定「在某个已有工作流上追加」还是「新开一条」。
//	          执行时把节点展开为一批单 Agent 子任务，按 depends_on 驱动推进。
//	单 Agent 子任务：工作流节点展开出来的执行单元，parent_task_id 指向工作流任务，
//	          复用 functional_call ↔ tool_call 循环，但 prompt 携带工作流信息
//	          与父节点输出，产出 AgentOutputData。
type TaskType int

const (
	// TaskTypeConversation 对话任务。取值 1 与历史的「单 agent 任务」一致，
	// 存量数据无需迁移。
	TaskTypeConversation TaskType = 1
	// TaskTypeWorkflow 工作流任务。取值 2 与历史的「工作流 agent 任务」一致。
	TaskTypeWorkflow TaskType = 2
	// TaskTypeWorkflowSubTask 单 Agent 子任务（工作流节点展开的执行单元）。
	// 取值 3，避开已用的 1/2。
	TaskTypeWorkflowSubTask TaskType = 3
)

// String 返回任务类型名称
func (t TaskType) String() string {
	switch t {
	case TaskTypeConversation:
		return "conversation"
	case TaskTypeWorkflow:
		return "workflow"
	case TaskTypeWorkflowSubTask:
		return "workflow_subtask"
	default:
		return "unknown"
	}
}

// ============================================================
// TaskPhase — 任务语义阶段（规划期 / 执行期）
// ============================================================

// TaskPhase 任务所处的语义阶段。
//
// 与 TaskStage（调度执行阶段）解耦：规划期和执行期共享 functional_call
// 这一个调度阶段，由 Phase 决定 functional_call 执行器拼装哪种 prompt。
//
// 引入动机：任务规划本身也需要调用工具（查资料/搜索辅助决策），
// 因此不能再把「规划」做成一个不能调工具的独立 stage，
// 而是把它降级为 functional_call 阶段内的一个语义状态。
type TaskPhase int

const (
	PhasePlanning  TaskPhase = 1 // 规划期：模型可调用工具辅助规划，最终 submit_plan
	PhaseExecuting TaskPhase = 2 // 执行期：正常 agent 循环，产出最终答案
)

// String 返回语义阶段名称
func (p TaskPhase) String() string {
	switch p {
	case PhasePlanning:
		return "planning"
	case PhaseExecuting:
		return "executing"
	default:
		return "unknown"
	}
}

// SubmitPlanToolName 规划期模型提交规划所调用的特殊工具名。
//
// 它不是真实的 MCP/Skill 工具，而是规划期由执行器注入的一个「协议工具」：
// 模型在调研充分后调用它提交规划，执行器据此持久化 plan 并把任务从
// 规划期切到执行期。执行器、状态机恢复、历史重建三处都要识别它，
// 故放在 model 包共享。
const SubmitPlanToolName = "submit_plan"

// DeepSearchToolName deep_search 内置工具名。
//
// deep_search 是「联网搜索」的框架级内置工具（见 pkg/workspace 的 deepSearchTool
// 与 schedule/actuator 的 deep_search runner）：它不是简单的单次 fetch，而是把
// 一次检索目标交给一个专门的搜索子任务（内部跑「模型 + fetch」多轮循环），
// 最后由子任务汇总成一份结论返回给主对话。放在 model 包共享，供 workspace
// （工具定义）与 actuator（runner 执行）两处引用同一名字。
const DeepSearchToolName = "deep_search"

// RecallContextToolName recall_context 内置工具名。
//
// 上下文压缩的逆动作：压缩把历史摘要化（有损、不可逆），本工具让模型拿
// 摘要旁挂的句柄（task_id / turn_id）把原文取回。见 pkg/workspace 的
// recallContextTool 与 schedule/actuator 的 context_recall runner。
//
// 放在 model 包共享，供 workspace（工具定义）与 actuator（runner 执行、
// 压缩索引文案里引用工具名）三处引用同一名字 —— 索引文案必须与真实工具名
// 一致，否则模型会照文案调用一个不存在的工具。
const RecallContextToolName = "recall_context"

// DefaultDeepSearchMaxRound deep_search 子任务默认的最大检索轮次。
//
// 与对话任务的 DefaultMaxToolCallRound（100）不同：搜索子任务要主动收口，
// 避免「反复抓取 → 每轮返回超长文本」既拖慢又吃满上下文。默认 4 轮
// （原为 8 轮，实测 8 轮下的增量信息很少，却要多花一倍的时间与 token），
// 配合搜索 prompt 的软约束，在「查证充分」与「及时收口」之间取平衡。
const DefaultDeepSearchMaxRound = 4

// DeepSearchMaxRoundLimit deep_search 子任务允许的最大检索轮次上限。
// 防止模型通过 max_rounds 参数把预算设得过大，绕开「及时收口」的约束。
const DeepSearchMaxRoundLimit = 20

// SubTaskKindDeepSearch deep_search 子任务的类型标记（写入 TaskInputData.SubTaskKind）。
//
// deep_search 子任务与 workflow DAG 节点子任务同为 TaskTypeWorkflowSubTask，
// 但二者的工具集策略不同：deep_search 子任务内部只应使用原生 fetch 抓取，
// 若再暴露 deep_search 工具会递归嵌套搜索子任务。执行器据此标记在加载工具集时
// 隐藏 deep_search，从根上切断递归。
const SubTaskKindDeepSearch = "deep_search"

// ============================================================
// PlanMode — 规划判定出的处理模式
// ============================================================

// PlanMode 规划期 submit_plan 给出的处理模式。
//
// 定义在 model 包而非 actuator：状态机恢复（container.llmTurnIntent）
// 也要按模式判断流转方向 —— workflow 模式下对话任务当轮即收口，
// 不能像 single_agent 那样回到 functional_call 继续跑。
type PlanMode = string

const (
	// PlanModeSingleAgent 单 Agent 模式：当前对话任务继续 agent 循环产出答案
	PlanModeSingleAgent PlanMode = "single_agent"
	// PlanModeWorkflow 工作流模式：节点并入某条工作流任务，
	// 当前对话任务只回复执行流程概述后立即收口
	PlanModeWorkflow PlanMode = "workflow"
	// PlanModeDirectReply 直接回复模式：规划器判定本轮无需 Agent 动手执行
	// （闲聊、常识问答、基于已有上下文或规划期调研结果即可作答），
	// 直接把 submit_plan.reply 作为最终回复落库并收口，不再进执行期。
	//
	// 省掉的是「执行期再调一次 LLM 重新组织答案」：规划期那次调用已经看过
	// 完整上下文，让它顺手给出答案，比切执行期多一轮往返（含 system prompt /
	// 工具 schema 的重复计费）更快更省。
	PlanModeDirectReply PlanMode = "direct_reply"
)

// PlanModeFinishesTask 该规划模式是否让当前对话任务在规划期当轮收口。
//
// workflow（活已移交给工作流）与 direct_reply（答案已给出）都不再进执行期；
// 只有 single_agent 需要切执行期继续跑 agent 循环。
// 执行器（planning_phase）与状态恢复（container.llmTurnIntent）共用这一判据，
// 保证进程重启后流转方向与首次执行一致。
func PlanModeFinishesTask(mode PlanMode) bool {
	return mode == PlanModeWorkflow || mode == PlanModeDirectReply
}

// PlanWorkflowAction 规划判定为 workflow 后，本轮节点落在哪条工作流上。
//
// 一个对话可以有多条工作流，每轮规划都要显式回答这个问题：
// 是续接某条已有工作流（复用它已完成节点的产出），还是新开一条。
type PlanWorkflowAction = string

const (
	// PlanActionAppend 在 workflow_id 指定的已有工作流上追加节点
	PlanActionAppend PlanWorkflowAction = "append"
	// PlanActionNew 规划一条全新的工作流
	PlanActionNew PlanWorkflowAction = "new"
)

// ============================================================
// 协议 pb 对应的 Go 结构体（用于 JSON 序列化到 task_input/task_output）
// ============================================================

// FileInfo 文件信息
type FileInfo struct {
	Source   string `json:"source"`
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"`
	URL      string `json:"url,omitempty"`
	Size     int64  `json:"size,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileType int    `json:"file_type"` // 1-image, 2-video, 3-text, 4-other
}

// TaskInput 任务输入
type TaskInputData struct {
	InputText  string     `json:"input_text,omitempty"`
	InputFiles []FileInfo `json:"input_files,omitempty"`
	// SubTaskKind 子任务类型标记（仅子任务使用）。用于区分同一 TaskType 下
	// 不同用途的子任务 —— 例如 deep_search 搜索子任务与 workflow DAG 节点子任务
	// 同为 TaskTypeWorkflowSubTask，但前者需要隐藏 deep_search 工具避免递归嵌套。
	// 空串表示普通子任务（无特殊工具集策略）。
	SubTaskKind string `json:"sub_task_kind,omitempty"`
	// ParentTurnID 仅子任务使用。记录派发该子任务的父任务 turn id（task_turn.id），
	// 让调试页能把子任务入口直接挂在对应的 tool_call 行上，而不是堆在顶部列表里。
	// 老数据没有此字段，反查不到时前端不显示入口（不影响主流程）。
	//
	// 该字段保留为「首个派发 turn」，新增的复用 turn 见 ParentTurnIDs。
	// 读取时统一走 ParentTurnIDList()，不要直接用本字段。
	ParentTurnID int64 `json:"parent_turn_id,omitempty"`
	// ParentTurnIDs 复用同一子任务的全部父 turn id（task_turn.id）。
	//
	// 存在场景：LLM 一批 tool_call 里对同一 query 调了多次 deep_search，并发命中
	// 同一个 pending 子任务后复用；进程重启重放也会重入同一个子任务。这些 turn
	// 都该挂上调试入口，单值的 ParentTurnID 只能记一个，故升级为数组。
	// 为空表示尚无复用（此时退化为 ParentTurnID 单值，见 ParentTurnIDList）。
	ParentTurnIDs []int64 `json:"parent_turn_ids,omitempty"`
	// DeepSearchQuery 仅 deep_search 子任务使用：记录检索目标原文。
	//
	// 用途：并发/重放时判断能否复用已有 pending 子任务 —— 只有 query 完全一致
	// 才是同一件事的重入，query 不同必须各自建子任务，否则后一次会拿到前一次的
	// 搜索结论（数据张冠李戴）。空串表示非搜索子任务或改动前的老数据。
	DeepSearchQuery string `json:"deep_search_query,omitempty"`
	// ArtifactDir 本任务的产出目录（相对工作区，如 "outputs/wf-ac049a2f/n1_search"）。
	// 仅子任务设置；顶层对话任务为空，产出直接落在工作区。
	//
	// 存在的理由：工作区是**对话级**的，所有并行节点与它们的 deep_search
	// 子任务共享同一份文件。产物收集原先只能按 mtime 扫描（mtime > 任务
	// start_time），并行节点的执行窗口互相重叠，于是每个节点收口时都会把
	// 同一时刻其它节点（及其子任务）写的文件一并算成自己的产物 —— 实测一个
	// 选型调研里，三个并行节点的产物清单都是同样的 24 个文件。
	//
	// 有了独立目录，产物归属变成「按目录」这件确定的事，不再依赖时间窗口。
	// 空串时行为与改动前一致（落到 outputs/ 并按 mtime 扫描）。
	ArtifactDir string `json:"artifact_dir,omitempty"`
	// ForceNewWorkflow 仅对话任务使用：用户显式要求「新开一个工作流」。
	//
	// v1.4 起一个对话可以有多条工作流，本轮规划默认由模型决定续接哪条。
	// 用户在前端打开这个开关时，表示他认为本轮是个新话题 —— 用户判断优先于
	// 模型判断，规划期直接跳过 append 分支（见 resolveWorkflowTarget）。
	ForceNewWorkflow bool `json:"force_new_workflow,omitempty"`
}

// ParentTurnIDList 返回本子任务关联的全部父 turn id（去重、顺序稳定）。
//
// 兼容老数据：改动前只写单值 ParentTurnID，这里把它补到列表首位，
// 保证「首个派发 turn」在数组化前后都是同一个，调试入口位置不跳。
func (d *TaskInputData) ParentTurnIDList() []int64 {
	if d == nil {
		return nil
	}
	out := make([]int64, 0, len(d.ParentTurnIDs)+1)
	seen := make(map[int64]bool, len(d.ParentTurnIDs)+1)
	if d.ParentTurnID != 0 {
		out = append(out, d.ParentTurnID)
		seen[d.ParentTurnID] = true
	}
	for _, id := range d.ParentTurnIDs {
		if id != 0 && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	return out
}

// AppendParentTurnID 追加一个复用本子任务的父 turn id，返回是否发生变更。
//
// id 为 0 或已存在时返回 false（幂等），调用方据此跳过无意义的写库。
func (d *TaskInputData) AppendParentTurnID(id int64) bool {
	if d == nil || id == 0 {
		return false
	}
	for _, existing := range d.ParentTurnIDList() {
		if existing == id {
			return false
		}
	}
	d.ParentTurnIDs = append(d.ParentTurnIDs, id)
	return true
}

// ToolCallRecord MCP 工具调用记录
type ToolCallRecord struct {
	ToolName   string `json:"tool_name"`
	ServerName string `json:"server_name"`
	Input      string `json:"input"`
	Output     string `json:"output"`
	Status     int    `json:"status"`
	TimeCost   string `json:"time_cost"`
	// Summary 模型在工具调用 schema 的 summary 参数里写的一句话说明。
	// 前端 ToolCalls 块默认展示 summary，让用户清楚「这次调用在做什么」。
	// 为空时前端降级为 server_name/tool_name 拼接。
	Summary string `json:"summary,omitempty"`
}

// MaxToolCallOutputChars tool_call 记录里 Output 字段的最大字符数（按 rune）。
//
// read_file / run_command 等工具的输出可能很大（读大文件全文、命令完整 stdout），
// 全量复制进 task_output.tool_calls 会让工作流的 task_output 膨胀到 MB 级，
// 拖慢详情接口（DB 读 + JSON 解析）。落库时就截断，从源头控制 task_output 体积。
// 完整输出仍在 task_turn 表，调试可回查。
const MaxToolCallOutputChars = 4 * 1024

// TruncateToolCallOutput 截断 tool_call 的输出，超过上限时加标记。
func TruncateToolCallOutput(s string) string {
	if n := len([]rune(s)); n > MaxToolCallOutputChars {
		r := []rune(s)
		return string(r[:MaxToolCallOutputChars]) + "\n…(输出过大，已截断)…"
	}
	return s
}

// AgentOutput 单 agent 任务输出
type AgentOutputData struct {
	ReplyText    string           `json:"reply_text"`
	OutputFiles  []FileInfo       `json:"output_files,omitempty"`
	ToolCalls    []ToolCallRecord `json:"tool_calls,omitempty"`
	ThinkProcess []string         `json:"think_process,omitempty"`
	// ThinkLayers 分层思考过程（规划期 / 执行期），前端据此分组折叠展示。
	// 每条为结构化 ThinkStep：Summary 默认展示、Detail 点击展开。
	// ThinkProcess 与 ThinkLayers 二者总是同时写入 —— 前者是平铺的完整文本
	// （兼容旧前端与降级展示），后者是带 summary/detail 的结构化条目。
	ThinkLayers *ThinkLayers `json:"think_layers,omitempty"`
	// WorkflowTaskID 本轮回复规划出的目标工作流任务 ID（仅对话任务在
	// 判定 workflow 模式收口时携带）。前端据此在回复消息上渲染
	// 「查看工作流」跳转按钮 —— 一个对话可有多条工作流，回复文本本身
	// 不含可靠的 ID，靠结构化字段跳转才不会指错条目。
	WorkflowTaskID string `json:"workflow_task_id,omitempty"`
	// WorkflowSummary 该轮规划出的工作流节点全部执行结束后，由后端调用
	// LLM 对这批节点的结果做的一次汇总（成功→最终结论与产物；失败→失败
	// 位置与原因）。工作流执行中为空，收口后由 workflow_actuator 写回，
	// 前端把它追加在规划回复末尾展示。
	WorkflowSummary string `json:"workflow_summary,omitempty"`
}

// WorkflowNode DAG 工作流节点
type WorkflowNodeData struct {
	NodeID      string           `json:"node_id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Input       TaskInputData    `json:"input,omitempty"`
	DependsOn   []string         `json:"depends_on,omitempty"`
	TaskID      string           `json:"task_id,omitempty"`
	Status      TaskStatus       `json:"status"`
	Output      *AgentOutputData `json:"output,omitempty"`
	// Context 节点级上下文（自由文本，规划模型声明）：这个节点特有的、工作流
	// 全局 known_context 没覆盖到的背景。续接节点用它承载「本轮用户新增的诉求」——
	// 工作流全局的「用户原始输入」是创建轮的原文，续接轮新输入只有这里能进。
	// 注入节点 prompt 时放在【本步骤背景】段。
	Context string `json:"context,omitempty"`
	// SourceTaskID 提出该节点的对话任务 ID。
	// 前端据此把 DAG 节点关联回「哪一轮对话规划出来的」。
	SourceTaskID string `json:"source_task_id,omitempty"`
	// CreateTs 节点加入工作流的时间（毫秒）
	CreateTs int64 `json:"create_ts,omitempty"`
	// Archived 该节点是否已被归档：不再参与调度与成败/进度判定，但仍在 DAG 里
	// 保留供追溯（前端灰显 + 虚线边框）。
	//
	// 与 Status 正交：Status 表达「执行结果」（完成/失败/废弃），Archived 表达
	// 「已被后续规划划为历史、本轮及以后的执行都不再管它」。
	// 不加这个标记的话，工作流因历史失败节点被续接后会立刻再次判定为失败 ——
	// 执行器每轮扫全部节点统计 anyFailed，历史 failed 节点会一直命中。
	Archived bool `json:"archived,omitempty"`
	// ArchiveReason 归档原因（前端 tooltip 展示），如「上游节点失败，分支已废弃」。
	ArchiveReason string `json:"archive_reason,omitempty"`
}

// WorkflowOutput 工作流 agent 任务输出
type WorkflowOutputData struct {
	Nodes        []WorkflowNodeData `json:"nodes"`
	PlanSummary  string             `json:"plan_summary"`
	ToolCalls    []ToolCallRecord   `json:"tool_calls,omitempty"`
	ThinkProcess []string           `json:"think_process,omitempty"`
	// OutputFiles 整段工作流的产物清单：各子任务 output_files 的并集（按 path 去重）。
	//
	// 单独聚合一份而不让前端遍历 Nodes 自行收集：
	//   - 工作流是「一次交付」的语义单元，用户要的是最终产物清单，
	//     而不是"第 3 个节点产出了什么"这种执行细节
	//   - 节点可能被重试/重置（见 resetWorkflowNodesForRetry），
	//     node.Output 会被清空，但已落盘的文件仍在工作区里
	OutputFiles []FileInfo `json:"output_files,omitempty"`
	// Version DAG 版本号（乐观锁 + 前端展示）。
	// 与 task.dag_version 列同步，每次成功写入 DAG 时 +1。
	// 读侧一律以 task.dag_version 列为权威，本字段仅是写入时刻的快照。
	Version int64 `json:"version"`
	// Title 工作流短标题（8~16 字），由规划期模型生成。
	//
	// 一个对话可以有多个工作流，切换器要按标题区分它们。放在 DAG 里的同时
	// 也会冗余写一份到 task.title —— 列表页要按 chat_id 拉全部工作流的
	// 标题/状态/进度，逐条解析 longtext 的 task_output 太重。
	Title string `json:"title,omitempty"`
	// PlanContext 规划期调研上下文：规划阶段已经查到、工作流节点可直接
	// 复用的结论与产物。
	//
	// 存在的理由：过去规划期 deep_search 查到的东西只留在规划任务的 turn
	// 与子任务里，而节点 input_text 只有「总体目标 + 步骤说明」，于是节点
	// 从零重搜 —— 实测一次 Agent 平台选型调研里，规划期已产出约 58KB 结论，
	// 工作流却又跑了 1011 次 web_search，其中一个节点还在重复检索里被死循环
	// 检测掐断。现在规划收口时把结论抽成这份结构写进 DAG，每个节点注入。
	PlanContext *WorkflowPlanContext `json:"plan_context,omitempty"`
}

// ============================================================
// 规划任务 ↔ 工作流节点 状态绑定
// ============================================================

// DeriveBoundTaskStatus 由「某轮对话任务规划出的工作流节点」的状态，
// 推导该对话任务当前应对外展示的状态。
//
// 背景：对话任务在规划判定 workflow 后当轮收口（stage=finished、status=completed），
// 但用户视角里「这条回复」的活并没有干完 —— 真正的执行在它规划出的节点上。
// 把对话任务的状态绑定到它提出的节点（WorkflowNodeData.SourceTaskID），
// 避免出现「工作流还在跑，回复却显示已完成」的割裂。
//
// 绑定规则（产品口径，顺序即优先级）：
//  1. 绑定节点全部完成                          → 已完成
//  2. 绑定节点中任一失败（含废弃，见下）        → 失败
//  3. 节点未全部完成，但工作流整体已判失败      → 失败
//     （覆盖「本轮节点都在排队/执行中，却因别的分支失败被整体收口」的情况，
//     否则这些节点永远等不到依赖，回复状态卡在执行中）
//  4. 其余                                      → 执行中
//
// 规则未覆盖的边界按如下口径归并：
//   - 废弃（Abandoned）节点视为失败：它因上游失败被剪枝，永远不会再执行，
//     按「执行中/已完成」展示都是谎言；归为失败并配合工作流跳转链接，
//     用户能直接看到废弃原因。
//   - 停止（Stopped）节点或工作流被用户停止 → 已终止：这是用户主动动作，
//     报「失败」会误导排查方向。
//   - 已归档（Archived）节点不豁免：归档只改变调度与统计口径，节点的
//     执行结果（完成/失败）是既成事实，参与绑定判定。
//
// 返回值第二个是对应进度（0~100，按绑定节点完成比例），供任务列表展示。
// nodes 为空（该轮没有产出节点 / 旧数据无 source_task_id）时返回 completed：
// 调用方不应为这种轮次改状态，这里给 completed 只是兜底语义。
func DeriveBoundTaskStatus(nodes []WorkflowNodeData, wfStatus TaskStatus) (TaskStatus, float64) {
	if len(nodes) == 0 {
		return TaskStatusCompleted, 100
	}

	total := len(nodes)
	completed := 0
	anyFailed := false
	anyStopped := false
	for i := range nodes {
		switch nodes[i].Status {
		case TaskStatusCompleted:
			completed++
		case TaskStatusFailed, TaskStatusPartialFailed, TaskStatusAbandoned:
			anyFailed = true
		case TaskStatusStopped:
			anyStopped = true
		default:
			// queued / running：仍在推进
		}
	}

	switch {
	case anyFailed:
		// 规则 2：任一绑定节点失败（或被剪枝废弃），本轮规划即视为失败
		return TaskStatusFailed, float64(completed) / float64(total) * 100
	case completed == total:
		// 规则 1：全部完成
		return TaskStatusCompleted, 100
	case wfStatus == TaskStatusFailed || wfStatus == TaskStatusPartialFailed:
		// 规则 3：节点没跑完，但工作流已整体失败 —— 本轮不可能再有产出
		return TaskStatusFailed, float64(completed) / float64(total) * 100
	case anyStopped || wfStatus == TaskStatusStopped:
		// 用户主动停止：跟随工作流口径，报终止而非失败
		return TaskStatusStopped, float64(completed) / float64(total) * 100
	default:
		// 规则 4：仍在执行（含工作流排队中）
		return TaskStatusRunning, float64(completed) / float64(total) * 100
	}
}

// WorkflowPlanContext 规划期沉淀下来、供工作流节点复用的已知信息。
//
// Context 是主字段：规划模型在 submit_plan 里自由声明的、下游节点需要继承的
// 全部已知信息（检索结论、文件路径、关键决策、历史约束、边界风险等），后端
// 原样注入每个节点 —— 不再按信息类型拆字段（那会导致每出现一类信息就补一个字段）。
//
// 旧字段 Summary / KeyFacts 仅做历史数据 / 旧模型兼容读取；Files 是后端
// 自动收集的规划期落盘产物（非模型声明），仍单独渲染。
//
// 规划期没调研（模型直接给出 DAG）且没有落盘产物时，本结构整体为 nil，
// 节点照常从零开始，行为与改造前一致。
type WorkflowPlanContext struct {
	// Context 下游节点需要继承的全部已知信息（自由文本，模型声明）。
	Context string `json:"context,omitempty"`
	// 以下为旧字段，仅做历史数据 / 旧模型兼容读取，新数据一律写 Context：
	Summary  string   `json:"summary,omitempty"`
	KeyFacts []string `json:"key_facts,omitempty"`
	// Files 规划期落盘的产物文件（相对工作区路径），节点可用 read_file 读全文。
	// 只列文件不给内容：结论已经在 Context 里说清，完整素材按需读，避免每个
	// 节点都被同一份长文占满上下文。
	Files []FileInfo `json:"files,omitempty"`
	// Source 上下文来源，仅用于排查与调试页展示：
	//   - "model" 规划模型在 submit_plan 里自己总结的（首选，最贴合任务）
	//   - "auto"  模型没给，后端从规划期 turn / 子任务记录里自动抽取的
	Source string `json:"source,omitempty"`
}

// 规划期上下文的来源取值（WorkflowPlanContext.Source）。
const (
	// PlanContextSourceModel 规划模型在 submit_plan 里自己总结的（首选）。
	PlanContextSourceModel = "model"
	// PlanContextSourceAuto 模型未总结，后端从规划期记录自动抽取的（兜底）。
	PlanContextSourceAuto = "auto"
)

// IsEmpty 判断规划期上下文是否为空（没有任何可注入的内容）。
func (c *WorkflowPlanContext) IsEmpty() bool {
	return c == nil || (strings.TrimSpace(c.Context) == "" &&
		strings.TrimSpace(c.Summary) == "" &&
		len(c.KeyFacts) == 0 && len(c.Files) == 0)
}

// ============================================================
// 工作流可追加性判定
// ============================================================

// WorkflowAppendPolicy 一个工作流「能否被继续追加节点」的判定结果。
//
// 判定规则（对应产品需求）：
//   - 用户主动停止的工作流：不可追加。用户停止说明规划本身有问题，
//     没有后续利用价值，只能新开工作流。
//   - 成功 / 执行中的工作流：可追加，锚点是所有未被剪枝的节点。
//   - 失败 / 部分失败的工作流：可追加，但锚点只限「已完成节点」
//     （= 失败节点的上游），失败节点及其全部后代已被剪枝，不可依赖。
type WorkflowAppendPolicy struct {
	// Appendable 是否允许在该工作流上追加节点
	Appendable bool `json:"appendable"`
	// Reason 不可追加的原因（前端展示 / 规划期注入 prompt 让模型理解）
	Reason string `json:"reason,omitempty"`
	// AnchorIDs 允许的依赖锚点：新节点的 depends_on 只能取自这里
	AnchorIDs map[string]bool `json:"-"`
	// ReusableCount 可复用（已完成且未归档）的节点数，供规划期展示复用价值
	ReusableCount int `json:"reusable_count"`
}

// EvaluateAppendPolicy 按工作流任务当前状态 + DAG 内容判定可追加性。
//
// wfStatus 是工作流任务（task.status）的状态，不是节点状态。
// 判定与「剪枝」分离：本函数只读，不改 DAG；真正的剪枝落库在
// workflow_binding.pruneAbandonedBranch 里做。
func (w *WorkflowOutputData) EvaluateAppendPolicy(wfStatus TaskStatus) WorkflowAppendPolicy {
	// 用户主动停止：无后续利用价值，直接不可追加
	if wfStatus == TaskStatusStopped {
		return WorkflowAppendPolicy{
			Appendable: false,
			Reason:     "该工作流已被用户主动停止，说明规划存在问题，不可继续追加节点",
		}
	}

	dead := w.DeadNodeSet()
	failedWorkflow := wfStatus == TaskStatusFailed || wfStatus == TaskStatusPartialFailed

	anchors := make(map[string]bool, len(w.Nodes))
	reusable := 0
	for _, n := range w.Nodes {
		if dead[n.NodeID] {
			continue
		}
		if failedWorkflow {
			// 失败工作流只允许挂在真正跑成功的节点上 —— 只有它们的输出
			// 才是可用的上下文数据。排队/执行中的节点在失败收口时已被停止，
			// 既没有产出也不能作为依赖。
			if n.Status != TaskStatusCompleted {
				continue
			}
		} else if n.Status == TaskStatusFailed ||
			n.Status == TaskStatusPartialFailed ||
			n.Status == TaskStatusStopped ||
			n.Status == TaskStatusAbandoned {
			continue
		}
		anchors[n.NodeID] = true
		if n.Status == TaskStatusCompleted {
			reusable++
		}
	}

	// 有历史节点但一个都复用不了：续接没有意义，引导模型新开工作流
	if len(w.Nodes) > 0 && len(anchors) == 0 {
		return WorkflowAppendPolicy{
			Appendable: false,
			Reason:     "该工作流没有可复用的已完成节点，无法在其基础上继续规划",
		}
	}
	return WorkflowAppendPolicy{
		Appendable:    true,
		AnchorIDs:     anchors,
		ReusableCount: reusable,
	}
}

// DeadNodeSet 求 DAG 里「已死亡」的节点集合：失败节点 + 其全部后代 + 已归档节点。
//
// 后代通过 depends_on 反向边 BFS 得到。失败节点的下游依赖永远无法满足，
// 它们停在 queued 状态，既不能执行也不能被新节点依赖。
func (w *WorkflowOutputData) DeadNodeSet() map[string]bool {
	children := make(map[string][]string, len(w.Nodes))
	dead := make(map[string]bool, len(w.Nodes))
	queue := make([]string, 0, len(w.Nodes))

	for _, n := range w.Nodes {
		if n.Archived {
			dead[n.NodeID] = true
			queue = append(queue, n.NodeID)
			continue
		}
		if n.Status == TaskStatusFailed || n.Status == TaskStatusPartialFailed ||
			n.Status == TaskStatusStopped {
			dead[n.NodeID] = true
			queue = append(queue, n.NodeID)
		}
		for _, dep := range n.DependsOn {
			children[dep] = append(children[dep], n.NodeID)
		}
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range children[cur] {
			if dead[child] {
				continue
			}
			dead[child] = true
			queue = append(queue, child)
		}
	}
	return dead
}

// ActiveNodeCount 参与调度与进度统计的节点数（排除已归档节点）。
func (w *WorkflowOutputData) ActiveNodeCount() int {
	n := 0
	for i := range w.Nodes {
		if !w.Nodes[i].Archived {
			n++
		}
	}
	return n
}

// NodeIDs 返回全部节点 id，供规划期做唯一性校验
func (w *WorkflowOutputData) NodeIDs() map[string]bool {
	if w == nil {
		return map[string]bool{}
	}
	ids := make(map[string]bool, len(w.Nodes))
	for _, n := range w.Nodes {
		ids[n.NodeID] = true
	}
	return ids
}

// NextWorkflowStatus 计算追加节点后工作流任务应处于的状态。
//
// 规则（对应「根据当前工作流状态修改工作流的状态」）：
//   - 执行中（running）：新节点直接并入正在跑的 DAG，状态不变
//   - 其他（已完成 / 失败 / 终止 / 排队）：有新活要干，回到排队等待执行
//
// 返回 changed=false 表示无需写库。
func NextWorkflowStatus(current TaskStatus) (next TaskStatus, changed bool) {
	if current == TaskStatusRunning {
		return current, false
	}
	return TaskStatusQueued, true
}

// ============================================================
// Task — 终态任务表
// ============================================================

type Task struct {
	// 任务 ID
	TaskID string `gorm:"column:task_id;size:100;primaryKey"`
	// 父任务 ID（工作流子任务时有值）
	ParentTaskID string `gorm:"column:parent_task_id;size:100"`
	// Title 任务标题。目前只有工作流任务使用 —— 一个对话可以有多个工作流，
	// 切换器要按标题区分它们（对话任务本身就是一条消息，不需要额外标题）。
	// 冗余自 WorkflowOutputData.Title，供列表页轻量查询，避免解析 longtext。
	Title string `gorm:"column:title;size:512;not null;default:''" json:"title,omitempty"`
	// 所属对话 ID
	//
	// 复合索引用「未命名 + composite」写法（而不是写死索引名）：索引名由
	// gorm 按 表名+composite 生成（idx_task_chat_status_created /
	// idx_task_temp_chat_status_created）。写死名字在 MySQL 下没问题
	// （索引名按表隔离），但 sqlite 的索引名是**库级唯一**的 —— Task 与
	// TaskTemp（嵌入 Task）会抢同一个名字，第二张表建索引直接报
	// "index already exists"，sqlite 模式下启动即失败。
	ChatID string `gorm:"column:chat_id;size:100;not null;index:,composite:chat_status_created,priority:1"`
	// 任务类型
	TaskType TaskType `gorm:"column:task_type;not null;default:0"`
	// 任务输入 (JSON)
	TaskInput JsonField[*TaskInputData] `gorm:"column:task_input;type:text"`
	// 任务输出 (JSON 字符串) — 根据 TaskType 可能是 AgentOutputData 或 WorkflowOutputData
	TaskOutput string `gorm:"column:task_output;type:longtext"`
	// 规划结果 (JSON 字符串) — planning 阶段由 LLM 生成的执行计划快照
	Plan string `gorm:"column:plan;type:longtext"`
	// 任务状态
	Status TaskStatus `gorm:"column:status;not null;default:1;index:,composite:chat_status_created,priority:2;index:,composite:stage_status_created,priority:2"`
	// 任务阶段（functional_call_wait/functional_call_doing/tool_call_wait/...）
	// stage_status_created 复合索引供调度器高频扫描 stage=? AND status=? 使用。
	// 该索引通过 TaskTemp 嵌入同步作用于 task_temp 表，避免每 1s 的全表扫描。
	Stage int `gorm:"column:stage;not null;default:0;index:,composite:stage_status_created,priority:1"`
	// 任务语义阶段（1=规划期/2=执行期）。与 Stage 解耦：
	// 规划期与执行期共享 functional_call 调度阶段，由 Phase 决定拼装哪种 prompt。
	Phase int `gorm:"column:phase;not null;default:1"`
	// 任务进度 0.0~100.0
	Progress float64 `gorm:"column:progress;not null;default:0"`
	// 已执行的原子调用总轮次。
	// 每完成一次 LLM 调用或工具调用（= 一个 turn）就 +1，与 task_turn 表记录数一致。
	TurnCount int32 `gorm:"column:turn_count;not null;default:0"`
	// DAG 版本号（乐观锁）。仅工作流任务使用：
	// 每次成功回写 task_output 的 DAG 时 +1，并发「追加节点」与「执行回写」
	// 通过「WHERE dag_version = ?」校验版本，冲突方放弃或重试，避免丢失更新。
	DAGVersion int64 `gorm:"column:dag_version;not null;default:0"`
	// 失败原因 (JSON)
	FailedReason JsonField[*FailedReason] `gorm:"column:failed_reason;type:text"`
	// 开始执行时间（任务首次进入执行时记录，之后保持不变）。
	// 语义是「任务真正开始的时刻」，供产物扫描（mtime > start_time）等需要
	// 任务生命周期的起点时使用 —— 不能把它当「单次进入 doing 阶段」的超时起点。
	StartTime *time.Time `gorm:"column:start_time"`
	// StageStartTime 当前阶段进入 doing 的时间（看门狗「单次进入 doing 阶段」超时计时用）。
	// 与 StartTime 解耦：StartTime 恒定，StageStartTime 每次流转进入 RUNNING 都会重设，
	// 因此看门狗据此判断「本次 LLM/工具调用是否超时」，而不会误杀长任务。
	StageStartTime *time.Time `gorm:"column:stage_start_time"`
	// 结束时间
	EndTime *time.Time `gorm:"column:end_time"`
	// 尝试次数
	AttemptTimes int32 `gorm:"column:attempt_times;not null;default:0"`
	// DelayUntil 延后调度的时间点：调度器扫描待调度任务时会跳过「delay_until
	// 晚于当前时间」的行（见 container.GetWaitingTask），到期后才重新进入队列。
	//
	// 为什么需要它：一个对话允许用户连发多个问题（每个问题一个 task，并行推进）。
	// 上下文压缩要吃掉「更早的历史 task」，而那些 task 若还在跑，压进去的内容
	// 是不完整的 —— 一旦把它们标记为已压缩（covered），它们后续产生的 turn 会被
	// 历史回放过滤掉，用户就再也看不到前一个问题后半段的结论。因此压缩触发时
	// 必须等前置任务收尾；但原地阻塞会占着并发槽、还有被看门狗判超时的风险，
	// 于是改成「延后重排 + 指数退避」，把等待交给调度器。
	DelayUntil *time.Time `gorm:"column:delay_until"`
	// DeferCount 已延后重排的次数（指数退避的指数）。
	//
	// 落库而非放内存：延后跨越多次调度周期，且调度器可能重启/多副本，
	// 内存计数会在切换进程时归零，退避退回到最小值，导致任务被高频重复唤醒
	// （每轮都要重建历史 + 计量 token，代价不低）。
	DeferCount int `gorm:"column:defer_count;not null;default:0"`
	// 创建时间
	CreatedAt time.Time `gorm:"column:created_time;not null;index:,composite:chat_status_created,priority:3;index:,composite:stage_status_created,priority:3"`
	// 最后更新时间
	UpdatedAt time.Time `gorm:"column:updated_time;not null;autoUpdateTime"`
	// 删除时间
	DeletedAt gorm.DeletedAt `gorm:"column:delete_time;index"`
}

func (Task) TableName() string { return "task" }

// FailedReason 失败原因
type FailedReason struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (t *Task) IsTerminal() bool {
	return t.Status == TaskStatusCompleted ||
		t.Status == TaskStatusFailed ||
		t.Status == TaskStatusStopped ||
		t.Status == TaskStatusPartialFailed
}

// ============================================================
// TaskTemp — 临时态任务表（嵌入 Task，表名不同）
// ============================================================

type TaskTemp struct {
	Task
}

func (TaskTemp) TableName() string { return "task_temp" }

// ============================================================
// TaskStage — 任务执行阶段枚举（调度系统用）
// ============================================================

// TaskStage 任务执行阶段。每个中间阶段包含 wait / doing 两个子状态。
// 数据库存储为 int，映射到 task.stage 字段。
type TaskStage int

const (
	StageNone                TaskStage = 0 // 未设置（初始态）
	StageFunctionalCallWait  TaskStage = 3 // LLM 调用阶段-等待
	StageFunctionalCallDoing TaskStage = 4 // LLM 调用阶段-执行中
	StageToolCallWait        TaskStage = 5 // 工具调用阶段-等待
	StageToolCallDoing       TaskStage = 6 // 工具调用阶段-执行中
	// StageWorkflowWait / StageWorkflowDoing 工作流任务的 DAG 编排阶段。
	//
	// 由独立的 workflow 调度器扫描（见 schedule/actuator/workflow_actuator.go），
	// 负责把 DAG 节点展开为一批单 Agent 子任务并按 depends_on 驱动推进。
	// 工作流任务本身不占用 functional_call 调度器 —— 那会让它被当成
	// 对话任务跑一遍单 Agent 循环。
	StageWorkflowWait  TaskStage = 7 // 工作流编排阶段-等待
	StageWorkflowDoing TaskStage = 8 // 工作流编排阶段-执行中
	StageFinished      TaskStage = 9 // 完成
)

// String 返回阶段名称
func (s TaskStage) String() string {
	switch s {
	case StageNone:
		return "none"
	case StageFunctionalCallWait:
		return "functional_call_wait"
	case StageFunctionalCallDoing:
		return "functional_call_doing"
	case StageToolCallWait:
		return "tool_call_wait"
	case StageToolCallDoing:
		return "tool_call_doing"
	case StageWorkflowWait:
		return "workflow_wait"
	case StageWorkflowDoing:
		return "workflow_doing"
	case StageFinished:
		return "finished"
	default:
		return "unknown"
	}
}

// IsWaitStage 判断是否为等待阶段
func (s TaskStage) IsWaitStage() bool {
	switch s {
	case StageFunctionalCallWait, StageToolCallWait, StageWorkflowWait:
		return true
	default:
		return false
	}
}

// IsDoingStage 判断是否为执行中阶段
func (s TaskStage) IsDoingStage() bool {
	switch s {
	case StageFunctionalCallDoing, StageToolCallDoing, StageWorkflowDoing:
		return true
	default:
		return false
	}
}

// ToDoingStage 将 wait 阶段转为对应的 doing 阶段
func (s TaskStage) ToDoingStage() TaskStage {
	switch s {
	case StageFunctionalCallWait:
		return StageFunctionalCallDoing
	case StageToolCallWait:
		return StageToolCallDoing
	case StageWorkflowWait:
		return StageWorkflowDoing
	default:
		return s
	}
}

// ToWaitStage 将 doing 阶段转为对应的 wait 阶段
func (s TaskStage) ToWaitStage() TaskStage {
	switch s {
	case StageFunctionalCallDoing:
		return StageFunctionalCallWait
	case StageToolCallDoing:
		return StageToolCallWait
	case StageWorkflowDoing:
		return StageWorkflowWait
	default:
		return s
	}
}

// IsTerminalStage 判断是否为终态阶段
func (s TaskStage) IsTerminalStage() bool {
	return s == StageFinished
}

// ============================================================
// StageTransition — 状态流转决策（状态机）
// ============================================================

// TransitionAction 触发流转的动作类型。
// 容器根据不同动作，配合当前 stage 计算出 (nextStage, nextStatus) 二元组。
type TransitionAction int

const (
	// ActionSchedule 调度动作：wait → doing
	ActionSchedule TransitionAction = iota
	// ActionSuccess 当前阶段执行成功：doing → 下一个 wait 或终态
	ActionSuccess
	// ActionFailed 当前阶段执行失败：→ 失败终态
	ActionFailed
	// ActionStop 主动停止：doing → 回退到 wait
	ActionStop
)

// String 返回动作名称
func (a TransitionAction) String() string {
	switch a {
	case ActionSchedule:
		return "schedule"
	case ActionSuccess:
		return "success"
	case ActionFailed:
		return "failed"
	case ActionStop:
		return "stop"
	default:
		return "unknown"
	}
}

// StageTransitionHint 阶段流转的决策提示。
// 由执行器（actuator）在执行过程中填充，容器在流转时读取。
type StageTransitionHint struct {
	// Deferred 本轮「不推进、也不收口」，回到当前阶段的 wait 重新排队。
	//
	// 用于「上下文压缩要等前置任务收尾」这类场景：本轮什么都没做（没调 LLM、
	// 没写 turn），任务不该被判成功流转到下一阶段，更不该被判失败 ——
	// 它只是还没到执行时机。容器据此把它拨回本阶段 wait，配合 delay_until
	// 让扫描在退避期内看不见它，到期后重新调度、原样重跑本轮。
	//
	// 优先级最高：它描述的是「本轮没执行」，先于任何「执行后去哪」的判定。
	Deferred bool
	// NeedToolCall functional_call 阶段的决策：LLM 是否返回了 tool_call 指令
	NeedToolCall bool
	// PlanSubmitted functional_call 阶段（规划期）的决策：本轮提交了规划
	// （submit_plan），需切到执行期后继续 functional_call，而非进入 tool_call 或 finished。
	PlanSubmitted bool
	// PlanDelegated 规划期已给出本轮最终结果，当前对话任务当轮收口：
	//   - workflow 模式：任务已移交给对话绑定的工作流任务；
	//   - direct_reply 模式：规划器直接给出了答案（见 PlanModeFinishesTask）。
	//
	// workflow 时当前对话任务已写好「执行流程概述」回复，活由工作流任务接着干，
	// 因此必须直接收口 finished —— 不能再进执行期跑一遍单 Agent 循环，
	// 否则同一件事会被做两遍，用户还会看到两份互相矛盾的回复。
	//
	// 优先级高于 PlanSubmitted 与 NeedToolCall。
	PlanDelegated bool
	// ToolCallRound 当前已完成的 tool_call 轮次
	ToolCallRound int
	// MaxToolCallRound 允许的最大 tool_call 轮次（超出则强制结束）
	MaxToolCallRound int
	// ConsecutiveFailures 当前最大的「同一调用连续失败次数」（熔断用）。
	//
	// 记账粒度是「工具名 + 参数」的调用指纹：同一指纹每失败一次 +1，
	// 成功一次即清零；换工具或换参数是新的指纹、从 1 起算 —— 那是模型
	// 在换方法尝试，不该被打断。只有反复用**完全相同**的调用撞同一堵墙，
	// 才说明工具配置或环境有问题，达到上限后强制收口。
	// （FailStreaks 里的最大值，由记账处维护，消费端直接读。）
	ConsecutiveFailures int
	// FailStreaks 各调用指纹的连续失败次数（指纹 = 工具名+参数规范化后的组合）。
	FailStreaks map[string]int
	// LastToolCallFingerprint 上一轮 tool_call 的调用指纹
	// （工具名 + 参数规范化后的字符串）。用于死循环检测：
	// 连续多轮请求完全相同的工具调用，说明模型卡在「重复同一动作」上，
	// 不管结果如何都不会再推进。
	LastToolCallFingerprint string
	// RepeatedToolCallCount 连续重复相同工具调用指纹的轮次数（死循环检测）。
	// 本轮指纹与上一轮一致则 +1，不一致则重置为 1；达到阈值后
	// functional_call 阶段强制收口，避免「同一调用无限重复」。
	RepeatedToolCallCount int
	// MaxToolCallCount 允许的**工具调用总次数**上限（跨轮累计），<=0 表示不限制。
	//
	// 与 MaxToolCallRound 是两个正交的预算：后者数的是「轮」，而实测一轮里
	// 模型常并行发 4~6 个工具调用（一次 Agent 选型调研里 max_rounds=12 的子
	// 任务跑出了 49 次 web_search）。只限轮次等于没限总量，这是 deep_search
	// 发散的直接原因 —— 检索面随每轮并行调用指数扩张。
	//
	// 目前只有 deep_search 子任务设置它（见 runDeepSearch），普通对话任务
	// 不设，保持原有行为。
	MaxToolCallCount int
	// ToolCallCount 已完成的工具调用总次数（跨轮累计，内存态）。
	//
	// 仅作 hint 内的增量记录；真正的判定以 DB 统计为准（见
	// resolveToolCallBudget）—— 内存态在进程重启后会丢失，而预算失效的
	// 后果是发散，不能靠内存态兜底。
	ToolCallCount int
}

// DefaultMaxToolCallRound 默认最大工具调用轮次
const DefaultMaxToolCallRound = 100

// DefaultMaxConsecutiveFailures 默认连续失败熔断阈值（同一调用连续失败次数）。
const DefaultMaxConsecutiveFailures = 3

// DefaultMaxRepeatedToolCalls 默认连续重复相同工具调用阈值（轮次）。
//
// 死循环检测的第二道防线：放宽轮次上限到 100 后，光靠「连续失败熔断」
// 拦不住「工具调用每次都成功、但模型反复请求同一动作」的空转 ——
// 那种循环不会触发 ConsecutiveFailures，却会空耗 token 与时间。
// 因此对「工具名 + 参数」的指纹做连续重复计数，达到本阈值即强制收口。
const DefaultMaxRepeatedToolCalls = 3

// DefaultMaxLowValueToolCalls 默认「连续无效工具调用」熔断阈值（次）。
//
// 无效 = 检索类工具（搜索/抓取）返回为空或过短（低于 minUsefulToolResultChars）。
// 与 DefaultMaxConsecutiveFailures 的区别：后者数的是**报错**，这里数的是
// 「调用成功却什么都没拿到」。实测一次调研里 25.7% 的搜索返回 <100B，
// 模型看到空内容不会当失败、只会继续换词再搜，失败熔断与死循环检测都拦不住，
// 只有「连续 N 次都没拿到东西」这个信号能逼它换策略或收口。
const DefaultMaxLowValueToolCalls = 4

// StageTransition 一次状态流转的结果，包含 stage 和对外 status 两个维度。
type StageTransition struct {
	// Stage 流转后的内部执行阶段
	Stage TaskStage
	// Status 流转后的对外任务状态
	Status TaskStatus
	// IsTerminal 是否已到达终态（不再继续流转）
	IsTerminal bool
}

// Transit 根据当前阶段、触发动作和决策提示，动态计算流转后的 (stage, status)。
//
// 这是整个调度系统的状态机核心。stage 和status 都动态计算，不写死。
//
// 流转规则表：
//
//	当前阶段               动作       → 下一阶段                对外状态
//	──────────────────────────────────────────────────────────────────────────
//	xxx_wait              schedule   → xxx_doing               RUNNING
//	functional_call_doing success    → finished                COMPLETED (判定 workflow，已移交)
//	                                 → functional_call_wait    RUNNING   (提交了规划，切执行期)
//	                                 → tool_call_wait          RUNNING   (需工具且未超轮次)
//	                                 → finished                COMPLETED (无需工具或超轮次)
//	tool_call_doing       success    → functional_call_wait    RUNNING   (loop)
//	任意阶段               failed     → 保留当前阶段            FAILED
//	xxx_doing             stop       → xxx_wait                STOPPED
func (s TaskStage) Transit(action TransitionAction, hint *StageTransitionHint) StageTransition {
	switch action {
	// ────────────────────────────────────────────────
	// 失败：任何阶段失败都统一进入 FAILED 终态
	// stage 保留在失败发生的阶段，便于定位问题
	// ────────────────────────────────────────────────
	case ActionFailed:
		return StageTransition{
			Stage:      s,
			Status:     TaskStatusFailed,
			IsTerminal: true,
		}

	// ────────────────────────────────────────────────
	// 停止：doing 回退到对应的 wait，对外状态为 STOPPED
	// ────────────────────────────────────────────────
	case ActionStop:
		return StageTransition{
			Stage:      s.ToWaitStage(),
			Status:     TaskStatusStopped,
			IsTerminal: true,
		}

	// ────────────────────────────────────────────────
	// 调度：wait → doing，对外状态变为 RUNNING
	// ────────────────────────────────────────────────
	case ActionSchedule:
		if !s.IsWaitStage() {
			// 非 wait 阶段无法调度，保持原状
			return StageTransition{Stage: s, Status: TaskStatusRunning}
		}
		return StageTransition{
			Stage:      s.ToDoingStage(),
			Status:     TaskStatusRunning,
			IsTerminal: false,
		}

	// ────────────────────────────────────────────────
	// 成功：doing → 下一个 wait 或终态，status 随之动态计算
	// ────────────────────────────────────────────────
	case ActionSuccess:
		return s.transitOnSuccess(hint)

	default:
		return StageTransition{Stage: s, Status: TaskStatusQueued}
	}
}

// transitOnSuccess 处理执行成功后的流转决策
func (s TaskStage) transitOnSuccess(hint *StageTransitionHint) StageTransition {
	// 延后重排：本轮没有真正执行（在等前置任务收尾），回到本阶段的 wait。
	//
	// 语义上它不是「成功完成」，但框架只有 success / failed 两条出口，
	// 走 failed 会消耗重试次数并最终把任务判死；因此复用 success 出口 +
	// 本标志，由状态机把它翻译成「原地重排」。对外状态仍是 queued，
	// 前端看到的还是「执行中」，不会因为一次延后就给用户报完成或失败。
	if hint != nil && hint.Deferred {
		return StageTransition{
			Stage:      s.ToWaitStage(),
			Status:     TaskStatusQueued,
			IsTerminal: false,
		}
	}

	switch s {
	case StageFunctionalCallDoing:
		// 规划判定为 workflow：活已移交给对话绑定的工作流任务，本任务当轮收口。
		// 必须先于 PlanSubmitted 判断 —— 移交时同样会切 phase，
		// 若被 PlanSubmitted 分支截住，任务会回到 functional_call 再跑一遍单 Agent。
		if hint != nil && hint.PlanDelegated {
			return StageTransition{
				Stage:      StageFinished,
				Status:     TaskStatusCompleted,
				IsTerminal: true,
			}
		}

		// 规划期提交了规划（submit_plan）：已切到执行期，继续 functional_call，
		// 而不是进入 tool_call 或 finished。
		if hint != nil && hint.PlanSubmitted {
			return StageTransition{
				Stage:      StageFunctionalCallWait,
				Status:     TaskStatusRunning,
				IsTerminal: false,
			}
		}

		// LLM 调用完成 → 判断是否需要继续调用工具
		if hint != nil && hint.NeedToolCall {
			maxRound := hint.MaxToolCallRound
			if maxRound <= 0 {
				maxRound = DefaultMaxToolCallRound
			}
			if hint.ToolCallRound < maxRound {
				// 进入工具调用，对外仍是 RUNNING
				return StageTransition{
					Stage:      StageToolCallWait,
					Status:     TaskStatusRunning,
					IsTerminal: false,
				}
			}
		}
		// 无需工具调用，或轮次已耗尽 → 任务完成
		return StageTransition{
			Stage:      StageFinished,
			Status:     TaskStatusCompleted,
			IsTerminal: true,
		}

	case StageToolCallDoing:
		// 工具调用完成 → loop 回 functional_call（可能仍在规划期，也可能已执行期），
		// 让 LLM 基于工具结果继续推理
		return StageTransition{
			Stage:      StageFunctionalCallWait,
			Status:     TaskStatusRunning,
			IsTerminal: false,
		}

	case StageWorkflowDoing:
		// 工作流编排完成 → 终态。成功与否由 workflow 执行器在
		// GetAsyncTaskStatus 里依据「所有子任务是否都完成」判定：
		// 走到这里说明执行器已报 SUCCESS，直接收口 finished。
		return StageTransition{
			Stage:      StageFinished,
			Status:     TaskStatusCompleted,
			IsTerminal: true,
		}

	case StageFinished:
		// 已是终态，保持
		return StageTransition{
			Stage:      StageFinished,
			Status:     TaskStatusCompleted,
			IsTerminal: true,
		}

	default:
		// wait 阶段收到 success 属于异常，保持原状
		return StageTransition{Stage: s, Status: TaskStatusRunning}
	}
}

// NextStage 便捷方法：仅获取流转后的下一阶段
func (s TaskStage) NextStage(hint *StageTransitionHint) TaskStage {
	if s.IsWaitStage() {
		return s.Transit(ActionSchedule, hint).Stage
	}
	return s.Transit(ActionSuccess, hint).Stage
}

// AllWaitStages 返回所有已接入调度器的 wait 阶段。
//
// 实际调度由 stage_scheduler.go 的 specs 显式声明，本函数仅作为
// 状态机自检与文档用途。
func AllWaitStages() []TaskStage {
	return []TaskStage{
		StageFunctionalCallWait,
		StageToolCallWait,
		StageWorkflowWait,
	}
}

// AllDoingStages 返回所有已接入调度器的 doing 阶段（状态机自检用）
func AllDoingStages() []TaskStage {
	return []TaskStage{
		StageFunctionalCallDoing,
		StageToolCallDoing,
		StageWorkflowDoing,
	}
}

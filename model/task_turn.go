// Package model — TaskTurn 原子调用明细
//
// 设计背景：
// 任务在多个 stage 之间流转，functional_call 与 tool_call 会循环多轮。
// 为了完整回溯执行过程，把「每一次原子调用」都单独落库一条记录。
//
// 什么算一次原子调用（= 一个 turn）：
//   - planning        阶段调一次 LLM        → 1 turn
//   - functional_call 阶段调一次 LLM        → 1 turn
//   - tool_call       阶段调一个 MCP 工具   → 1 turn（一轮返回 N 个工具则记 N 条）
//   - workflow        阶段做一次调度决策    → 1 turn
//
// Task 上维护 TurnCount 累计总数，每产生一个 turn 就 +1，
// TaskTurn.TurnSeq 即该 turn 在本任务内的序号（从 1 开始，全局递增不复用）。
package model

import "time"

// ============================================================
// TurnType — 原子调用类型
// ============================================================

// TurnType 原子调用的类型 —— 「这一次调用在调什么」。
//
// 这是与 TaskPhase 正交的另一个维度，两者必须分开记录：
//
//	TurnType  ：调的是 LLM 还是工具        （functional_call / tool_call / workflow）
//	TaskPhase ：这次调用发生在哪个语义阶段  （planning / executing）
//
// 组合是完全合法且常见的，例如「规划期调工具做调研」就是
// phase=planning + turn_type=tool_call。
//
// 历史包袱：早期把 planning 当成一种 turn_type（取值 1），导致规划期的
// LLM 调用丢掉了「它是一次 functional_call」这一事实，两个维度被压成一个。
// 现在 phase 独立成列，取值 1 保留为 TurnTypePlanningLegacy 仅用于兼容存量数据
// （读取时由 NormalizeTurn 归一），新写入一律不再使用。
type TurnType int

const (
	// TurnTypePlanningLegacy 已废弃：历史上表示「planning 阶段调 LLM」。
	//
	// 不要在新代码里写入。存量记录读出来后由 NormalizeTurn 归一成
	// turn_type=functional_call + phase=planning。
	// 保留该常量而不复用取值 1，是为了让存量数据无需迁移。
	TurnTypePlanningLegacy TurnType = 1

	TurnTypeFunctionalCall TurnType = 2 // 调用 LLM
	TurnTypeToolCall       TurnType = 3 // 调用 MCP / Skill 工具
	TurnTypeWorkflow       TurnType = 4 // 工作流编排决策

	// TurnTypeContextCompact 上下文压缩（内部 LLM 调用）。
	//
	// 与上面三类不是一个层面的东西：它不是 agent 循环里的一步，而是
	// 组装输入时「上下文放不下了」触发的自救动作。落 turn 是为了让它在
	// 调试页可见 —— 压缩既烧 token 又引入延迟，看不见就无从排查。
	//
	// 明确不算 IsLLMCall：那是「agent 这一轮调过模型了吗」的判据，
	// 压缩调用不该被当成一轮对话（否则幂等跳过会误判「这轮已调过」）。
	TurnTypeContextCompact TurnType = 5

	// TurnTypeContextRecall 上下文取回（内部 LLM 调用）。
	//
	// 压缩的逆动作：压缩把历史摘要化（有损），recall_context 工具让模型
	// 拿着摘要旁挂的 task_id 把某段历史重新提取回来。按 task_id 取回时
	// 要把该 task 的全量交互喂给 LLM 按 query 提炼，素材超预算则走
	// map-reduce —— 与压缩同一套分块机制，因此同样落 turn。
	//
	// 与 TurnTypeContextCompact 同理不算 IsLLMCall：它发生在工具执行阶段，
	// 是 tool_call 内部的子动作，不是 agent 循环里的一轮模型调用。
	TurnTypeContextRecall TurnType = 6
)

// String 返回类型名称
func (t TurnType) String() string {
	switch t {
	case TurnTypePlanningLegacy:
		// 存量数据兜底。正常路径下 NormalizeTurn 已把它转成 functional_call，
		// 不会走到这里。
		return "functional_call"
	case TurnTypeFunctionalCall:
		return "functional_call"
	case TurnTypeToolCall:
		return "tool_call"
	case TurnTypeWorkflow:
		return "workflow"
	case TurnTypeContextCompact:
		return "context_compact"
	case TurnTypeContextRecall:
		return "context_recall"
	default:
		return "unknown"
	}
}

// IsLLMCall 判断该 turn 是否为 agent 循环里的一次 LLM 调用。
//
// 上下文压缩（TurnTypeContextCompact）与上下文取回（TurnTypeContextRecall）
// 虽然也调 LLM，但刻意都不算：调用方拿它判断「这一轮模型调过了没」，
// 压缩发生在组装输入阶段、取回发生在工具执行阶段，把它们算进来会让
// 幂等跳过逻辑误判成「本轮已执行」，直接吞掉真正的调用。
func (t TurnType) IsLLMCall() bool {
	return t == TurnTypeFunctionalCall || t == TurnTypePlanningLegacy
}

// TurnTypeFromStage 根据 stage 推导对应的 turn 类型。
//
// 只覆盖「stage → 类型」能唯一确定的映射。注意 phase 不参与推导 ——
// 规划期与执行期共享 functional_call 调度阶段，两者的 turn_type 都是
// functional_call，区别记在 phase 列上。
func TurnTypeFromStage(s TaskStage) TurnType {
	switch s {
	case StageFunctionalCallWait, StageFunctionalCallDoing:
		return TurnTypeFunctionalCall
	case StageToolCallWait, StageToolCallDoing:
		return TurnTypeToolCall
	case StageWorkflowWait, StageWorkflowDoing:
		return TurnTypeWorkflow
	default:
		return 0
	}
}

// ============================================================
// TurnStatus — 原子调用状态
// ============================================================

// TurnStatus 原子调用的执行状态
type TurnStatus int

const (
	TurnStatusRunning     TurnStatus = 1 // 调用中
	TurnStatusSuccess     TurnStatus = 2 // 调用成功
	TurnStatusFailed      TurnStatus = 3 // 调用失败
	TurnStatusInterrupted TurnStatus = 4 // 调用被中断（进程重启等，turn 未正常收尾）
)

// String 返回状态名称
func (s TurnStatus) String() string {
	switch s {
	case TurnStatusRunning:
		return "running"
	case TurnStatusSuccess:
		return "success"
	case TurnStatusFailed:
		return "failed"
	case TurnStatusInterrupted:
		return "interrupted"
	default:
		return "unknown"
	}
}

// ============================================================
// Turn 输入输出的结构化载荷
// ============================================================

// TurnLLMInput LLM 调用的输入快照
type TurnLLMInput struct {
	// Model 模型名 / provider
	Model string `json:"model,omitempty"`
	// SystemPrompt 系统提示词
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Messages 完整消息列表（role + content）
	Messages []TurnMessage `json:"messages,omitempty"`
	// Tools 本次提供给模型的工具名列表（快速扫一眼用）
	Tools []string `json:"tools,omitempty"`
	// ToolSchemas 工具的完整定义（含 JSON Schema 参数）。
	//
	// 真实请求一直是带 schema 发给 provider 的，但快照过去只存了工具名，
	// 调试时无法核对「给模型的参数定义对不对」。参数错误是工具调用失败的
	// 高频原因，这里落全量以便排查。
	ToolSchemas []TurnToolSchema `json:"tool_schemas,omitempty"`
	// ToolChoice 工具选择策略（auto / any / none）
	ToolChoice string `json:"tool_choice,omitempty"`
	// Options 生成参数快照
	Options map[string]interface{} `json:"options,omitempty"`
	// ContextBlocks 输入上下文的分块 token 明细。
	//
	// 与 ContextManager（上下文压缩）的五块模型对齐：system_prompt / tools /
	// 工作流摘要 / 已压缩摘要 / 未压缩历史详情 + 本轮输入与当前 task 交互。
	// 组装消息序列时顺手计量（assembleWithCompaction 反正要算水位），
	// 落在 input 快照里随 turn 持久化 —— 调试页与「上下文用量」弹窗直接读它，
	// 无需新建表或重新估算。
	ContextBlocks *TurnContextBlocks `json:"context_blocks,omitempty"`
}

// TurnContextBlocks 一次 LLM 调用输入上下文的分块 token 占用。
//
// 字段含义与 actuator.contextBlocks 一致，独立定义是为了避免 model 包
// 反向依赖 schedule 包。六项之和即 Total；ContextLimit 是该次调用所用
// 模型的最大输入 token 上限（口径=最大输入而非总窗口）。
type TurnContextBlocks struct {
	SystemPrompt    int `json:"system_prompt"`    // 块1，不可压
	Tools           int `json:"tools"`            // 块2，不可压
	WorkflowSummary int `json:"workflow_summary"` // 块3，不可压（本身已是压缩产物）
	Compacted       int `json:"compacted"`        // 块4，已压缩历史摘要
	Detail          int `json:"detail"`           // 块5，未压缩历史详情
	UserAndCurrent  int `json:"user_and_current"` // 本轮 user 输入 + 当前 task 交互（永不压缩）
	Total           int `json:"total"`            // 六项之和 = 估算的输入 token 总量
	ContextLimit    int `json:"context_limit"`    // 模型最大输入 token 上限
}

// TurnToolSchema 一个工具的完整定义快照
type TurnToolSchema struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters JSON Schema（type/properties/required）
	Parameters map[string]interface{} `json:"parameters,omitempty"`
}

// TurnMessage 一条消息（用于输入快照）
//
// 对应 llm.Message 的三种形态，快照必须完整覆盖，否则调试页会看到
// 「assistant.content 为空」这种无信息量的记录：
//  1. 普通文本消息      → Role + Content
//  2. assistant 发起调用 → Role=assistant + ToolCalls（Content 天然为空）
//  3. 工具执行结果      → Role=tool + Content + ToolName
type TurnMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls assistant 消息携带的工具调用请求（形态 2）。
	// 模型决定调工具时 Content 就是空字符串，真正的语义全在这里。
	ToolCalls []TurnToolCallReq `json:"tool_calls,omitempty"`
	// ToolName 工具结果消息对应的工具名（形态 3）
	ToolName string `json:"tool_name,omitempty"`
}

// TurnLLMOutput LLM 调用的输出快照
type TurnLLMOutput struct {
	// Text 模型返回的纯文本
	Text string `json:"text,omitempty"`
	// ToolCalls 模型请求的工具调用列表
	ToolCalls []TurnToolCallReq `json:"tool_calls,omitempty"`
	// PromptTokens / CompletionTokens / TotalTokens token 消耗
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// TurnToolCallReq 模型发起的一次工具调用请求
type TurnToolCallReq struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
	// Signature provider 侧的推理签名（Gemini thoughtSignature）。
	//
	// 必须持久化：agent 历史是从 task_turn 重建的，而 Gemini 3 要求回放
	// functionCall 时原样带回签名，签名丢了整条会话就无法继续（400）。
	// 并行调用中只有第一个 call 有签名，其余为空属正常。
	Signature string `json:"signature,omitempty"`
}

// TurnToolInput 工具调用的输入快照
type TurnToolInput struct {
	// ToolName 工具名
	ToolName string `json:"tool_name"`
	// ServerName MCP server 名
	ServerName string `json:"server_name,omitempty"`
	// Arguments 调用参数
	Arguments map[string]interface{} `json:"arguments,omitempty"`
	// Summary 模型生成的一句话说明，描述「这次调用要做什么」。
	//
	// 由 functional_call 阶段的模型回复文本（assistant content）携带，
	// tool_call 阶段执行时透传进来，供思考过程默认折叠展示。
	// 为空时展示层退化为「调用工具 X」。
	Summary string `json:"summary,omitempty"`
}

// TurnToolOutput 工具调用的输出快照
type TurnToolOutput struct {
	// Content 工具返回内容（文本化）
	Content string `json:"content,omitempty"`
	// IsError 工具是否返回业务错误
	IsError bool `json:"is_error,omitempty"`
	// Raw 原始返回（保留结构化数据）
	Raw interface{} `json:"raw,omitempty"`
}

// ============================================================
// TaskTurn — 原子调用明细表
// ============================================================

// TaskTurn 记录任务执行过程中的每一次原子调用。
//
// 一个 Task 会有多条 TaskTurn，按 TurnSeq 升序即为完整执行时间线。
// 该表只追加不修改（除了 turn 结束时回填 output/status/耗时），便于审计与回溯。
type TaskTurn struct {
	// ID 自增主键
	ID int64 `gorm:"column:id;primaryKey;autoIncrement"`
	// TaskID 所属任务（联合索引主键，按 task_id + turn_seq 查询时间线）
	TaskID string `gorm:"column:task_id;size:100;not null;index:task_id_turn_seq_idx,priority:1,unique;index:task_id_turn_type_seq_idx,priority:1"`
	// TurnSeq 该 turn 在本任务内的序号，从 1 开始
	TurnSeq int32 `gorm:"column:turn_seq;not null;index:task_id_turn_seq_idx,priority:2,unique;index:task_id_turn_type_seq_idx,priority:3"`
	// ChatID 所属对话（便于按对话维度统计）
	// idx_turn_chat_created 覆盖 ListTurnsByChatID 的 chat_id=? + ORDER BY created_time，
	// 避免历史重建（每轮 agent loop 触发）时对长字段表做 filesort。
	ChatID string `gorm:"column:chat_id;size:100;not null;index:idx_turn_chat_created,priority:1"`
	// ParentTaskID 父任务 ID（工作流子任务时有值）
	ParentTaskID string `gorm:"column:parent_task_id;size:100"`

	// TurnType 原子调用类型（functional_call / tool_call / workflow）。
	// task_id_turn_type_seq_idx 覆盖 GetLatestTurnByType / CountTurnsByType 的
	// task_id=? AND turn_type=? 查询，这是 agent loop 每轮的高频路径。
	TurnType TurnType `gorm:"column:turn_type;not null;index:task_id_turn_type_seq_idx,priority:2"`
	// Phase 该 turn 发生时任务所处的语义阶段（1=规划期 / 2=执行期）。
	//
	// 与 TurnType 正交：规划期同样会调 LLM 与工具，所以
	// (phase=planning, turn_type=tool_call) 这类组合是正常的。
	//
	// 存量记录该列为 0，由 NormalizeTurn 依据 turn_type 反推补齐。
	Phase int `gorm:"column:phase;not null;default:0"`
	// Stage 该 turn 发生时任务所处的 stage
	Stage int `gorm:"column:stage;not null;default:0"`
	// Status 调用状态（running / success / failed）
	Status TurnStatus `gorm:"column:status;not null;default:1"`

	// Round 循环轮次。functional_call ↔ tool_call 来回循环时的第几轮，从 0 开始
	Round int32 `gorm:"column:round;not null;default:0"`

	// ── 调用元信息（便于不解 JSON 直接按维度查询/聚合）──

	// Model LLM 调用时的模型名 / provider 名
	Model string `gorm:"column:model;size:128"`
	// ToolName 工具调用时的工具名
	ToolName string `gorm:"column:tool_name;size:128;index"`
	// ServerName 工具调用时的 MCP server 名
	ServerName string `gorm:"column:server_name;size:128"`
	// TotalTokens LLM 调用消耗的总 token（便于成本统计）
	TotalTokens int `gorm:"column:total_tokens;not null;default:0"`
	// PromptTokens LLM 调用消耗的**输入** token。
	//
	// 单独成列而不是只留 total，是因为它是「上下文水位」唯一正确的度量：
	// 系统提示 + 历史消息 + 工具 schema 全算在输入里，上下文管理（遮蔽 / 摘要）
	// 要压的就是这一项。而 completion 是模型输出，压它只会让回答变短。
	//
	// 存量记录该列为 0（值在 output 快照 JSON 里），可用 cmd/tools/backfill_tokens 回填。
	PromptTokens int `gorm:"column:prompt_tokens;not null;default:0"`
	// CompletionTokens LLM 调用消耗的**输出** token。
	//
	// 单独成列的另一个理由：input / output 单价通常差 3–5 倍，
	// 只有一个 total 没法算真实成本，也没法看出「输入暴涨」与「输出暴涨」的区别。
	CompletionTokens int `gorm:"column:completion_tokens;not null;default:0"`

	// ── 输入输出（JSON 字符串，完整快照）──

	// Input 输入快照。TurnLLMInput 或 TurnToolInput 序列化后的 JSON
	Input string `gorm:"column:input;type:longtext"`
	// Output 输出快照。TurnLLMOutput 或 TurnToolOutput 序列化后的 JSON
	Output string `gorm:"column:output;type:longtext"`
	// ErrorMsg 失败原因（Status=failed 时有值）
	ErrorMsg string `gorm:"column:error_msg;type:text"`

	// ── 时间与耗时 ──

	// StartTime 调用开始时间
	StartTime time.Time `gorm:"column:start_time;not null"`
	// EndTime 调用结束时间
	EndTime *time.Time `gorm:"column:end_time"`
	// TimeCostMs 耗时（毫秒）
	TimeCostMs int64 `gorm:"column:time_cost_ms;not null;default:0"`
	// CreatedAt 记录创建时间
	CreatedAt time.Time `gorm:"column:created_time;not null;index:idx_turn_chat_created,priority:2"`
	// UpdatedAt 记录更新时间
	UpdatedAt time.Time `gorm:"column:updated_time;not null;autoUpdateTime"`
}

// TableName 表名
func (TaskTurn) TableName() string { return "task_turn" }

// NormalizeTurn 把存量记录归一到「双维度」表示，读路径统一调用。
//
// 处理两类历史数据：
//  1. turn_type=1（TurnTypePlanningLegacy）→ turn_type=functional_call + phase=planning
//  2. phase=0（该列上线前的记录）→ 按 turn_type 反推：
//     legacy planning 记为规划期，其余一律记为执行期
//
// 幂等：新写入的记录两个字段都已正确，调用它不产生变化。
func NormalizeTurn(t *TaskTurn) *TaskTurn {
	if t == nil {
		return nil
	}
	if t.TurnType == TurnTypePlanningLegacy {
		t.TurnType = TurnTypeFunctionalCall
		if t.Phase == 0 {
			t.Phase = int(PhasePlanning)
		}
	}
	if t.Phase == 0 {
		// 该列上线前的 functional_call / tool_call 记录都属于执行期 ——
		// 那时规划期的 LLM 调用一律写成 turn_type=1，已在上面处理。
		t.Phase = int(PhaseExecuting)
	}
	return t
}

// NormalizeTurns 批量归一，返回原切片便于链式使用
func NormalizeTurns(turns []*TaskTurn) []*TaskTurn {
	for _, t := range turns {
		NormalizeTurn(t)
	}
	return turns
}

// TurnPhase 返回该 turn 的语义阶段
func (t *TaskTurn) TurnPhase() TaskPhase {
	return TaskPhase(t.Phase)
}

// IsPlanning 判断该 turn 是否发生在规划期
func (t *TaskTurn) IsPlanning() bool {
	return TaskPhase(t.Phase) == PhasePlanning
}

// IsTerminal 判断该 turn 是否已结束
func (t *TaskTurn) IsTerminal() bool {
	return t.Status == TurnStatusSuccess || t.Status == TurnStatusFailed || t.Status == TurnStatusInterrupted
}

// Duration 返回该 turn 的耗时
func (t *TaskTurn) Duration() time.Duration {
	if t.EndTime == nil {
		return time.Since(t.StartTime)
	}
	return t.EndTime.Sub(t.StartTime)
}

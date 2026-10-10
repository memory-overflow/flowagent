package model

import "time"

// ============================================================
// TaskContextSummary — 任务级「当前交互压缩水位」
// ============================================================

// TaskContextSummary 单个任务自身交互的压缩水位。
//
// 与对话级 ContextSummary 的分工：
//   - ContextSummary 存**对话**视角的水位：历史 task 摘要 + 当前 task 摘要，
//     一个对话一行（chat_id 主键）。它服务的是「主对话的 agent loop」。
//   - 本表存**任务**视角的水位（task_id 主键），目前只有一类使用者：
//     工作流节点子任务。
//
// 为什么子任务不能复用对话级那一行：工作流节点是**并发**跑的，同一时刻
// 可能有多个节点子任务在同一个对话下推进。它们共享 chat_id，若都往
// context_summary.current_* 写，后一个节点会直接覆盖前一个节点的水位，
// 被覆盖的那个节点下一轮就会「水位丢失 → 从头重压」，既烧 LLM 又让
// 摘要反复漂移。水位细化到 task 之后，各节点互不可见，天然隔离。
//
// 子任务也不读对话级摘要：它的上下文刻意不含主对话历史（见
// workflowSubTaskExecImpl），节点只认「工作流目标 + 本步骤说明 + 上游
// 输出 + 用户附件」，对话历史的压缩产物对它毫无意义。
type TaskContextSummary struct {
	// TaskID 任务 ID，主键。一个任务至多一行。
	TaskID string `gorm:"column:task_id;size:100;primaryKey" json:"task_id"`
	// CompactedMsgs 该任务交互序列中已被摘要代表的消息条数。
	//
	// 语义与 ContextSummary.CurrentCompactedMsgs 完全一致：任务的消息
	// 序列只增不减（新 turn 追加在末尾），前 N 条在任何一轮重建都指向
	// 同一批内容，因此「条数」本身就是可靠的水位。
	CompactedMsgs int `gorm:"column:compacted_msgs;not null;default:0" json:"compacted_msgs"`
	// CompactedTurnIDs 已被摘要代表的 turn ID 列表（JSON 数组，元素为
	// task_turn.id）。语义与 ContextSummary.CurrentCompactedTurnIDs 一致：
	// 条数是水位，这里是「被压掉的是哪些 turn」，供后续按 turn 取回原文。
	CompactedTurnIDs string `gorm:"column:compacted_turn_ids;type:text" json:"compacted_turn_ids"`
	// Summary 该任务早期交互的压缩摘要正文。
	Summary string `gorm:"column:summary;type:longtext" json:"summary"`
	// SummaryTokens 摘要 token 数，写入时计量并缓存，避免每轮重建重复编码。
	SummaryTokens int `gorm:"column:summary_tokens;not null;default:0" json:"summary_tokens"`
	// CompactCount 累计压缩次数，仅用于可观测性。
	CompactCount int `gorm:"column:compact_count;not null;default:0" json:"compact_count"`
	// OriginalTokens 被压缩掉的原始交互 token 总量（累计）。
	OriginalTokens int       `gorm:"column:original_tokens;not null;default:0" json:"original_tokens"`
	CreatedAt      time.Time `gorm:"column:created_time;not null" json:"created_time"`
	UpdatedAt      time.Time `gorm:"column:updated_time;not null;autoUpdateTime" json:"updated_time"`
}

func (TaskContextSummary) TableName() string { return "task_context_summary" }

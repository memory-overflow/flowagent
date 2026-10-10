package model

import (
	"encoding/json"
	"strings"
	"time"
)

// ============================================================
// ContextSummary — 对话级「已压缩历史摘要」
// ============================================================

// ContextSummary 一个对话当前的已压缩历史摘要。
//
// 定位：模型输入被拆成 5 块（system_prompt / tools / 工作流摘要 /
// 已压缩历史摘要 / 未压缩历史详情），本表存的正是第 4 块。
//
// 为什么必须落库而不是放内存：agent loop 每轮都从 task_turn 重建历史
// （见 actuator/agent_history.go 的设计说明 —— 调度器可能多副本、
// 进程会重启）。压缩结果若只在内存里，下一轮重建就丢了，会反复压缩
// 同一段历史，既浪费 LLM 调用又让摘要不断漂移。
//
// 一个对话恰好一行（chat_id 为主键），压缩是「滚动更新」语义：
// 每次压缩把「旧摘要 + 新纳入的历史详情」合成一份新摘要覆盖写回，
// 而不是追加多行。这样第 4 块永远只有一段文本，体量受
// contextmgr.SummaryRatio 约束，不会自我膨胀。
type ContextSummary struct {
	// ChatID 对话 ID，主键。一个对话至多一行。
	ChatID string `gorm:"column:chat_id;size:100;primaryKey" json:"chat_id"`
	// Summary 已压缩历史的摘要正文。作为一条 assistant 消息注入历史开头。
	Summary string `gorm:"column:summary;type:longtext" json:"summary"`
	// CoveredTaskIDs 已被这份摘要覆盖的 task ID 列表（JSON 数组）。
	//
	// 这是压缩的「水位线」：重建历史时，凡 task_id 命中该集合的 turn
	// 一律不再回放原始详情，改由 Summary 代表。用显式 ID 集合而非
	// 「时间戳边界」是刻意的 —— task 的 created_time 可能相同或乱序
	// （并发创建），用边界会误伤未压缩的 task。
	CoveredTaskIDs string `gorm:"column:covered_task_ids;type:text" json:"covered_task_ids"`
	// CoveredTaskIndex 覆盖任务的「可检索索引」（JSON 数组，见 TaskIndexEntry）。
	//
	// 与 CoveredTaskIDs 的区别：那个只是判定用的 ID 集合，这个是给模型看的
	// 目录（ID + 一句话标签）。必须由代码写入、绝不能交给压缩 LLM 生成 ——
	// 压缩是多层 map-reduce 合并，中间步骤一定会丢 ID 甚至编造 ID。
	//
	// 落库而非每次现算：标签取自被压掉的那批消息，压完就没了，
	// 不存下来后续轮次再算不出来。
	CoveredTaskIndex string `gorm:"column:covered_task_index;type:text" json:"covered_task_index"`
	// SummaryTokens 摘要的 token 数（写入时用 contextmgr 计量并缓存）。
	// 缓存起来避免每轮重建历史时重复编码这段长文本。
	SummaryTokens int `gorm:"column:summary_tokens;not null;default:0" json:"summary_tokens"`
	// CompactCount 累计压缩次数。仅用于可观测性：次数异常增长说明
	// 阈值配置不合理（压完一轮马上又触发）。
	CompactCount int `gorm:"column:compact_count;not null;default:0" json:"compact_count"`
	// OriginalTokens 被压缩掉的历史详情原始 token 总量（累计）。
	// 与 SummaryTokens 对比即得压缩比，供调试页展示。
	OriginalTokens int `gorm:"column:original_tokens;not null;default:0" json:"original_tokens"`

	// ---- 以下四个字段是「当前 task 上下文压缩」的水位 ----
	//
	// 与上面「历史 task 压缩」的区别：历史压缩的对象是**已结束**的 task，
	// 压完就永久不再回放；当前压缩的对象是**正在执行**的 task 的早期交互，
	// 它还在增长，因此水位必须细化到「这个 task 的前 N 条消息已压」，
	// 不能简单把整个 task 标记为 covered —— 那会把最新几轮的交互也吃掉。

	// CurrentTaskID 当前压缩水位所属的 task ID。
	//
	// 换 task 时（用户发了新消息 → 新 task）旧水位要归位：
	// 旧 task 已结束，其压缩摘要并入 Summary、task ID 并入 CoveredTaskIDs。
	CurrentTaskID string `gorm:"column:current_task_id;size:100" json:"current_task_id"`
	// CurrentCompactedMsgs 当前 task 交互序列中已被压缩的消息条数。
	//
	// 用「条数」而非 turn ID 集合作水位：当前 task 的消息序列只增不减
	// （新 turn 追加在末尾），前 N 条在任何一轮重建都指向同一批内容。
	// turn ID 集合当然更严谨，但当前 task 每压一轮就要重写整个集合，
	// 40+ 个 UUID 的 JSON 写起来没必要 —— 真正的风险（误吃掉新消息）
	// 由「只增不减」这条性质排除。
	CurrentCompactedMsgs int `gorm:"column:current_compacted_msgs;not null;default:0" json:"current_compacted_msgs"`
	// CurrentCompactedTurnIDs 已被当前摘要代表的 turn ID 列表（JSON 数组，
	// 元素为 task_turn.id）。
	//
	// 与 CurrentCompactedMsgs 是同一件事的两种表示：条数是水位（判定用），
	// 这里是「被压掉的是哪些 turn」（取回用）。有了它，模型才能指名道姓地
	// 回看某一轮被压掉的原始记录，而不是面对一段不可逆的摘要。
	CurrentCompactedTurnIDs string `gorm:"column:current_compacted_turn_ids;type:text" json:"current_compacted_turn_ids"`
	// CurrentSummary 当前 task 早期交互的压缩摘要。
	//
	// 独立于 Summary 是必须的：它代表的是**晚于**全部历史 task 才发生的事，
	// 注入位置在当前 task 未压缩交互之前（而不是历史最前），
	// 合并进 Summary 会造成时序错乱。
	CurrentSummary string `gorm:"column:current_summary;type:longtext" json:"current_summary"`
	// CurrentSummaryTokens 当前 task 摘要的 token 数，缓存避免重复编码。
	CurrentSummaryTokens int       `gorm:"column:current_summary_tokens;not null;default:0" json:"current_summary_tokens"`
	CreatedAt            time.Time `gorm:"column:created_time;not null" json:"created_time"`
	UpdatedAt            time.Time `gorm:"column:updated_time;not null;autoUpdateTime" json:"updated_time"`
}

func (ContextSummary) TableName() string { return "context_summary" }

// CoveredTasks 解析已覆盖的 task ID 集合。
//
// 解析失败时返回空集合而非 error：摘要元数据损坏不该让整个对话跑不动，
// 退化成「没有任何 task 被压缩」是安全方向 —— 历史会全量回放，
// 可能触发一次新的压缩，但不会丢数据、也不会漏放 turn。
func (c *ContextSummary) CoveredTasks() map[string]bool {
	out := make(map[string]bool)
	if c == nil || c.CoveredTaskIDs == "" {
		return out
	}
	var ids []string
	if err := json.Unmarshal([]byte(c.CoveredTaskIDs), &ids); err != nil {
		return out
	}
	for _, id := range ids {
		if id != "" {
			out[id] = true
		}
	}
	return out
}

// SetCoveredTasks 序列化 task ID 集合写入 CoveredTaskIDs。
//
// 顺序无关（读取端只做集合判定），但仍按传入顺序写入，便于人工排查时
// 对照 task 的时间序。
func (c *ContextSummary) SetCoveredTasks(ids []string) error {
	if len(ids) == 0 {
		c.CoveredTaskIDs = "[]"
		return nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	c.CoveredTaskIDs = string(b)
	return nil
}

// TaskIndexEntry 一条「可检索任务」索引项。
//
// 只带两个字段是刻意的：ID 是句柄，Label 让模型能判断「是不是我要找的
// 那一轮」。再多（时间戳、token 数…）就是给模型加噪声，且都要额外落库。
type TaskIndexEntry struct {
	TaskID string `json:"task_id"`
	Label  string `json:"label"`
}

// CoveredTaskIndexEntries 解析覆盖任务的索引。
//
// 解析失败返回 nil（安全方向：退化成「没有索引」，只是模型少一份目录，
// 不影响压缩水位本身 —— 水位在 CoveredTaskIDs 里，两者互不影响）。
func (c *ContextSummary) CoveredTaskIndexEntries() []TaskIndexEntry {
	if c == nil || strings.TrimSpace(c.CoveredTaskIndex) == "" {
		return nil
	}
	var out []TaskIndexEntry
	if err := json.Unmarshal([]byte(c.CoveredTaskIndex), &out); err != nil {
		return nil
	}
	return out
}

// setCoveredTaskIndex 写入覆盖任务索引（不做去重，调用方负责）。
func (c *ContextSummary) setCoveredTaskIndex(entries []TaskIndexEntry) {
	if len(entries) == 0 {
		c.CoveredTaskIndex = ""
		return
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return
	}
	c.CoveredTaskIndex = string(b)
}

// AppendCoveredTaskIndex 追加索引项，按 TaskID 去重并保持原有顺序。
//
// 超出 maxCoveredTaskIndexEntries 时丢最旧的：索引是给模型看的目录，
// 太旧的任务被回取的概率极低，而它占的是**上下文**（注入时按条数截断，
// 但存储也要有上限，避免长对话把这一列撑成巨文本）。
func (c *ContextSummary) AppendCoveredTaskIndex(added ...TaskIndexEntry) {
	entries := c.CoveredTaskIndexEntries()
	seen := make(map[string]bool, len(entries)+len(added))
	for _, e := range entries {
		seen[e.TaskID] = true
	}
	for _, e := range added {
		if e.TaskID == "" || seen[e.TaskID] {
			continue
		}
		seen[e.TaskID] = true
		entries = append(entries, e)
	}
	if n := len(entries); n > maxCoveredTaskIndexEntries {
		entries = entries[n-maxCoveredTaskIndexEntries:]
	}
	c.setCoveredTaskIndex(entries)
}

// maxCoveredTaskIndexEntries 覆盖任务索引的存储上限（超出丢最旧）。
const maxCoveredTaskIndexEntries = 60

// CurrentCompactedTurns 解析已被当前摘要代表的 turn ID 列表。
func (c *ContextSummary) CurrentCompactedTurns() []int64 {
	if c == nil || strings.TrimSpace(c.CurrentCompactedTurnIDs) == "" {
		return nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(c.CurrentCompactedTurnIDs), &ids); err != nil {
		return nil
	}
	return ids
}

// SetCurrentCompactedTurns 写入当前摘要覆盖的 turn ID 列表。
func (c *ContextSummary) SetCurrentCompactedTurns(ids []int64) {
	if len(ids) == 0 {
		c.CurrentCompactedTurnIDs = ""
		return
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return
	}
	c.CurrentCompactedTurnIDs = string(b)
}

// HasCurrentCompaction 当前 task 是否存在压缩水位。
func (c *ContextSummary) HasCurrentCompaction() bool {
	return c != nil && c.CurrentTaskID != "" && strings.TrimSpace(c.CurrentSummary) != ""
}

// RollOverCurrent 把当前 task 的压缩水位归位到历史摘要。
//
// 对话进入新 task 时调用（用户发了新消息 → 新 task 接手）：旧 task 已结束，
// 它那份「早期交互摘要」从此变成普通历史，应当并入 Summary；同时把旧 task
// 的 ID 并入覆盖集合，这样它剩余的交互也不再回放 —— 它已经有摘要代表了。
//
// 不在这里落库：调用方拿到改动后的对象自行保存，便于和其他字段一起提交。
func (c *ContextSummary) RollOverCurrent() {
	if !c.HasCurrentCompaction() {
		c.ResetCurrent()
		return
	}

	if strings.TrimSpace(c.Summary) == "" {
		c.Summary = c.CurrentSummary
		c.SummaryTokens = c.CurrentSummaryTokens
	} else {
		c.Summary = c.Summary + "\n\n" + c.CurrentSummary
		if c.SummaryTokens > 0 || c.CurrentSummaryTokens > 0 {
			c.SummaryTokens += c.CurrentSummaryTokens
		}
	}

	covered := c.CoveredTasks()
	if !covered[c.CurrentTaskID] {
		covered[c.CurrentTaskID] = true
		ids := make([]string, 0, len(covered))
		for id := range covered {
			ids = append(ids, id)
		}
		// 序列化失败不该中断归位：最坏结果是旧 task 的剩余交互被重新回放
		// 一次（内容重复但无害），下轮压缩会把它收进新的历史摘要。
		_ = c.SetCoveredTasks(ids)
	}
	// 旧 task 整段进历史：它的 turn 级索引不再需要 —— 此后要回看它，
	// 按 task_id 整段取回即可（task 已经在覆盖集合里）。这里补一条索引项，
	// 标签取摘要首行，让模型在目录里认得出这一轮。
	c.AppendCoveredTaskIndex(TaskIndexEntry{
		TaskID: c.CurrentTaskID,
		Label:  firstLineLabel(c.CurrentSummary),
	})
	c.ResetCurrent()
}

// ResetCurrent 清空当前 task 的压缩水位（不动 Summary 与覆盖集合）。
func (c *ContextSummary) ResetCurrent() {
	c.CurrentTaskID = ""
	c.CurrentCompactedMsgs = 0
	c.CurrentSummary = ""
	c.CurrentSummaryTokens = 0
	c.CurrentCompactedTurnIDs = ""
}

// firstLineLabel 取一段文本的首行作为索引标签，超长截断。
func firstLineLabel(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > 40 {
		s = string(runes[:40]) + "…"
	}
	return s
}

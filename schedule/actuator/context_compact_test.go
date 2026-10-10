package actuator

import (
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// seg 构造一个带指定 token 数的可压缩对话段。
func seg(taskID string, tokens int) historySegment {
	return historySegment{
		TaskID:   taskID,
		Kind:     segKindConversation,
		Tokens:   tokens,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: taskID}},
	}
}

// sumTokens 汇总段的 token。
func sumTokens(segs []historySegment) int {
	n := 0
	for i := range segs {
		n += segs[i].Tokens
	}
	return n
}

// ============================================================
// 选取算法：前 80% + task 原子性
// ============================================================

func TestPickSegments_Empty(t *testing.T) {
	picked, tokens := pickSegmentsToCompact(nil, 0)
	if len(picked) != 0 || tokens != 0 {
		t.Fatalf("empty input should pick nothing, got %d segs / %d tokens", len(picked), tokens)
	}
}

func TestPickSegments_TakesRoughly80Percent(t *testing.T) {
	// 10 段各 100 token，总 1000，预算 800 → 应取 8 段
	segs := make([]historySegment, 0, 10)
	for i := 0; i < 10; i++ {
		segs = append(segs, seg(string(rune('a'+i)), 100))
	}
	picked, tokens := pickSegmentsToCompact(segs, sumTokens(segs))
	if len(picked) != 8 {
		t.Errorf("want 8 segments picked, got %d", len(picked))
	}
	if tokens != 800 {
		t.Errorf("want 800 tokens, got %d", tokens)
	}
}

func TestPickSegments_NeverSplitsATask(t *testing.T) {
	// 关键不变量：选出的段必须是原样的整段，绝不能出现「半个 task」。
	// 半压会切断 assistant tool_calls 与 tool 结果的配对，provider 判 400。
	segs := []historySegment{
		seg("t1", 300), seg("t2", 300), seg("t3", 300), seg("t4", 100),
	}
	picked, _ := pickSegmentsToCompact(segs, sumTokens(segs))
	for i := range picked {
		// 段的 token 数必须与原段完全一致（没被按比例裁过）
		if picked[i].Tokens != segs[i].Tokens {
			t.Errorf("segment %d was split: want %d tokens, got %d",
				i, segs[i].Tokens, picked[i].Tokens)
		}
		if picked[i].TaskID != segs[i].TaskID {
			t.Errorf("segment %d task mismatch: want %s, got %s",
				i, segs[i].TaskID, picked[i].TaskID)
		}
	}
}

func TestPickSegments_PickedIsAlwaysAPrefix(t *testing.T) {
	// 压缩语义要求选中的必须是「最早的一段连续前缀」：
	// 摘要代表的是被吃掉的那批最早 task，若选中的不连续，
	// 时序就乱了（摘要里混着中间某段，而它前后的原始详情都还在）
	segs := []historySegment{
		seg("t1", 50), seg("t2", 50), seg("t3", 50), seg("t4", 50), seg("t5", 50),
	}
	picked, _ := pickSegmentsToCompact(segs, sumTokens(segs))
	for i := range picked {
		if picked[i].TaskID != segs[i].TaskID {
			t.Fatalf("picked is not a prefix: index %d is %s, want %s",
				i, picked[i].TaskID, segs[i].TaskID)
		}
	}
}

func TestPickSegments_SingleHugeTaskIsForciblyTaken(t *testing.T) {
	// 只有一个 task 且它就是全部详情：80% 预算装不下 100% 的它。
	// 必须强制取它，否则压缩循环空转，最终带着超限输入去调 provider。
	segs := []historySegment{seg("only", 500000)}
	picked, tokens := pickSegmentsToCompact(segs, 500000)
	if len(picked) != 1 {
		t.Fatalf("single huge task must be forcibly taken, got %d segs", len(picked))
	}
	if tokens != 500000 {
		t.Errorf("want 500000 tokens, got %d", tokens)
	}
}

func TestPickSegments_FirstTaskOverBudgetIsForciblyTaken(t *testing.T) {
	// 第一个 task 单独就超过 80% 预算（典型：一次巨型 deep_search）。
	// 不强制推进就永远压不动它 —— 而它正是撑爆窗口的元凶。
	segs := []historySegment{
		seg("huge", 900), seg("small1", 50), seg("small2", 50),
	}
	// 总 1000，预算 800，但首段 900 > 800
	picked, tokens := pickSegmentsToCompact(segs, sumTokens(segs))
	if len(picked) != 1 || picked[0].TaskID != "huge" {
		t.Fatalf("oversized first task must be forcibly taken, got %v", picked)
	}
	if tokens != 900 {
		t.Errorf("want 900 tokens, got %d", tokens)
	}
}

func TestPickSegments_KeepsLastTaskWhenAllWouldBeTaken(t *testing.T) {
	// 全部段都落在预算内时，至少保留最后一个 task 的原始详情：
	// 最近一轮的交互细节（用户刚问了什么、上个工具返回了什么）
	// 对继续作业最关键，全压成摘要会明显降低回答质量。
	segs := []historySegment{seg("t1", 10), seg("t2", 10), seg("t3", 10)}
	// 预算给得极大，三段全都装得下
	picked, tokens := pickSegmentsToCompact(segs, 100000)
	if len(picked) != 2 {
		t.Fatalf("should keep the last task uncompressed, want 2 picked, got %d", len(picked))
	}
	if picked[len(picked)-1].TaskID != "t2" {
		t.Errorf("last picked should be t2, got %s", picked[len(picked)-1].TaskID)
	}
	if tokens != 20 {
		t.Errorf("want 20 tokens, got %d", tokens)
	}
}

func TestPickSegments_SingleSegmentNotReducedToEmpty(t *testing.T) {
	// 「保留最后一段」的规则不能把唯一一段也让出去 —— 那样一个都不选，
	// 压缩无法推进。只剩一段时应走强制推进分支。
	segs := []historySegment{seg("only", 10)}
	picked, _ := pickSegmentsToCompact(segs, 100000)
	if len(picked) != 1 {
		t.Fatalf("single segment must still be picked, got %d", len(picked))
	}
}

func TestPickSegments_ReturnedTokensMatchPickedSegments(t *testing.T) {
	// 返回的 token 数必须与选中段的实际总和一致 ——
	// 它会被累加进 OriginalTokens 用于统计压缩比，算错会让指标失真
	segs := []historySegment{
		seg("t1", 123), seg("t2", 456), seg("t3", 789), seg("t4", 1000),
	}
	picked, tokens := pickSegmentsToCompact(segs, sumTokens(segs))
	if got := sumTokens(picked); got != tokens {
		t.Errorf("returned tokens %d != sum of picked %d", tokens, got)
	}
}

// ============================================================
// 五块 token 明细
// ============================================================

func TestContextBlocks_Total(t *testing.T) {
	b := contextBlocks{
		SystemPrompt: 1, Tools: 2, WorkflowSummary: 4,
		Compacted: 8, Detail: 16, UserAndCurrent: 32,
	}
	if b.Total() != 63 {
		t.Errorf("total want 63, got %d", b.Total())
	}
}

func TestContextBlocks_Incompressible(t *testing.T) {
	// 不可压 = system + tools + 工作流摘要 + 本轮/当前 task
	// （按决策当前 task 永不压缩，故它算作不可压）
	b := contextBlocks{
		SystemPrompt: 1, Tools: 2, WorkflowSummary: 4,
		Compacted: 8, Detail: 16, UserAndCurrent: 32,
	}
	if got := b.Incompressible(); got != 39 {
		t.Errorf("incompressible want 39 (1+2+4+32), got %d", got)
	}
}

// ============================================================
// 分段历史的拍平
// ============================================================

func TestFlattenSegments_PreservesOrder(t *testing.T) {
	segs := []historySegment{
		{TaskID: "t1", Kind: segKindConversation, Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "q1"},
			{Role: llm.RoleAssistant, Content: "a1"},
		}},
		{TaskID: "wf", Kind: segKindWorkflowSummary, Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: "wf-summary"},
		}},
		{TaskID: "t2", Kind: segKindConversation, Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "q2"},
			{Role: llm.RoleAssistant, Content: "a2"},
		}},
	}
	out := flattenSegments(segs)
	want := []string{"q1", "a1", "wf-summary", "q2", "a2"}
	if len(out) != len(want) {
		t.Fatalf("want %d messages, got %d", len(want), len(out))
	}
	for i := range want {
		if out[i].Content != want[i] {
			t.Errorf("message %d: want %q, got %q", i, want[i], out[i].Content)
		}
	}
}

func TestFlattenSegments_Empty(t *testing.T) {
	if out := flattenSegments(nil); out != nil {
		t.Errorf("nil segments should flatten to nil, got %v", out)
	}
}

func TestFlattenSegments_MergesAdjacentUsersAcrossSegments(t *testing.T) {
	// 某 task 清理后只剩一条 user 提问时，它会与下一个 task 的提问相邻。
	// 部分 provider 对连续同角色消息直接报错，必须折叠。
	segs := []historySegment{
		{TaskID: "t1", Kind: segKindConversation, Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "q1"},
		}},
		{TaskID: "t2", Kind: segKindConversation, Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "q2"},
			{Role: llm.RoleAssistant, Content: "a2"},
		}},
	}
	out := flattenSegments(segs)
	for i := 1; i < len(out); i++ {
		if out[i].Role == llm.RoleUser && out[i-1].Role == llm.RoleUser {
			t.Fatalf("adjacent user messages not merged at index %d: %v", i, out)
		}
	}
}

// ============================================================
// 摘要覆盖集合的序列化往返
// ============================================================

func TestContextSummary_CoveredTasksRoundTrip(t *testing.T) {
	s := &model.ContextSummary{}
	ids := []string{"task-a", "task-b", "task-c"}
	if err := s.SetCoveredTasks(ids); err != nil {
		t.Fatalf("SetCoveredTasks failed: %v", err)
	}
	got := s.CoveredTasks()
	if len(got) != 3 {
		t.Fatalf("want 3 covered tasks, got %d", len(got))
	}
	for _, id := range ids {
		if !got[id] {
			t.Errorf("task %s should be covered", id)
		}
	}
}

func TestContextSummary_CoveredTasksNilSafe(t *testing.T) {
	// 压缩摘要不存在是正常状态（对话还没长到需要压缩），
	// 对 nil 取覆盖集合必须安全返回空集，而非 panic
	var s *model.ContextSummary
	if got := s.CoveredTasks(); len(got) != 0 {
		t.Errorf("nil summary should yield empty set, got %v", got)
	}
}

func TestContextSummary_CorruptedMetadataDegradesGracefully(t *testing.T) {
	// 元数据损坏时退化成「没有任何 task 被压缩」：历史会全量回放，
	// 可能触发一次新压缩，但不丢数据、不漏放 turn —— 这是安全方向。
	// 若改成返回 error 并中断，一条坏数据就能让整个对话跑不动。
	s := &model.ContextSummary{CoveredTaskIDs: "{not valid json"}
	if got := s.CoveredTasks(); len(got) != 0 {
		t.Errorf("corrupted metadata should yield empty set, got %v", got)
	}
}

func TestContextSummary_SetCoveredTasksEmpty(t *testing.T) {
	s := &model.ContextSummary{}
	if err := s.SetCoveredTasks(nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.CoveredTaskIDs != "[]" {
		t.Errorf("empty list should serialize to [], got %q", s.CoveredTaskIDs)
	}
}

func TestContextSummary_IgnoresEmptyIDs(t *testing.T) {
	s := &model.ContextSummary{CoveredTaskIDs: `["a","","b"]`}
	got := s.CoveredTasks()
	if len(got) != 2 {
		t.Errorf("empty id should be skipped, want 2, got %d: %v", len(got), got)
	}
}

// ============================================================
// 压缩素材渲染
// ============================================================

func TestRenderSegmentsForCompaction_LabelsRoles(t *testing.T) {
	segs := []historySegment{{
		TaskID: "t1", Kind: segKindConversation,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "帮我写个脚本"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "write_file"}}},
			{Role: llm.RoleTool, ToolName: "write_file", Content: "ok"},
		},
	}}
	out := renderSegmentsForCompaction(segs)
	for _, want := range []string{"用户：", "助手调用工具：write_file", "工具 write_file 返回："} {
		if !contains(out, want) {
			t.Errorf("rendered text missing %q:\n%s", want, out)
		}
	}
}

func TestRenderSegmentsForCompaction_SkipsEmptyAssistantText(t *testing.T) {
	// 纯 tool_calls 的 assistant 消息没有正文，不该渲染出空的「助手：」行
	segs := []historySegment{{
		TaskID: "t1", Kind: segKindConversation,
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: "   ", ToolCalls: []llm.ToolCall{{Name: "f"}}},
		},
	}}
	out := renderSegmentsForCompaction(segs)
	if contains(out, "助手：") {
		t.Errorf("should not render empty assistant line:\n%s", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// ============================================================
// 阈值与上限的联动（回归：确保压缩目标可达性判定正确）
// ============================================================

func TestThresholds_DegradeConditionForBigToolSchemas(t *testing.T) {
	// 真实场景：128K 窗口的模型 + 几十个 MCP 工具。
	// 若 tools 本身就占掉 40K（>30% 目标线），Target 永远达不成，
	// 必须靠 Ceiling 降级判定放行，否则会一直空转压缩。
	th := contextmgr.NewThresholds(128000)
	blocks := contextBlocks{
		SystemPrompt: 2000, Tools: 40000, UserAndCurrent: 3000,
		Compacted: 5000, Detail: 10000,
	}
	if blocks.Incompressible() < th.Target {
		t.Fatalf("test premise broken: incompressible %d should exceed target %d",
			blocks.Incompressible(), th.Target)
	}
	if blocks.Total() > th.Ceiling {
		t.Fatalf("test premise broken: total %d should be within ceiling %d",
			blocks.Total(), th.Ceiling)
	}
	// 这两个条件同时成立时，压缩循环应判定「降级通过」而不是继续压
}

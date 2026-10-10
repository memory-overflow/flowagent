package actuator

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// ------------------------------------------------------------
// 工作流摘要必须带检索句柄（wf task_id + node_id + 节点 task_id）
// ------------------------------------------------------------

func TestRenderWorkflowSummaryCarriesIDs(t *testing.T) {
	wf := &model.WorkflowOutputData{
		PlanSummary: "排查 deepseek 规划期空返回",
		Nodes: []model.WorkflowNodeData{
			{
				NodeID: "n1",
				Name:   "取证",
				TaskID: "11111111-1111-1111-1111-111111111111",
				Status: model.TaskStatusCompleted,
				Output: &model.AgentOutputData{ReplyText: "结论：网关返回 400"},
			},
			{
				NodeID:    "n2",
				Name:      "审代码",
				TaskID:    "22222222-2222-2222-2222-222222222222",
				Status:    model.TaskStatusRunning,
				DependsOn: []string{"n1"},
			},
			// 尚未调度：没有 task_id，不能编一个出来
			{NodeID: "n3", Name: "写报告", Status: model.TaskStatusQueued},
		},
	}

	got := renderWorkflowSummary(wf, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	for _, want := range []string{
		"工作流任务 ID：aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"[n1] 取证（task_id=11111111-1111-1111-1111-111111111111）",
		"[n2] 审代码（task_id=22222222-2222-2222-2222-222222222222）",
		"[n3] 写报告",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("摘要缺少 %q\n--- 实际 ---\n%s", want, got)
		}
	}
	// 未调度的节点不该出现空 task_id 占位
	if strings.Contains(got, "task_id=）") || strings.Contains(got, "（task_id=）") {
		t.Errorf("未调度节点写出了空 task_id：\n%s", got)
	}
}

// ------------------------------------------------------------
// 索引渲染：条数上限 + 只保留最近 + 计数提示
// ------------------------------------------------------------

func TestRenderTaskIndexCapsToRecent(t *testing.T) {
	entries := make([]model.TaskIndexEntry, 0, 25)
	for i := 0; i < 25; i++ {
		entries = append(entries, model.TaskIndexEntry{
			TaskID: "task-" + string(rune('a'+i)),
			Label:  "第" + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "轮提问",
		})
	}

	got := renderTaskIndex(entries)
	if strings.Contains(got, "task-a") {
		t.Errorf("超出上限时应丢掉最旧的 task-a：\n%s", got)
	}
	if !strings.Contains(got, "task-y") {
		t.Errorf("最近的条目必须保留：\n%s", got)
	}
	if !strings.Contains(got, "更早还有 5 个") {
		t.Errorf("应提示被省略的条数：\n%s", got)
	}
	if gotLine := strings.Count(got, "\n"); gotLine > maxInjectedIndexEntries+2 {
		t.Errorf("注入行数 %d 超过上限 %d", gotLine, maxInjectedIndexEntries+2)
	}

	// 无标签时只出 ID，不写空竖线
	if s := renderTaskIndex([]model.TaskIndexEntry{{TaskID: "x"}}); strings.Contains(s, "｜") {
		t.Errorf("无标签不该输出分隔符：%q", s)
	}
	if renderTaskIndex(nil) != "" {
		t.Errorf("空索引应返回空串")
	}
}

func TestRenderTurnIndexCapsToRecent(t *testing.T) {
	ids := make([]int64, 0, 30)
	for i := 0; i < 30; i++ {
		ids = append(ids, int64(100+i))
	}
	got := renderTurnIndex(ids)
	if strings.Contains(got, "100,") {
		t.Errorf("超出上限时应丢掉最旧的 turn_id：%s", got)
	}
	if !strings.Contains(got, "129") {
		t.Errorf("最近的 turn_id 必须保留：%s", got)
	}
	if !strings.Contains(got, "更早还有 10 个") {
		t.Errorf("应提示被省略的条数：%s", got)
	}
	if renderTurnIndex(nil) != "" {
		t.Errorf("空列表应返回空串")
	}
}

// ------------------------------------------------------------
// turn 记账：去重、跳过无来源的消息、追加不重复
// ------------------------------------------------------------

func TestCollectCompactedTurns(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: "我去查", SourceTurnID: 7},
		{Role: llm.RoleTool, Content: "结果", SourceTurnID: 8},
		{Role: llm.RoleTool, Content: "结果2", SourceTurnID: 8}, // 同一 turn 的第二条消息
		{Role: llm.RoleUser, Content: "追问"},                   // 无来源（非 turn 重建而来）
	}
	got := collectCompactedTurns(msgs)
	if len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Fatalf("collectCompactedTurns = %v, want [7 8]", got)
	}

	if got := collectCompactedTurns(nil); len(got) != 0 {
		t.Errorf("空输入应返回空，got %v", got)
	}
}

func TestAppendTurnIDsDedup(t *testing.T) {
	got := appendTurnIDs([]int64{1, 2, 3}, []int64{3, 4, 0, 4})
	want := []int64{1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("appendTurnIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("appendTurnIDs = %v, want %v", got, want)
		}
	}
}

// ------------------------------------------------------------
// 水位载体：turn 列表与任务索引的存取与归零
// ------------------------------------------------------------

func TestContextSummaryTurnIDsRoundTrip(t *testing.T) {
	s := &model.ContextSummary{ChatID: "c1"}
	s.SetCurrentCompactedTurns([]int64{11, 22})
	got := s.CurrentCompactedTurns()
	if len(got) != 2 || got[1] != 22 {
		t.Fatalf("turn 列表往返失败：%v", got)
	}

	s.ResetCurrent()
	if len(s.CurrentCompactedTurns()) != 0 {
		t.Errorf("ResetCurrent 应清空 turn 列表，got %q", s.CurrentCompactedTurnIDs)
	}
	if s.CurrentCompactedMsgs != 0 || s.CurrentSummary != "" {
		t.Errorf("ResetCurrent 应清干净整个当前水位")
	}
}

func TestAppendCoveredTaskIndex(t *testing.T) {
	s := &model.ContextSummary{ChatID: "c1"}
	s.AppendCoveredTaskIndex(
		model.TaskIndexEntry{TaskID: "t1", Label: "第一轮"},
		model.TaskIndexEntry{TaskID: "t2", Label: "第二轮"},
		// 重复项必须被吃掉
		model.TaskIndexEntry{TaskID: "t1", Label: "第一轮（改）"},
		// 空 ID 不入库
		model.TaskIndexEntry{Label: "没有 ID"},
	)
	got := s.CoveredTaskIndexEntries()
	if len(got) != 2 || got[0].TaskID != "t1" || got[0].Label != "第一轮" {
		t.Fatalf("索引去重/保序失败：%+v", got)
	}

	// 超上限：丢最旧，保留最近
	for i := 0; i < 70; i++ {
		s.AppendCoveredTaskIndex(model.TaskIndexEntry{TaskID: "x" + string(rune('A'+i%26)) + string(rune('a'+i/26))})
	}
	if n := len(s.CoveredTaskIndexEntries()); n > 60 {
		t.Errorf("存储上限失效：%d 条", n)
	}
}

func TestRollOverCurrentAddsIndexEntry(t *testing.T) {
	s := &model.ContextSummary{
		ChatID:               "c1",
		CurrentTaskID:        "old-task",
		CurrentSummary:       "排查 deepseek 400\n第二段内容",
		CurrentSummaryTokens: 30,
		Summary:              "更早的历史",
		SummaryTokens:        10,
	}
	s.SetCurrentCompactedTurns([]int64{5, 6})

	s.RollOverCurrent()

	if !strings.Contains(s.Summary, "排查 deepseek 400") {
		t.Errorf("归位后摘要应并入历史：%q", s.Summary)
	}
	entries := s.CoveredTaskIndexEntries()
	if len(entries) != 1 || entries[0].TaskID != "old-task" {
		t.Fatalf("归位应补一条索引项：%+v", entries)
	}
	if entries[0].Label != "排查 deepseek 400" {
		t.Errorf("标签应取摘要首行，got %q", entries[0].Label)
	}
	if len(s.CurrentCompactedTurns()) != 0 {
		t.Errorf("归位后应清空 turn 列表（整段按 task_id 取回即可）")
	}
}

// ------------------------------------------------------------
// 标签提取
// ------------------------------------------------------------

func TestSegmentLabel(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: "先说一句"},
		{Role: llm.RoleUser, Content: "帮我查一下为什么规划期没输出\n附件：a.txt"},
		{Role: llm.RoleUser, Content: "第二条提问"},
	}
	if got := segmentLabel(msgs); got != "帮我查一下为什么规划期没输出" {
		t.Errorf("segmentLabel = %q", got)
	}

	long := strings.Repeat("问", 100)
	if got := segmentLabel([]llm.Message{{Role: llm.RoleUser, Content: long}}); len([]rune(got)) != 41 {
		t.Errorf("超长标签应截断到 40+省略号，got len=%d", len([]rune(got)))
	}

	if got := segmentLabel([]llm.Message{{Role: llm.RoleAssistant, Content: "x"}}); got != "" {
		t.Errorf("没有 user 消息时应返回空串，got %q", got)
	}
}

// ------------------------------------------------------------
// 端到端：组装出来的消息里必须真的带上这些句柄
// ------------------------------------------------------------

func openIndexTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	d, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "index.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := d.AutoMigrate(&model.Task{}, &model.TaskTemp{}, &model.TaskTurn{},
		&model.ContextSummary{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return d
}

// TestBuildAssembledInjectsIndexes 压缩摘要注入上下文时，旁边必须挂着句柄。
//
// 这是整个 P0 的落点：光把 ID 存进数据库没用，模型看不见就等于没有。
func TestBuildAssembledInjectsIndexes(t *testing.T) {
	db := openIndexTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

	mkTurn := func(taskID string, seq int32, text string, at time.Time) int64 {
		turn := &model.TaskTurn{
			TaskID:    taskID,
			ChatID:    "chat-1",
			TurnSeq:   seq,
			TurnType:  model.TurnTypeFunctionalCall,
			Phase:     int(model.PhaseExecuting),
			Status:    model.TurnStatusSuccess,
			Output:    `{"text":"` + text + `"}`,
			StartTime: at,
			CreatedAt: at,
			UpdatedAt: at,
		}
		if err := db.Create(turn).Error; err != nil {
			t.Fatalf("create turn: %v", err)
		}
		return turn.ID
	}

	// 已被压缩的历史任务（covered，不回放详情）
	mkTurn("task-old", 1, "第一轮回答", base)
	// 当前任务的三轮交互
	id1 := mkTurn("task-cur", 1, "第一步", base.Add(time.Minute))
	id2 := mkTurn("task-cur", 2, "第二步", base.Add(2*time.Minute))
	mkTurn("task-cur", 3, "第三步", base.Add(3*time.Minute))

	prev := funcCallRegistry
	t.Cleanup(func() { funcCallRegistry = prev })
	funcCallRegistry.turnDAO = &dao.TaskTurnDAO{Tx: db}
	funcCallRegistry.taskDAO = &dao.TaskDAO{Tx: db}

	summary := &model.ContextSummary{ChatID: "chat-1"}
	summary.Summary = "历史摘要正文"
	summary.SummaryTokens = 20
	if err := summary.SetCoveredTasks([]string{"task-old"}); err != nil {
		t.Fatalf("set covered: %v", err)
	}
	summary.AppendCoveredTaskIndex(model.TaskIndexEntry{TaskID: "task-old", Label: "第一轮提问"})
	summary.CurrentTaskID = "task-cur"
	summary.CurrentSummary = "当前摘要正文"
	summary.CurrentSummaryTokens = 10
	summary.CurrentCompactedMsgs = 2
	summary.SetCurrentCompactedTurns([]int64{id1, id2})

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-cur", ChatID: "chat-1"}}
	res, err := buildAssembled(ctx, compactInput{
		Temp:         temp,
		SystemPrompt: "sys",
		UserText:     "本轮提问",
	}, contextBlocks{}, summary, contextmgr.GetCounter())
	if err != nil {
		t.Fatalf("buildAssembled: %v", err)
	}

	// 消息来源记账：前两条确实对应前两个 turn
	if got := collectCompactedTurns(res.current[:2]); len(got) != 2 || got[0] != id1 || got[1] != id2 {
		t.Fatalf("current 的 turn 记账 = %v, want [%d %d]", got, id1, id2)
	}

	var hist, cur string
	for _, m := range res.msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		if strings.Contains(m.Content, "历史摘要正文") {
			hist = m.Content
		}
		if strings.Contains(m.Content, "当前摘要正文") {
			cur = m.Content
		}
	}
	if hist == "" || cur == "" {
		t.Fatalf("两份摘要没有都落进消息序列：\n%+v", res.msgs)
	}
	for _, want := range []string{"task_id=task-old", "第一轮提问"} {
		if !strings.Contains(hist, want) {
			t.Errorf("历史摘要缺少 %q：\n%s", want, hist)
		}
	}
	if !strings.Contains(cur, "turn_id") {
		t.Errorf("当前摘要缺少 turn_id 索引：\n%s", cur)
	}
	for _, id := range []int64{id1, id2} {
		if !strings.Contains(cur, strconv.FormatInt(id, 10)) {
			t.Errorf("当前摘要未列出被压掉的 turn %d：\n%s", id, cur)
		}
	}

	// 索引占的 token 必须计入「已压缩」块：它是真金白银的输入，
	// 漏算会让水位偏乐观，下一轮可能直接撑爆。
	if res.blocks.Compacted <= summary.SummaryTokens+summary.CurrentSummaryTokens {
		t.Errorf("索引 token 未计入 Compacted 块：%d vs 摘要 %d+%d",
			res.blocks.Compacted, summary.SummaryTokens, summary.CurrentSummaryTokens)
	}
}

// TestCoveredTaskIndexLegacyFallback 存量摘要没有索引列时，至少要把 ID 给出去。
//
// 索引列上线前压缩过的对话，标签已经随原消息一起没了，但 covered_task_ids
// 还在 —— 句柄比标签重要，退化成「只有 ID」也远好过「什么都没有」，
// 而且免掉一次全量回填。
func TestCoveredTaskIndexLegacyFallback(t *testing.T) {
	legacy := &model.ContextSummary{
		ChatID:         "c1",
		Summary:        "旧摘要",
		CoveredTaskIDs: `["t-b","t-a"]`,
	}
	got := coveredTaskIndex(legacy, legacy.CoveredTasks())
	if len(got) != 2 {
		t.Fatalf("应由 covered_task_ids 兜底出 2 条，got %+v", got)
	}
	// 排序去重随机的 map 遍历顺序，保证每轮注入文本一致
	if got[0].TaskID != "t-a" || got[1].TaskID != "t-b" {
		t.Errorf("兜底索引应排序：%+v", got)
	}

	// 有索引列时以索引列为准（它可能带标签，且顺序是时间序）
	modern := &model.ContextSummary{
		ChatID:         "c1",
		CoveredTaskIDs: `["t-b","t-a"]`,
	}
	modern.AppendCoveredTaskIndex(model.TaskIndexEntry{TaskID: "t-a", Label: "第一轮"})
	if got := coveredTaskIndex(modern, modern.CoveredTasks()); len(got) != 1 || got[0].Label != "第一轮" {
		t.Errorf("有索引列时不该走兜底：%+v", got)
	}
}

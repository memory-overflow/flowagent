package actuator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// ------------------------------------------------------------
// 测试脚手架
// ------------------------------------------------------------

func openRecallTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	d, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "recall.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := d.AutoMigrate(&model.Task{}, &model.TaskTemp{}, &model.TaskTurn{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return d
}

func recallTestDeps(db *gorm.DB) recallDeps {
	return recallDeps{
		taskDAO: &dao.TaskDAO{Tx: db},
		turnDAO: &dao.TaskTurnDAO{Tx: db},
	}
}

// TestResolveRecallOutputBudget 动态预算必须真正随「剩余空间」变化，而不是固定值。
//
// 测试环境无 llmResolver → resolveChatContextLimit 回落 DefaultContextLimit(128000)，
// Trigger = 128000 × 0.8 = 102400。
func TestResolveRecallOutputBudget(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()

	mkLLMTurn := func(taskID string, seq int32, total int) {
		in, _ := json.Marshal(model.TurnLLMInput{
			ContextBlocks: &model.TurnContextBlocks{Total: total},
		})
		now := time.Now()
		db.Create(&model.TaskTurn{
			TaskID: taskID, ChatID: "chat-1", TurnSeq: seq,
			TurnType: model.TurnTypeFunctionalCall, Phase: int(model.PhaseExecuting),
			Status: model.TurnStatusSuccess, Input: string(in),
			StartTime: now, CreatedAt: now, UpdatedAt: now,
		})
	}

	cases := []struct {
		name   string
		used   int
		hasUse bool
		want   int
	}{
		{"无水位回退默认", 0, false, recallMaxOutputTokens},
		// headroom = 102400 - 101000 = 1400 → ×0.7 = 980 → clamp 到 min
		{"剩余不足 clamp 到 min", 101000, true, recallMinOutputTokens},
		// headroom = 102400 - 50000 = 52400 → ×0.7 = 36680
		{"有剩余按比例", 50000, true, 36680},
		// headroom = 102400 - 20000 = 82400 → ×0.7 = 57680，未触顶
		{"大剩余", 20000, true, 57680},
	}
	for _, c := range cases {
		taskID := "task-" + c.name
		if c.hasUse {
			mkLLMTurn(taskID, 1, c.used)
		}
		temp := &model.TaskTemp{Task: model.Task{TaskID: taskID, ChatID: "chat-1"}}
		if got := resolveRecallOutputBudget(ctx, recallTestDeps(db), temp); got != c.want {
			t.Errorf("%s：budget = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestResolveRecallOutputBudgetCeiling 剩余空间极大时不能超过绝对上限。
func TestResolveRecallOutputBudgetCeiling(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()

	taskID := "task-ceiling"
	in, _ := json.Marshal(model.TurnLLMInput{
		ContextBlocks: &model.TurnContextBlocks{Total: 1000},
	})
	now := time.Now()
	db.Create(&model.TaskTurn{
		TaskID: taskID, ChatID: "chat-1", TurnSeq: 1,
		TurnType: model.TurnTypeFunctionalCall, Phase: int(model.PhaseExecuting),
		Status: model.TurnStatusSuccess, Input: string(in),
		StartTime: now, CreatedAt: now, UpdatedAt: now,
	})

	temp := &model.TaskTemp{Task: model.Task{TaskID: taskID, ChatID: "chat-1"}}
	// headroom = 102400 - 1000 = 101400 → ×0.7 = 70980 → clamp 到 64K ceiling
	if got := resolveRecallOutputBudget(ctx, recallTestDeps(db), temp); got != recallMaxOutputCeiling {
		t.Errorf("budget = %d, want ceiling %d", got, recallMaxOutputCeiling)
	}
}

// mkRecallTask 建一个任务记录（取回要靠它校验归属对话）。
func mkRecallTask(t *testing.T, db *gorm.DB, taskID, chatID string) {
	t.Helper()
	now := time.Now()
	task := &model.Task{
		TaskID:    taskID,
		ChatID:    chatID,
		TaskType:  model.TaskTypeConversation,
		Status:    model.TaskStatusCompleted,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
}

// mkRecallTurn 建一条成功的 functional_call turn，输出为 text。
func mkRecallTurn(t *testing.T, db *gorm.DB, taskID, chatID string, seq int32, text string) int64 {
	t.Helper()
	now := time.Now()
	turn := &model.TaskTurn{
		TaskID:    taskID,
		ChatID:    chatID,
		TurnSeq:   seq,
		TurnType:  model.TurnTypeFunctionalCall,
		Phase:     int(model.PhaseExecuting),
		Status:    model.TurnStatusSuccess,
		Output:    `{"text":"` + text + `"}`,
		StartTime: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(turn).Error; err != nil {
		t.Fatalf("create turn: %v", err)
	}
	return turn.ID
}

// ------------------------------------------------------------
// 模式一：turn_id 直取
// ------------------------------------------------------------

// TestRecallByTurnIDReturnsVerbatim turn_id 取回必须给原文，且不调 LLM。
//
// 不调 LLM 是这条路径的核心价值：本测试没有注入任何 LLM client，
// 若实现里偷偷走了模型调用，这里会直接 panic 或报错。
func TestRecallByTurnIDReturnsVerbatim(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()
	resetRecallBudgets()

	mkRecallTask(t, db, "task-a", "chat-1")
	id := mkRecallTurn(t, db, "task-a", "chat-1", 1,
		"exit status 1: connection refused at 127.0.0.1:3306")

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-cur", ChatID: "chat-1"}}
	got, err := recallByTurnID(ctx, recallTestDeps(db), temp, id)
	if err != nil {
		t.Fatalf("recallByTurnID: %v", err)
	}
	for _, want := range []string{
		"connection refused at 127.0.0.1:3306", // 原始细节必须一字不改
		"turn_id=",
		"task_id=task-a",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("取回结果缺少 %q：\n%s", want, got)
		}
	}
}

// TestRecallByTurnIDRejectsCrossChat 跨对话取回必须拒绝。
//
// 这是最重要的一条护栏：turn_id 是自增整数，模型随手猜一个就可能命中
// 别人的对话。少了它就是越权读取。
func TestRecallByTurnIDRejectsCrossChat(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()
	resetRecallBudgets()

	mkRecallTask(t, db, "task-other", "chat-OTHER")
	id := mkRecallTurn(t, db, "task-other", "chat-OTHER", 1, "别人对话里的机密内容")

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-cur", ChatID: "chat-1"}}
	got, err := recallByTurnID(ctx, recallTestDeps(db), temp, id)
	if err == nil {
		t.Fatalf("跨对话取回本该报错，却返回了：%s", got)
	}
	if strings.Contains(got, "机密") {
		t.Fatalf("越权泄漏了别人对话的内容：%s", got)
	}
	if !strings.Contains(err.Error(), "不属于当前对话") {
		t.Errorf("错误信息应说明原因，实际：%v", err)
	}
}

// TestRecallByTurnIDMissingTurnDegrades turn 不存在时要给出可行动的提示，
// 而不是报错或返回空。
//
// turn 会因任务重试被清理，这是正常情况。返回 error 会让模型以为是自己
// 参数写错了从而反复重试；返回空则让它以为「那轮什么都没发生」。
func TestRecallByTurnIDMissingTurnDegrades(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()
	resetRecallBudgets()

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-cur", ChatID: "chat-1"}}
	got, err := recallByTurnID(ctx, recallTestDeps(db), temp, 99999)
	if err != nil {
		t.Fatalf("turn 缺失不该报错: %v", err)
	}
	for _, want := range []string{"未找到", "task_id"} {
		if !strings.Contains(got, want) {
			t.Errorf("降级提示缺少 %q：\n%s", want, got)
		}
	}
}

// ------------------------------------------------------------
// 模式二：task_id 取回
// ------------------------------------------------------------

// TestRecallByTaskIDShortReturnsVerbatim 记录放得下时必须给原文，不调 LLM。
//
// 这是有意的优化：原文比 LLM 转述忠实，且省一次调用。同样靠「没注入
// client」来反证没走模型。
func TestRecallByTaskIDShortReturnsVerbatim(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()
	resetRecallBudgets()

	mkRecallTask(t, db, "task-a", "chat-1")
	mkRecallTurn(t, db, "task-a", "chat-1", 1, "第一步：读取配置 db_config.json")
	mkRecallTurn(t, db, "task-a", "chat-1", 2, "第二步：发现 port=27250")

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-cur", ChatID: "chat-1"}}
	got, err := recallByTaskID(ctx, recallTestDeps(db), temp, 0, "task-a", "")
	if err != nil {
		t.Fatalf("recallByTaskID: %v", err)
	}
	for _, want := range []string{
		"未经压缩转述", // 必须标明这是原文而非摘要
		"db_config.json",
		"port=27250",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("取回结果缺少 %q：\n%s", want, got)
		}
	}
}

// TestRecallByTaskIDRejectsCrossChat 跨对话按 task_id 取回同样要拒绝。
func TestRecallByTaskIDRejectsCrossChat(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()
	resetRecallBudgets()

	mkRecallTask(t, db, "task-other", "chat-OTHER")
	mkRecallTurn(t, db, "task-other", "chat-OTHER", 1, "别人的机密内容")

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-cur", ChatID: "chat-1"}}
	got, err := recallByTaskID(ctx, recallTestDeps(db), temp, 0, "task-other", "查什么")
	if err == nil {
		t.Fatalf("跨对话取回本该报错，却返回了：%s", got)
	}
	if strings.Contains(got, "机密") {
		t.Fatalf("越权泄漏了别人对话的内容：%s", got)
	}
}

// TestRecallByTaskIDMissingTaskDegrades 任务不存在时给提示而非报错。
func TestRecallByTaskIDMissingTaskDegrades(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()
	resetRecallBudgets()

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-cur", ChatID: "chat-1"}}
	got, err := recallByTaskID(ctx, recallTestDeps(db), temp, 0, "task-nope", "查什么")
	if err != nil {
		t.Fatalf("任务缺失不该报错: %v", err)
	}
	if !strings.Contains(got, "未找到") {
		t.Errorf("降级提示缺少「未找到」：\n%s", got)
	}
}

// TestRecallByTaskIDEmptyHistoryDegrades 任务存在但没有可取回的交互。
func TestRecallByTaskIDEmptyHistoryDegrades(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()
	resetRecallBudgets()

	mkRecallTask(t, db, "task-empty", "chat-1")

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-cur", ChatID: "chat-1"}}
	got, err := recallByTaskID(ctx, recallTestDeps(db), temp, 0, "task-empty", "")
	if err != nil {
		t.Fatalf("空任务不该报错: %v", err)
	}
	if !strings.Contains(got, "没有留下可取回的交互记录") {
		t.Errorf("降级提示不符预期：\n%s", got)
	}
}

// ------------------------------------------------------------
// 配额
// ------------------------------------------------------------

// TestRecallQuotaExhaustsWithGuidance 配额用尽后返回引导话术而非报错。
//
// 返回 error 会让模型把它当工具故障原地重试；返回引导文本它才会换路径。
func TestRecallQuotaExhaustsWithGuidance(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()
	resetRecallBudgets()
	t.Cleanup(resetRecallBudgets)

	mkRecallTask(t, db, "task-a", "chat-1")
	id := mkRecallTurn(t, db, "task-a", "chat-1", 1, "原始内容")

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-quota", ChatID: "chat-1"}}
	deps := recallTestDeps(db)
	req := workspacepkg.ContextRecallRequest{TurnID: id}

	// 前 recallQuotaPerTask 次必须放行
	for i := 1; i <= recallQuotaPerTask; i++ {
		got, err := runContextRecall(ctx, deps, temp, 0, req)
		if err != nil {
			t.Fatalf("第 %d 次取回本该放行: %v", i, err)
		}
		if !strings.Contains(got, "原始内容") {
			t.Fatalf("第 %d 次取回没拿到原文：%s", i, got)
		}
	}

	// 第 N+1 次应被拦下，且不是 error
	got, err := runContextRecall(ctx, deps, temp, 0, req)
	if err != nil {
		t.Fatalf("配额用尽应返回引导文本而非 error: %v", err)
	}
	if !strings.Contains(got, "配额已用尽") {
		t.Errorf("未返回配额话术：%s", got)
	}
	if strings.Contains(got, "原始内容") {
		t.Errorf("配额用尽后仍返回了原文：%s", got)
	}
}

// TestRecallQuotaIsPerTask 配额按任务隔离，别的任务用满不影响本任务。
func TestRecallQuotaIsPerTask(t *testing.T) {
	resetRecallBudgets()
	t.Cleanup(resetRecallBudgets)

	for i := 0; i < recallQuotaPerTask+3; i++ {
		takeRecallQuota("task-hungry")
	}
	if _, allowed := takeRecallQuota("task-fresh"); !allowed {
		t.Error("另一个任务的配额被串用了")
	}
}

// ------------------------------------------------------------
// 路径决策
// ------------------------------------------------------------

// TestPlanRecall 钉死「什么情况走哪条路」。
//
// 提取路径要真调 LLM、单测里跑不了，但决策本身最容易出错 ——
// 早先的实现在「放得下」时直接忽略 query 给全文，实测导致模型明确说了
// 要查什么，却拿回 9 条交互的全文（前两轮 list_dir 的目录清单纯属噪声）。
func TestPlanRecall(t *testing.T) {
	const budget = 8000
	cases := []struct {
		name  string
		total int
		query string
		want  recallPlan
	}{
		// 极小的记录：提取的 LLM 往返不划算，一律给全文
		{"极小无 query", 100, "", recallPlanVerbatim},
		{"极小有 query", 100, "查报错", recallPlanVerbatim},
		{"正好在门槛上", recallVerbatimFloor, "查报错", recallPlanVerbatim},

		// 中等长度：给了 query 就按 query 提取，这是本次修正的核心
		{"中等有 query", recallVerbatimFloor + 1, "查报错", recallPlanExtract},
		{"中等无 query", recallVerbatimFloor + 1, "", recallPlanVerbatim},
		{"接近上限无 query", budget, "", recallPlanVerbatim},
		{"接近上限有 query", budget, "查报错", recallPlanExtract},

		// 超上限：没 query 无法处理（不能静默截断，要找的那段可能刚好被切掉）
		{"超上限无 query", budget + 1, "", recallPlanNeedQuery},
		{"超上限有 query", budget + 1, "查报错", recallPlanExtract},

		// 只有空白的 query 等同于没给
		{"query 全是空白", budget + 1, "   \n\t", recallPlanNeedQuery},
	}
	for _, c := range cases {
		if got := planRecall(c.total, c.query, budget); got != c.want {
			t.Errorf("%s：planRecall(%d, %q, %d) = %v, want %v",
				c.name, c.total, c.query, budget, got, c.want)
		}
	}
}

// TestPlanRecallHonorsDynamicBudget 动态预算要真正参与决策：
// 预算变大，原本「超限要补 query」的记录就变成「放得下直接给全文」。
func TestPlanRecallHonorsDynamicBudget(t *testing.T) {
	total := 10000
	// 小预算下超限
	if got := planRecall(total, "", 8000); got != recallPlanNeedQuery {
		t.Errorf("budget=8000 时 total=10000 应判 need-query，got %v", got)
	}
	// 大预算下放得下
	if got := planRecall(total, "", 64000); got != recallPlanVerbatim {
		t.Errorf("budget=64000 时 total=10000 应判 verbatim，got %v", got)
	}
}

// TestRecallByTaskIDWithQueryDoesNotReturnVerbatim 给了 query 时不能再走全文路径。
//
// 走到提取路径时会因为没有 LLM client 而报错 —— 这里正是拿这个错误当
// 「确实走了提取」的证据：只要返回的不是「未经压缩转述」的全文即可。
func TestRecallByTaskIDWithQueryDoesNotReturnVerbatim(t *testing.T) {
	db := openRecallTestDB(t)
	ctx := context.Background()
	resetRecallBudgets()

	mkRecallTask(t, db, "task-big", "chat-1")
	// 堆到超过 recallVerbatimFloor，确保不落进「极小」分支
	for i := 0; i < 40; i++ {
		mkRecallTurn(t, db, "task-big", "chat-1", int32(i+1),
			strings.Repeat("这是一段足够长的历史记录内容用来撑过门槛。", 12))
	}

	temp := &model.TaskTemp{Task: model.Task{TaskID: "task-cur", ChatID: "chat-1"}}
	got, _ := recallByTaskID(ctx, recallTestDeps(db), temp, 0, "task-big", "我要查什么")
	if strings.Contains(got, "未经压缩转述") {
		t.Errorf("给了 query 却仍返回全文：\n%s", got[:min(300, len(got))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ------------------------------------------------------------
// 工具参数校验（在 workspace 包的 handler 层）
// ------------------------------------------------------------

// TestRecallContextToolArgValidation turn_id 与 task_id 必须恰好给一个。
//
// 都不给无法定位目标；都给则意图不明（取一轮还是取整段）。两种都直接
// 报错让模型重选，不替它猜 —— 猜错会静默返回错误的历史段。
func TestRecallContextToolArgValidation(t *testing.T) {
	var called bool
	wc := &workspacepkg.Context{
		ContextRecaller: func(ctx context.Context, req workspacepkg.ContextRecallRequest) (string, error) {
			called = true
			return "ok", nil
		},
	}
	tool := findBuiltinTool(t, model.RecallContextToolName)
	ctx := context.Background()

	cases := []struct {
		name    string
		args    map[string]interface{}
		wantErr bool
	}{
		{"都不给", map[string]interface{}{}, true},
		{"都给", map[string]interface{}{"turn_id": 12, "task_id": "t1"}, true},
		{"只给 turn_id", map[string]interface{}{"turn_id": 12}, false},
		{"只给 task_id", map[string]interface{}{"task_id": "t1"}, false},
		// turn_id=0 视为未提供：JSON 里省略字段与显式 0 应当同义
		{"turn_id=0 且给 task_id", map[string]interface{}{"turn_id": 0, "task_id": "t1"}, false},
	}
	for _, c := range cases {
		called = false
		_, err := tool.Handler(ctx, wc, c.args)
		if c.wantErr && err == nil {
			t.Errorf("%s：本该报错却通过了", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s：本该通过却报错 %v", c.name, err)
		}
		if !c.wantErr && !called {
			t.Errorf("%s：没有转发到取回执行器", c.name)
		}
	}
}

// TestRecallContextToolWithoutRecaller 未注入执行器时必须明确报错，
// 不能静默返回空（否则模型以为「历史里真的什么都没有」）。
func TestRecallContextToolWithoutRecaller(t *testing.T) {
	tool := findBuiltinTool(t, model.RecallContextToolName)
	_, err := tool.Handler(context.Background(), &workspacepkg.Context{},
		map[string]interface{}{"turn_id": 1})
	if err == nil {
		t.Fatal("未注入取回执行器时本该报错")
	}
}

func findBuiltinTool(t *testing.T, name string) *workspacepkg.ToolDef {
	t.Helper()
	for _, d := range workspacepkg.BuiltinTools() {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("内置工具 %s 未注册", name)
	return nil
}

// ------------------------------------------------------------
// 索引文案必须告知取回方式
// ------------------------------------------------------------

// TestIndexTextTellsHowToRecall 光给 ID 不说用途，模型不会想到能回取 ——
// 那段索引就成了纯占位符，白占 token。
//
// 同时校验文案里的工具名与真实注册名一致：写死字符串一旦与 BuiltinTools
// 里的名字漂移，模型会照文案调用一个不存在的工具。
func TestIndexTextTellsHowToRecall(t *testing.T) {
	taskIdx := renderTaskIndex([]model.TaskIndexEntry{
		{TaskID: "t-1", Label: "排查 deepseek 空返回"},
	})
	turnIdx := renderTurnIndex([]int64{260, 261})
	wfSummary := renderWorkflowSummary(&model.WorkflowOutputData{
		PlanSummary: "目标",
		Nodes: []model.WorkflowNodeData{
			{NodeID: "n1", Name: "取证", TaskID: "wt-1", Status: model.TaskStatusCompleted},
		},
	}, "wf-1")

	for _, c := range []struct{ name, text string }{
		{"历史任务索引", taskIdx},
		{"turn 索引", turnIdx},
		{"工作流摘要", wfSummary},
	} {
		if !strings.Contains(c.text, model.RecallContextToolName) {
			t.Errorf("%s 没告诉模型用什么工具取回：\n%s", c.name, c.text)
		}
	}
	// 真实注册名校验：文案提到的工具必须真的存在
	findBuiltinTool(t, model.RecallContextToolName)
}

// ------------------------------------------------------------
// 工具集边界
// ------------------------------------------------------------

// TestDeepSearchSubTaskHidesRecallContext deep_search 子任务不该看到 recall_context。
//
// 它是 Isolated 的，压根看不到主对话的历史摘要与那些句柄；而它自己的历史
// 全是抓回来的网页正文，一旦取回就把巨量原文搬回上下文，正好违背「用子任务
// 隔离脏上下文」的设计意图。
func TestDeepSearchSubTaskHidesRecallContext(t *testing.T) {
	entries := []ToolEntry{
		{ExposedName: model.DeepSearchToolName, Kind: toolKindBuiltin},
		{ExposedName: model.RecallContextToolName, Kind: toolKindBuiltin},
		{ExposedName: "read_file", Kind: toolKindBuiltin},
	}
	got := excludeTool(newToolSet(entries),
		model.DeepSearchToolName, model.RecallContextToolName)

	for _, e := range got.tools {
		if e.ExposedName == model.RecallContextToolName {
			t.Error("deep_search 子任务的工具集里仍有 recall_context")
		}
		if e.ExposedName == model.DeepSearchToolName {
			t.Error("deep_search 子任务的工具集里仍有 deep_search")
		}
	}
	// 其他工具不能被误删
	var hasRead bool
	for _, e := range got.tools {
		if e.ExposedName == "read_file" {
			hasRead = true
		}
	}
	if !hasRead {
		t.Error("read_file 被误删了")
	}
}

// ------------------------------------------------------------
// turn 类型语义
// ------------------------------------------------------------

// TestContextRecallTurnTypeSemantics 取回 turn 不能算 agent 循环里的 LLM 调用。
//
// IsLLMCall 是「这一轮模型调过了没」的判据，被 functionalCallIdempotentSkip
// 用来做幂等跳过。取回发生在工具执行阶段，算进去会让下一轮 functional_call
// 被误判为「已执行」而整轮吞掉。
func TestContextRecallTurnTypeSemantics(t *testing.T) {
	if model.TurnTypeContextRecall.IsLLMCall() {
		t.Error("context_recall 不该算 IsLLMCall（会破坏 functional_call 的幂等跳过）")
	}
	if got := model.TurnTypeContextRecall.String(); got != "context_recall" {
		t.Errorf("String() = %q, want context_recall", got)
	}
	// 与压缩 turn 不能撞值，否则调试页会把两类内部调用混在一起
	if model.TurnTypeContextRecall == model.TurnTypeContextCompact {
		t.Error("context_recall 与 context_compact 撞了取值")
	}
}

// TestContextRecallTurnNotReplayedIntoHistory 取回 turn 不该回放进对话历史。
//
// 它是工具执行的内部动作。若被回放，模型会在历史里看到自己「凭空调用了
// 一次不存在于 tool_calls 里的东西」，且取回的原文会被永久固化进上下文 ——
// 压缩省下来的空间又被吃回去。
func TestContextRecallTurnNotReplayedIntoHistory(t *testing.T) {
	turn := &model.TaskTurn{
		TaskID:   "task-a",
		ChatID:   "chat-1",
		TurnType: model.TurnTypeContextRecall,
		Status:   model.TurnStatusSuccess,
		Output:   `{"text":"取回的原文"}`,
	}
	if _, ok := turnToMessage(turn); ok {
		t.Error("context_recall turn 被回放进了对话历史")
	}
}

// ------------------------------------------------------------
// 工作流二层下钻
// ------------------------------------------------------------

// TestRecallWorkflowNodesListsNodes 工作流 task 下钻必须列出节点清单，
// 每个节点带 task_id 供逐节点取回，归档节点跳过、未调度节点标注。
func TestRecallWorkflowNodesListsNodes(t *testing.T) {
	dag := model.WorkflowOutputData{
		PlanSummary: "调研开源大模型并出报告",
		Nodes: []model.WorkflowNodeData{
			{
				NodeID: "n1_search", Name: "检索", TaskID: "node-task-1",
				Status: model.TaskStatusCompleted,
				Output: &model.AgentOutputData{ReplyText: "检索到 5 个候选模型"},
			},
			{
				NodeID: "n2_report", Name: "写报告", TaskID: "node-task-2",
				Status: model.TaskStatusCompleted,
				Output: &model.AgentOutputData{ReplyText: "报告已生成 outputs/report.md"},
			},
			// 未调度节点：没有 task_id，不能编一个
			{NodeID: "n3_verify", Name: "校验", Status: model.TaskStatusQueued},
			// 归档节点：跳过
			{NodeID: "n4_old", Name: "废弃", TaskID: "node-task-4", Status: model.TaskStatusFailed, Archived: true},
		},
	}
	out, _ := json.Marshal(dag)
	target := &model.Task{
		TaskID: "wf-task-1", ChatID: "chat-1",
		TaskType: model.TaskTypeWorkflow, TaskOutput: string(out),
	}

	got, err := recallWorkflowNodes(context.Background(),
		&model.TaskTemp{Task: model.Task{TaskID: "cur", ChatID: "chat-1"}}, target, "")
	if err != nil {
		t.Fatalf("recallWorkflowNodes: %v", err)
	}

	for _, want := range []string{
		"wf-task-1",
		"调研开源大模型并出报告",
		"[n1_search] 检索",
		"task_id=node-task-1",
		"[n2_report] 写报告",
		"task_id=node-task-2",
		"[n3_verify] 校验",
		"未调度，无 task_id",
		"recall_context",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("节点清单缺少 %q：\n%s", want, got)
		}
	}
	// 归档节点不该出现
	if strings.Contains(got, "n4_old") || strings.Contains(got, "node-task-4") {
		t.Errorf("归档节点不该出现在清单里：\n%s", got)
	}
}

// TestRecallWorkflowNodesBrokenDAG DAG 解析失败要明确报错，不能静默返回空。
func TestRecallWorkflowNodesBrokenDAG(t *testing.T) {
	target := &model.Task{
		TaskID: "wf-task-broken", ChatID: "chat-1",
		TaskType: model.TaskTypeWorkflow, TaskOutput: "not-json",
	}
	_, err := recallWorkflowNodes(context.Background(),
		&model.TaskTemp{Task: model.Task{TaskID: "cur", ChatID: "chat-1"}}, target, "")
	if err == nil {
		t.Fatal("DAG 解析失败本该报错")
	}
}

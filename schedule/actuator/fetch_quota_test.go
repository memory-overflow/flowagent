package actuator

import (
	"strings"
	"sync"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	mcppkg "github.com/memory-overflow/flowagent/pkg/mcp"
)

func TestTakeFetchQuota_ExhaustsAfterLimit(t *testing.T) {
	resetFetchBudgets()
	defer resetFetchBudgets()

	for i := 1; i <= fetchQuotaPerTask; i++ {
		used, allowed := takeFetchQuota("task-a")
		if !allowed {
			t.Fatalf("call #%d must be allowed (used=%d)", i, used)
		}
		if used != i {
			t.Fatalf("used = %d, want %d", used, i)
		}
	}
	if used, allowed := takeFetchQuota("task-a"); allowed {
		t.Fatalf("call #%d must be rejected (used=%d)", fetchQuotaPerTask+1, used)
	}
}

func TestTakeFetchQuota_PerTaskIsolated(t *testing.T) {
	resetFetchBudgets()
	defer resetFetchBudgets()

	for i := 0; i < fetchQuotaPerTask; i++ {
		takeFetchQuota("task-a")
	}
	// 另一个任务的配额独立：不能因为 A 用尽就误伤 B
	if _, allowed := takeFetchQuota("task-b"); !allowed {
		t.Fatal("quota must be per-task: task-b should still be allowed")
	}
}

func TestTakeFetchQuota_EmptyTaskIDBypasses(t *testing.T) {
	// 缺任务 ID 时退化为放行：配额是护栏，不是开关
	for i := 0; i < fetchQuotaPerTask*3; i++ {
		if _, allowed := takeFetchQuota(""); !allowed {
			t.Fatalf("empty task id must bypass quota (call #%d)", i+1)
		}
	}
}

// TestTakeFetchQuota_Concurrent 一次 LLM 响应里的多个 tool_call 是并行执行的，
// 配额必须精确计数：既不能重复放行，也不能 data race（-race 下会直接失败）。
func TestTakeFetchQuota_Concurrent(t *testing.T) {
	resetFetchBudgets()
	defer resetFetchBudgets()

	const goroutines = 32
	var wg sync.WaitGroup
	var allowed int64
	var mu sync.Mutex

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, ok := takeFetchQuota("task-concurrent"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != fetchQuotaPerTask {
		t.Fatalf("allowed = %d, want exactly %d", allowed, fetchQuotaPerTask)
	}
}

func TestClampFetchMaxLength(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]interface{}
		want interface{}
	}{
		// 压回后统一写成 int：JSON 序列化后与 float64 等价，MCP 侧无差别
		{"超上限被压回", map[string]interface{}{"max_length": float64(500000)}, fetchMaxLength},
		{"在上限内不动", map[string]interface{}{"max_length": float64(3000)}, float64(3000)},
		{"int 型也认", map[string]interface{}{"max_length": 99999}, fetchMaxLength},
	}
	for _, c := range cases {
		clampFetchMaxLength(c.in)
		got := c.in["max_length"]
		if got != c.want {
			t.Errorf("%s: max_length = %v (%T), want %v", c.name, got, got, c.want)
		}
	}

	// 未传 max_length 时不替模型填值，交给 MCP server 自己的默认
	args := map[string]interface{}{"url": "https://example.com"}
	clampFetchMaxLength(args)
	if _, ok := args["max_length"]; ok {
		t.Error("max_length must stay absent when the model did not pass one")
	}
	// nil map 不能 panic
	clampFetchMaxLength(nil)
}

// TestFetchLengthLimitFor_DeepSearchGetsWiderLimit deep_search 子任务靠多来源
// 交叉出结论，单页上限要比主 agent 宽，但仍必须有限（实测无限长会返回 201KB）。
func TestFetchLengthLimitFor_DeepSearchGetsWiderLimit(t *testing.T) {
	sub := &model.TaskTemp{}
	sub.TaskID = "sub"
	sub.TaskType = model.TaskTypeWorkflowSubTask
	sub.TaskInput.Data = &model.TaskInputData{SubTaskKind: model.SubTaskKindDeepSearch}

	if got := fetchLengthLimitFor(sub); got != fetchMaxLengthDeepSearch {
		t.Errorf("deep_search limit = %d, want %d", got, fetchMaxLengthDeepSearch)
	}
	main := &model.TaskTemp{}
	main.TaskID = "main"
	main.TaskType = model.TaskTypeConversation
	if got := fetchLengthLimitFor(main); got != fetchMaxLength {
		t.Errorf("main agent limit = %d, want %d", got, fetchMaxLength)
	}
	if fetchMaxLengthDeepSearch <= fetchMaxLength {
		t.Fatalf("deep_search limit (%d) must be wider than main agent (%d)",
			fetchMaxLengthDeepSearch, fetchMaxLength)
	}
}

func TestClampFetchMaxLengthTo_DeepSearch(t *testing.T) {
	args := map[string]interface{}{"max_length": float64(500000)}
	clampFetchMaxLengthTo(args, fetchMaxLengthDeepSearch)
	if got := args["max_length"]; got != fetchMaxLengthDeepSearch {
		t.Errorf("max_length = %v, want %d", got, fetchMaxLengthDeepSearch)
	}
	// 上限内不动
	args = map[string]interface{}{"max_length": float64(3000)}
	clampFetchMaxLengthTo(args, fetchMaxLengthDeepSearch)
	if got := args["max_length"]; got != float64(3000) {
		t.Errorf("max_length = %v, want untouched 3000", got)
	}
}

// TestCapFetchedContent 参数 clamp 对不守约的 server 无效，内容级限长是兜底：
// 单次抓取进上下文/进库的上限必须由我们确定。
func TestCapFetchedContent(t *testing.T) {
	const limit = 1000
	long := strings.Repeat("正", 5000)
	got := capFetchedContent(long, limit)
	runes := []rune(got)
	if len(runes) > limit+40 { // 允许省略标记的额外长度
		t.Fatalf("capped length = %d, want <= %d", len(runes), limit+40)
	}
	if !strings.Contains(got, "已省略") {
		t.Error("capped content must say how much was dropped")
	}

	// 未超限原样返回
	short := "短短的正文"
	if got := capFetchedContent(short, limit); got != short {
		t.Error("content within limit must be returned untouched")
	}
	// limit <= 0 视为不限
	if got := capFetchedContent(long, 0); got != long {
		t.Error("non-positive limit must mean no cap")
	}
}

func TestIsBuiltinFetchEntry(t *testing.T) {
	builtin := &ToolEntry{Kind: toolKindMCP, MCP: &mcppkg.RegisteredTool{
		OriginalName: mcpFetchToolName, Server: &mcppkg.ServerConfig{ServerName: "fetch"},
	}}
	if !isBuiltinFetchEntry(builtin) {
		t.Error("framework builtin fetch must be detected")
	}
	userBound := &ToolEntry{Kind: toolKindMCP, MCP: &mcppkg.RegisteredTool{
		OriginalName: mcpFetchToolName,
		Server:       &mcppkg.ServerConfig{ServerName: "fetch", ServerID: "mcp-001"},
	}}
	if isBuiltinFetchEntry(userBound) {
		t.Error("user-bound MCP must not be treated as framework builtin")
	}
	if isBuiltinFetchEntry(nil) {
		t.Error("nil entry must not panic and must be false")
	}
}

func TestIsTopLevelFetch(t *testing.T) {
	temp := &model.TaskTemp{}
	temp.TaskID = "t1"
	temp.TaskType = model.TaskTypeConversation

	// 框架内置 fetch（ServerID 为空）：命中
	builtin := &ToolEntry{Kind: toolKindMCP, MCP: &mcppkg.RegisteredTool{
		OriginalName: mcpFetchToolName, Server: &mcppkg.ServerConfig{ServerName: "fetch"},
	}}
	if !isTopLevelFetch(builtin, temp) {
		t.Error("framework builtin fetch must be quota-gated")
	}

	// 用户自己绑定的同名 MCP（有 DB 主键）：不拦 —— 那是用户显式接入的能力
	userBound := &ToolEntry{Kind: toolKindMCP, MCP: &mcppkg.RegisteredTool{
		OriginalName: mcpFetchToolName,
		Server:       &mcppkg.ServerConfig{ServerName: "fetch", ServerID: "mcp-001"},
	}}
	if isTopLevelFetch(userBound, temp) {
		t.Error("user-bound MCP must not be gated by the framework quota")
	}

	// 非 fetch 工具：不拦
	other := &ToolEntry{Kind: toolKindMCP, MCP: &mcppkg.RegisteredTool{
		OriginalName: "search", Server: &mcppkg.ServerConfig{ServerName: "fetch"},
	}}
	if isTopLevelFetch(other, temp) {
		t.Error("non-fetch tool must not be gated")
	}

	// 内置 native 工具：不拦
	if isTopLevelFetch(&ToolEntry{Kind: toolKindBuiltin}, temp) {
		t.Error("builtin tool must not be gated")
	}

	// deep_search 子任务内的 fetch：不拦（它的多轮抓取是设计意图）
	sub := &model.TaskTemp{}
	sub.TaskID = "sub"
	sub.TaskType = model.TaskTypeWorkflowSubTask
	sub.TaskInput.Data = &model.TaskInputData{SubTaskKind: model.SubTaskKindDeepSearch}
	if isTopLevelFetch(builtin, sub) {
		t.Error("deep_search subtask fetch must not be gated")
	}
}

func TestInjectTool_DedupesByName(t *testing.T) {
	e := ToolEntry{ExposedName: "fetch__fetch"}
	ts := injectTool(nil, e)
	ts = injectTool(ts, ToolEntry{ExposedName: "fetch__fetch"}) // 同名重复注入
	if len(ts.tools) != 1 {
		t.Fatalf("tools = %d, want 1 (duplicate must be skipped)", len(ts.tools))
	}
}

// TestInjectTools_KeepsDeepSearchSubTaskSingleFetch 主 agent 已带 fetch，
// 子任务再注入同一批框架工具时不能变成两个 fetch。
func TestInjectTools_KeepsDeepSearchSubTaskSingleFetch(t *testing.T) {
	base := newToolSet([]ToolEntry{
		{ExposedName: "run_command"},
		{ExposedName: "fetch__fetch"},
	})
	out := injectTools(base, []ToolEntry{
		{ExposedName: "fetch__fetch"},
		{ExposedName: "web_search"},
	})
	if len(out.tools) != 3 {
		t.Fatalf("tools = %d, want 3 (run_command + fetch + web_search)", len(out.tools))
	}
}

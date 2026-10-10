package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	mcppkg "github.com/memory-overflow/flowagent/pkg/mcp"
	skillpkg "github.com/memory-overflow/flowagent/pkg/skill"
)

// newTestToolSet 构造测试用工具集（MCP 工具）
func newTestToolSet(names ...string) *ToolSet {
	entries := make([]ToolEntry, 0, len(names))
	for _, n := range names {
		entries = append(entries, ToolEntry{
			ExposedName: n,
			Description: "test tool " + n,
			InputSchema: map[string]interface{}{"type": "object"},
			Kind:        toolKindMCP,
			MCP: &mcppkg.RegisteredTool{
				ExposedName:  n,
				OriginalName: strings.TrimPrefix(n, "srv__"),
				Description:  "test tool " + n,
				InputSchema:  map[string]interface{}{"type": "object"},
				Server:       &mcppkg.ServerConfig{ServerID: "s1", ServerName: "srv"},
			},
		})
	}
	return newToolSet(entries)
}

// TestToolSetLookup 验证工具反查
func TestToolSetLookup(t *testing.T) {
	ts := newTestToolSet("srv__read", "srv__write")

	entry, ok := ts.Lookup("srv__read")
	if !ok {
		t.Fatal("expected to find srv__read")
	}
	if entry.Kind != toolKindMCP {
		t.Errorf("kind = %v, want mcp", entry.Kind)
	}
	if entry.MCP.OriginalName != "read" {
		t.Errorf("original name = %q, want read", entry.MCP.OriginalName)
	}
	if entry.MCP.Server.ServerName != "srv" {
		t.Errorf("server name = %q, want srv", entry.MCP.Server.ServerName)
	}

	if _, ok := ts.Lookup("nonexistent"); ok {
		t.Error("expected lookup miss for unknown tool")
	}
}

// TestToolSetSkillEntry 验证 command 型 skill 作为工具暴露时，
// 名字走 skill__<name> 命名空间，Lookup 返回 Kind=toolKindSkill。
func TestToolSetSkillEntry(t *testing.T) {
	cfg := &skillpkg.Config{
		Name:        "text_stats",
		Description: "统计文本的字符/词/行数",
		Type:        model.SkillTypeCommand,
		Command:     "python3",
	}

	ts := newToolSet([]ToolEntry{{
		ExposedName: cfg.ToolName(),
		Description: cfg.Description,
		InputSchema: cfg.ToolSchema(),
		Kind:        toolKindSkill,
		Skill:       cfg,
	}})

	if got, want := cfg.ToolName(), "skill__text_stats"; got != want {
		t.Errorf("tool name = %q, want %q", got, want)
	}
	entry, ok := ts.Lookup(cfg.ToolName())
	if !ok {
		t.Fatalf("expected to find %q", cfg.ToolName())
	}
	if entry.Kind != toolKindSkill {
		t.Errorf("kind = %v, want skill", entry.Kind)
	}
	if entry.Skill == nil || entry.Skill.Command != "python3" {
		t.Errorf("skill entry not preserved: %+v", entry.Skill)
	}
}

// TestToolSetLookup_NilSafe nil ToolSet 上的操作必须安全，
// 因为工具加载失败时会降级为 nil
func TestToolSetLookup_NilSafe(t *testing.T) {
	var ts *ToolSet
	if _, ok := ts.Lookup("any"); ok {
		t.Error("nil toolset should not find anything")
	}
	if got := ts.LLMTools(); got != nil {
		t.Errorf("nil toolset LLMTools() = %v, want nil", got)
	}
	if got := ts.Len(); got != 0 {
		t.Errorf("nil toolset Len() = %d, want 0", got)
	}
	if got := ts.Names(); got != nil {
		t.Errorf("nil toolset Names() = %v, want nil", got)
	}
}

// TestToolSetLLMTools 验证转换为 LLM schema
func TestToolSetLLMTools(t *testing.T) {
	ts := newTestToolSet("srv__read", "srv__write")
	tools := ts.LLMTools()

	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(tools))
	}
	// 暴露给 LLM 的名字必须是 ExposedName，否则 tool_call 阶段反查不到
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
		if tl.Parameters == nil {
			t.Errorf("tool %s has nil parameters", tl.Name)
		}
	}
	if !names["srv__read"] || !names["srv__write"] {
		t.Errorf("unexpected tool names: %v", names)
	}
}

// TestInjectSummaryParam 验证 summary 必填参数被注入到 schema，且不污染原始 schema。
func TestInjectSummaryParam(t *testing.T) {
	orig := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string"},
		},
		"required": []string{"path"},
	}

	got := injectSummaryParam(orig)

	// 注入后必须有 summary 属性
	props, ok := got["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("properties missing after inject: %v", got)
	}
	sp, ok := props[summaryParamKey].(map[string]interface{})
	if !ok {
		t.Fatalf("summary prop missing: %v", props)
	}
	if sp["type"] != "string" {
		t.Errorf("summary type = %v, want string", sp["type"])
	}

	// required 必须包含 summary
	req, ok := got["required"].([]interface{})
	if !ok {
		t.Fatalf("required not []interface{} after inject: %#v", got["required"])
	}
	found := false
	for _, r := range req {
		if s, _ := r.(string); s == summaryParamKey {
			found = true
		}
	}
	if !found {
		t.Errorf("summary not in required: %v", req)
	}

	// 原始 schema 不能被污染
	if _, ok := orig["properties"].(map[string]interface{})[summaryParamKey]; ok {
		t.Error("original schema must not be mutated with summary prop")
	}
	if len(orig["required"].([]string)) != 1 {
		t.Errorf("original required mutated: %v", orig["required"])
	}
}

// TestInjectSummaryParam_EmptySchema 空 schema 也能安全注入。
func TestInjectSummaryParam_EmptySchema(t *testing.T) {
	got := injectSummaryParam(map[string]interface{}{"type": "object"})
	if got["type"] != "object" {
		t.Errorf("type = %v, want object", got["type"])
	}
	props, _ := got["properties"].(map[string]interface{})
	if _, ok := props[summaryParamKey]; !ok {
		t.Errorf("summary prop missing in empty schema: %v", got)
	}
}

// TestInjectSummaryParam_Nil  nil schema 注入后返回非 nil 可用 schema。
func TestInjectSummaryParam_Nil(t *testing.T) {
	got := injectSummaryParam(nil)
	if got == nil {
		t.Fatal("nil schema should yield non-nil injected schema")
	}
	props, _ := got["properties"].(map[string]interface{})
	if _, ok := props[summaryParamKey]; !ok {
		t.Errorf("summary prop missing for nil schema: %v", got)
	}
}

// TestSplitValidToolCalls 验证已知/未知工具的区分
func TestSplitValidToolCalls(t *testing.T) {
	ts := newTestToolSet("srv__read", "srv__write")
	calls := []llm.ToolCall{
		{Name: "srv__read"},
		{Name: "srv__unknown"},
		{Name: "srv__write"},
	}

	valid, invalid := splitValidToolCalls(calls, ts)

	if len(valid) != 2 {
		t.Errorf("valid = %d, want 2", len(valid))
	}
	if len(invalid) != 1 || invalid[0] != "srv__unknown" {
		t.Errorf("invalid = %v, want [srv__unknown]", invalid)
	}
}

// TestSplitValidToolCalls_Empty 空输入
func TestSplitValidToolCalls_Empty(t *testing.T) {
	ts := newTestToolSet("srv__read")
	valid, invalid := splitValidToolCalls(nil, ts)
	if len(valid) != 0 || len(invalid) != 0 {
		t.Errorf("expected empty results, got valid=%v invalid=%v", valid, invalid)
	}
}

// TestSplitValidToolCalls_NilToolSet 无工具时所有调用都算未知
func TestSplitValidToolCalls_NilToolSet(t *testing.T) {
	calls := []llm.ToolCall{{Name: "a"}, {Name: "b"}}
	valid, invalid := splitValidToolCalls(calls, nil)
	if len(valid) != 0 {
		t.Errorf("valid = %d, want 0", len(valid))
	}
	if len(invalid) != 2 {
		t.Errorf("invalid = %d, want 2", len(invalid))
	}
}

// TestFormatAvailableTools 验证可用工具列表格式化与截断
func TestFormatAvailableTools(t *testing.T) {
	if got := formatAvailableTools(nil); !strings.Contains(got, "无可用工具") {
		t.Errorf("empty list = %q, want hint text", got)
	}

	got := formatAvailableTools([]string{"a", "b", "c"})
	if !strings.Contains(got, "a") || !strings.Contains(got, "c") {
		t.Errorf("short list = %q, should contain all names", got)
	}

	// 超过 20 个必须截断，避免撑爆 LLM 上下文
	many := make([]string, 50)
	for i := range many {
		many[i] = "tool"
	}
	got = formatAvailableTools(many)
	if !strings.Contains(got, "共 50 个") {
		t.Errorf("long list = %q, should mention total count", got)
	}
	if len(got) > 500 {
		t.Errorf("long list output too long (%d chars), should be truncated", len(got))
	}
}

package mcp

import (
	"strings"
	"testing"
)

// TestBuildExposedName 验证 server 前缀命名
func TestBuildExposedName(t *testing.T) {
	cases := []struct {
		server string
		tool   string
		want   string
	}{
		{"filesystem", "read_file", "filesystem__read_file"},
		{"my server", "search", "my_server__search"},
		{"", "standalone", "standalone"},
		{"srv-1", "do.thing", "srv-1__dothing"},
	}
	for _, c := range cases {
		if got := buildExposedName(c.server, c.tool); got != c.want {
			t.Errorf("buildExposedName(%q, %q) = %q, want %q", c.server, c.tool, got, c.want)
		}
	}
}

// TestBuildExposedName_NonASCIIServer server 名全为非 ASCII 时，
// 必须仍生成稳定且带前缀的名字，否则不同 server 的同名工具会撞车
func TestBuildExposedName_NonASCIIServer(t *testing.T) {
	a := buildExposedName("文件系统", "read")
	b := buildExposedName("搜索服务", "read")

	if a == b {
		t.Errorf("different servers produced same name: %q", a)
	}
	if !strings.HasSuffix(a, nameSeparator+"read") {
		t.Errorf("name %q should keep tool suffix", a)
	}
	// 同一 server 名多次调用结果必须稳定
	if again := buildExposedName("文件系统", "read"); again != a {
		t.Errorf("name unstable: %q vs %q", a, again)
	}
}

// TestSanitizeName 验证名称清洗只保留合法字符
func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"read_file":   "read_file",
		"my server":   "my_server",
		"a.b/c":       "abc",
		"  spaced  ":  "spaced",
		"tool@v2":     "toolv2",
		"__leading__": "leading",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDedupExposedNames 重名工具追加序号，保证喂给 LLM 的名字唯一
func TestDedupExposedNames(t *testing.T) {
	tools := []RegisteredTool{
		{ExposedName: "srv__read"},
		{ExposedName: "srv__read"},
		{ExposedName: "srv__read"},
		{ExposedName: "srv__write"},
	}
	out := dedupExposedNames(tools)

	seen := make(map[string]bool, len(out))
	for _, tl := range out {
		if tl.ExposedName == "" {
			t.Error("exposed name should not be empty")
		}
		if seen[tl.ExposedName] {
			t.Errorf("duplicate exposed name after dedup: %s", tl.ExposedName)
		}
		seen[tl.ExposedName] = true
	}
	if len(seen) != 4 {
		t.Errorf("unique names = %d, want 4", len(seen))
	}
	// 第一个保持原名，后续加后缀
	if out[0].ExposedName != "srv__read" {
		t.Errorf("first name = %q, want srv__read", out[0].ExposedName)
	}
	if !strings.HasPrefix(out[1].ExposedName, "srv__read_") {
		t.Errorf("second name = %q, want srv__read_ prefix", out[1].ExposedName)
	}
}

// TestDedupExposedNames_EmptyName 空名字被补齐
func TestDedupExposedNames_EmptyName(t *testing.T) {
	tools := []RegisteredTool{{ExposedName: ""}, {ExposedName: ""}}
	out := dedupExposedNames(tools)
	for i, tl := range out {
		if tl.ExposedName == "" {
			t.Errorf("tool %d still has empty exposed name", i)
		}
	}
	if out[0].ExposedName == out[1].ExposedName {
		t.Error("generated names should be unique")
	}
}

// TestNormalizeSchema 缺字段的 schema 被补成合法 object schema，
// 否则直接喂给 LLM 会被拒
func TestNormalizeSchema(t *testing.T) {
	// nil schema
	s := normalizeSchema(nil)
	if s["type"] != "object" {
		t.Errorf("nil schema type = %v, want object", s["type"])
	}
	if s["properties"] == nil {
		t.Error("nil schema should get empty properties")
	}

	// 缺 type
	s = normalizeSchema(map[string]interface{}{
		"properties": map[string]interface{}{"a": map[string]interface{}{"type": "string"}},
	})
	if s["type"] != "object" {
		t.Errorf("type = %v, want object", s["type"])
	}

	// 缺 properties
	s = normalizeSchema(map[string]interface{}{"type": "object"})
	if s["properties"] == nil {
		t.Error("should get empty properties")
	}

	// 完整 schema 保持不变
	orig := map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}},
		"required":   []string{"q"},
	}
	s = normalizeSchema(orig)
	if s["required"] == nil {
		t.Error("required field should be preserved")
	}
}

// TestBuildDescription 描述带上来源 server，帮助 LLM 区分同类工具
func TestBuildDescription(t *testing.T) {
	got := buildDescription("filesystem", "读取文件")
	if !strings.Contains(got, "filesystem") || !strings.Contains(got, "读取文件") {
		t.Errorf("description = %q, should contain both server and desc", got)
	}

	got = buildDescription("filesystem", "")
	if !strings.Contains(got, "filesystem") {
		t.Errorf("description = %q, should contain server name", got)
	}

	got = buildDescription("", "读取文件")
	if got != "读取文件" {
		t.Errorf("description = %q, want 读取文件", got)
	}
}

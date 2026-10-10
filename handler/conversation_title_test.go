package handler

import "testing"

// TestIsDefaultConversationTitle 占位标题要认得当前语言版本与历史版本写入过的形态。
//
// 回归背景：前端 i18n 后新建对话传的占位标题变成「新建对话」/「New chat」，
// 后端只认「新对话」，isDefaultConversationTitle 返回 false，
// 首条输入便不再自动命名 —— 对话列表里一直挂着占位标题。
func TestIsDefaultConversationTitle(t *testing.T) {
	cases := []struct {
		title string
		want  bool
	}{
		{"", true},
		{"新对话", true},
		{"新建对话", true},
		{"New Chat", true},
		{"New chat", true},
		{"  New Chat  ", true},
		{"未命名对话", true},
		{"Untitled", true},
		{"帮我写一个周报", false},
		{"新对话：测试", false},
	}
	for _, c := range cases {
		if got := isDefaultConversationTitle(c.title); got != c.want {
			t.Errorf("isDefaultConversationTitle(%q) = %v, want %v", c.title, got, c.want)
		}
	}
}

func TestDefaultConversationTitle(t *testing.T) {
	if got := defaultConversationTitle("en"); got != "New Chat" {
		t.Errorf("defaultConversationTitle(en) = %q", got)
	}
	if got := defaultConversationTitle("zh"); got != "新对话" {
		t.Errorf("defaultConversationTitle(zh) = %q", got)
	}
	// 未登记的语言回落中文，且必须仍在占位集合里，否则永远不会被首条输入覆盖。
	if got := defaultConversationTitle("jp"); !isDefaultConversationTitle(got) {
		t.Errorf("fallback title %q is not recognized as placeholder", got)
	}
}

func TestDeriveConversationTitle(t *testing.T) {
	if got := deriveConversationTitle("   "); got != "" {
		t.Errorf("blank input should derive empty title, got %q", got)
	}
	if got := deriveConversationTitle("帮我写一个  周报"); got != "帮我写一个 周报" {
		t.Errorf("deriveConversationTitle = %q", got)
	}
	long := "这是一条非常非常非常长的用户输入用来验证截断逻辑是否正确"
	got := deriveConversationTitle(long)
	runes := []rune(got)
	if len(runes) != 21 { // 20 个字符 + 省略号
		t.Errorf("truncated title length = %d, want 21 (%q)", len(runes), got)
	}
}

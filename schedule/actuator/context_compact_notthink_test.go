package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llmreg"
)

// TestCompactNoThinkBody_OnlyHunyuan 只有混元（openai 协议 + 模型名 hy 开头）
// 才下发关闭思考链的字段。
//
// 写法必须是顶层 reasoning_effort=none：实测 chat_template_kwargs /
// enable_thinking 两种写法在 lkeap 网关上都关不掉思考，甚至更慢。
// 而 reasoning_effort 对非推理模型是未知字段，发给严格校验的网关可能
// 400 —— 那会把本可成功的压缩判死，所以宁可不生效也不能瞎传。
func TestCompactNoThinkBody_OnlyHunyuan(t *testing.T) {
	cases := []struct {
		name string
		spec *llmreg.ModelSpec
		want bool
		desc string
	}{
		{
			name: "混元hy4",
			spec: &llmreg.ModelSpec{Protocol: llmreg.ProtocolOpenAI, Model: "hy4-preview"},
			want: true,
		},
		{
			name: "混元hy3大写前缀",
			spec: &llmreg.ModelSpec{Protocol: llmreg.ProtocolOpenAI, Model: "HY3"},
			want: true,
		},
		{
			name: "openai协议非混元",
			spec: &llmreg.ModelSpec{Protocol: llmreg.ProtocolOpenAI, Model: "gpt-4o-mini"},
			want: false,
		},
		{
			name: "azure协议",
			spec: &llmreg.ModelSpec{Protocol: llmreg.ProtocolGPT, Model: "gpt-5.6"},
			want: false,
		},
		{
			name: "gemini协议",
			spec: &llmreg.ModelSpec{Protocol: llmreg.ProtocolGemini, Model: "gemini-2.5-pro"},
			want: false,
		},
		{
			name: "空spec",
			spec: nil,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := compactNoThinkBody(c.spec)
			if (got != nil) != c.want {
				t.Fatalf("compactNoThinkBody = %v, want non-nil=%v", got, c.want)
			}
			if got == nil {
				return
			}
			// 必须是顶层 reasoning_effort=none
			if got["reasoning_effort"] != "none" {
				t.Fatalf("reasoning_effort = %v, want none (顶层字段，非 chat_template_kwargs)",
					got["reasoning_effort"])
			}
			if _, ok := got["chat_template_kwargs"]; ok {
				t.Fatalf("chat_template_kwargs 实测无效且更慢，不应下发: %#v", got)
			}
		})
	}
}

// TestCheckWithinCeiling 规划期 / 子任务的超限判定。
//
// 这两条路径没有压缩兜底（裁剪已移除），超限必须报错而不是放行 ——
// 放行只会换来 provider 一个看不出原因的 400。
func TestCheckWithinCeiling(t *testing.T) {
	const limit = 16000
	th := contextmgr.NewThresholds(limit)

	under := &model.TurnContextBlocks{
		SystemPrompt: 1000, Tools: 800, UserAndCurrent: 2000,
		Total: 3800, ContextLimit: limit,
	}
	if err := checkWithinCeiling("t-under", under); err != nil {
		t.Fatalf("under ceiling should pass, got %v", err)
	}

	// 典型的撑爆形态：多轮工具结果堆在「当前任务交互」里
	over := &model.TurnContextBlocks{
		SystemPrompt:   1000,
		Tools:          800,
		UserAndCurrent: th.Ceiling, // 仅这一块就顶到底线之上
		ContextLimit:   limit,
	}
	over.Total = 1000 + 800 + th.Ceiling
	err := checkWithinCeiling("t-over", over)
	if err == nil {
		t.Fatal("over ceiling should fail")
	}
	// 错误信息里必须带上各块明细，否则用户只知道「超限」不知道超在哪
	for _, want := range []string{"sys=", "tools=", "cur=", "total=", "ceiling="} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}

	// 边界：正好等于底线不算超
	equal := &model.TurnContextBlocks{Total: th.Ceiling, ContextLimit: limit}
	if err := checkWithinCeiling("t-equal", equal); err != nil {
		t.Fatalf("exactly at ceiling should pass, got %v", err)
	}

	// 未接入分块计量的路径不做判定
	if err := checkWithinCeiling("t-nil", nil); err != nil {
		t.Fatalf("nil blocks should pass, got %v", err)
	}
}

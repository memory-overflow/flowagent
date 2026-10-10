package actuator

import (
	"context"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// summarizeLike 模拟「压缩摘要注入后的真实消息序列」：
// system → 历史摘要(assistant) → 历史段 → 本轮 user → 当前摘要(assistant)。
//
// 这个形状不是臆想的 —— 真实数据里 task 7c09cf38 的第 44 轮输入就是
// assistant / assistant / user / assistant（前后两条 assistant 分别是
// 历史摘要与工作流摘要，末尾是当前 task 摘要）。
func summarizeLike() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: "你是 agent"},
		{Role: llm.RoleAssistant, Content: "【此前对话的历史工作记录（已压缩）】\n- 用户目标…"},
		{Role: llm.RoleAssistant, Content: "【此前工作流执行摘要】\n总体目标：…"},
		{Role: llm.RoleUser, Content: "本轮提问"},
		{Role: llm.RoleAssistant, Content: "【当前任务已压缩的交互】\n- 查过 A…"},
	}
}

// assertNoAdjacentSameRole 断言序列里没有相邻的同角色文本消息
// （tool 结果本就允许连续，不在校验范围内）。
func assertNoAdjacentSameRole(t *testing.T, tag string, msgs []llm.Message) {
	t.Helper()
	for i := 1; i < len(msgs); i++ {
		prev, cur := msgs[i-1], msgs[i]
		if cur.Role == llm.RoleTool || prev.Role == llm.RoleTool {
			continue
		}
		if prev.Role == cur.Role {
			t.Errorf("%s: 位置 %d 与 %d 仍为同角色 %s",
				tag, i-1, i, cur.Role)
		}
	}
}

func TestCollapseAdjacentSameRole(t *testing.T) {
	cases := []struct {
		name       string
		msgs       []llm.Message
		wantLen    int
		wantIn     string // 期望出现在合并后文本里的片段
		wantImages int    // 期望保留的图片数（0 表示不校验）
		wantCalls  int    // 期望保留的工具调用数（0 表示不校验）
		brief      string
	}{
		{
			name:    "历史摘要与工作流摘要连续：必须合并",
			msgs:    summarizeLike(),
			wantLen: 4,
			wantIn:  "【此前工作流执行摘要】",
			brief:   "task 7c09cf38 第 44 轮的真实形态",
		},
		{
			name: "工具结果隔开的 assistant 不合并",
			msgs: []llm.Message{
				{Role: llm.RoleAssistant, Content: "先查一下", ToolCalls: []llm.ToolCall{{Name: "fetch"}}},
				{Role: llm.RoleTool, Content: "结果A", ToolName: "fetch"},
				{Role: llm.RoleAssistant, Content: "再想想"},
			},
			wantLen: 3,
			brief:   "tool 结果天然是分隔符，这里的 assistant 并不相邻",
		},
		{
			name: "当前摘要后紧跟带工具调用的 assistant：合并且保留调用",
			msgs: []llm.Message{
				{Role: llm.RoleAssistant, Content: "【当前摘要】"},
				{Role: llm.RoleAssistant, Content: "", ToolCalls: []llm.ToolCall{{Name: "deep_search"}, {Name: "deep_search"}}},
				{Role: llm.RoleTool, Content: "结果1", ToolName: "deep_search"},
				{Role: llm.RoleTool, Content: "结果2", ToolName: "deep_search"},
			},
			wantLen:   3,
			wantCalls: 2,
			brief:     "chat 8a3c060c 的真实形态：不合并会留下连续 assistant",
		},
		{
			name: "连续 tool 结果保持独立",
			msgs: []llm.Message{
				{Role: llm.RoleAssistant, Content: "", ToolCalls: []llm.ToolCall{{Name: "a"}, {Name: "b"}}},
				{Role: llm.RoleTool, Content: "r1", ToolName: "a"},
				{Role: llm.RoleTool, Content: "r2", ToolName: "b"},
			},
			wantLen: 3,
			brief:   "并行调用的多个结果是同角色的合法连续，合并会破坏结构",
		},
		{
			name: "连续 user 合并",
			msgs: []llm.Message{
				{Role: llm.RoleUser, Content: "问题一"},
				{Role: llm.RoleUser, Content: "问题二"},
			},
			wantLen: 1,
			wantIn:  "问题二",
			brief:   "历史重建里「某 task 只剩提问」会产生连续 user",
		},
		{
			name: "多模态消息合并但图片不丢",
			msgs: []llm.Message{
				{Role: llm.RoleUser, Content: "看图", Images: []llm.ImagePart{{MimeType: "image/png", Data: "x"}}},
				{Role: llm.RoleUser, Content: "再看这张", Images: []llm.ImagePart{{MimeType: "image/png", Data: "y"}}},
			},
			wantLen:    1,
			wantIn:     "再看这张",
			wantImages: 2,
			brief:      "图片按序并入前一条，避免连续 user 触发协议报错",
		},
		{
			name: "空内容合并时不引入多余空行",
			msgs: []llm.Message{
				{Role: llm.RoleAssistant, Content: ""},
				{Role: llm.RoleAssistant, Content: "正文"},
			},
			wantLen: 1,
			wantIn:  "正文",
			brief:   "空 assistant 消息（被过滤的 turn 残留）不该撑出空行",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collapseAdjacentSameRole(c.msgs)
			if len(got) != c.wantLen {
				t.Fatalf("len = %d, want %d（%s）：%+v", len(got), c.wantLen, c.brief, got)
			}
			if c.wantIn != "" {
				joined := ""
				for _, m := range got {
					joined += m.Content
				}
				if !strings.Contains(joined, c.wantIn) {
					t.Errorf("合并后缺少 %q：%+v", c.wantIn, got)
				}
			}
			assertNoAdjacentSameRole(t, c.name, got)
			if c.wantCalls > 0 {
				n := 0
				for _, m := range got {
					n += len(m.ToolCalls)
				}
				if n != c.wantCalls {
					t.Errorf("工具调用数 = %d, want %d（%s）", n, c.wantCalls, c.brief)
				}
			}
			if c.wantImages > 0 {
				n := 0
				for _, m := range got {
					n += len(m.Images)
				}
				if n != c.wantImages {
					t.Errorf("图片数 = %d, want %d（%s）", n, c.wantImages, c.brief)
				}
			}
		})
	}
}

// TestCollapseAdjacentSameRole_KeepsOrder 折叠只能改条数，不能改时序。
func TestCollapseAdjacentSameRole_KeepsOrder(t *testing.T) {
	got := collapseAdjacentSameRole(summarizeLike())
	wantRoles := []llm.Role{llm.RoleSystem, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant}
	if len(got) != len(wantRoles) {
		t.Fatalf("roles = %d 条, want %d", len(got), len(wantRoles))
	}
	for i, r := range wantRoles {
		if got[i].Role != r {
			t.Errorf("位置 %d 角色 = %s, want %s", i, got[i].Role, r)
		}
	}
	// 合并进同一条的必须是「先历史摘要、后工作流摘要」这个顺序
	if !strings.Contains(got[1].Content, "历史工作记录") ||
		!strings.Contains(got[1].Content, "工作流执行摘要") {
		t.Errorf("合并内容不完整：%q", got[1].Content)
	}
	// 顺序必须是「历史摘要在前」——折叠不能颠倒时序
	if strings.Index(got[1].Content, "历史工作记录") > strings.Index(got[1].Content, "工作流执行摘要") {
		t.Errorf("合并后时序颠倒：%q", got[1].Content)
	}
}

// TestRealData_AssembledHasNoAdjacentSameRole 真实数据回归：
// 把各对话重建出的历史按「注入摘要」的形态拼起来，折叠后不许再出现
// 相邻同角色 —— 这是 Gemini 协议（要求 user/model 严格交替）的硬要求。
func TestRealData_AssembledHasNoAdjacentSameRole(t *testing.T) {
	realDataGuard(t)
	ctx := context.Background()
	turnDAO := &dao.TaskTurnDAO{}
	taskDAO := &dao.TaskDAO{}

	for _, chatID := range listBusiestChats(ctx, t, 5) {
		turns, err := turnDAO.ListTurnsByChatID(ctx, chatID)
		if err != nil || len(turns) == 0 {
			continue
		}
		currentTaskID := turns[len(turns)-1].TaskID
		segs, current, err := buildSegmentedHistory(ctx, turnDAO, taskDAO, chatID, currentTaskID, nil)
		if err != nil {
			t.Errorf("chat %s: build failed: %v", chatID, err)
			continue
		}

		msgs := []llm.Message{{Role: llm.RoleSystem, Content: "system"}}
		if len(segs) > 0 {
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: "【历史摘要】"})
		}
		msgs = append(msgs, flattenSegments(segs)...)
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "本轮提问"})
		if len(current) > 0 {
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: "【当前摘要】"})
		}
		msgs = append(msgs, current...)

		got := collapseAdjacentSameRole(msgs)
		assertNoAdjacentSameRole(t, chatID, got)
		t.Logf("chat %s: %d 段 / %d current → 折叠后 %d 条 ✓", chatID, len(segs), len(current), len(got))
	}
}

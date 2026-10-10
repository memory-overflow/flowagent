package actuator

import (
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// TestBuildWorkflowSummaryPrompt_Success 全部完成：指令要求直接给结论、不罗列中间过程。
func TestBuildWorkflowSummaryPrompt_Success(t *testing.T) {
	nodes := []model.WorkflowNodeData{
		{NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted,
			Output: &model.AgentOutputData{ReplyText: "找到候选 A/B/C"}},
		{NodeID: "n2", Name: "汇总", Status: model.TaskStatusCompleted,
			Output: &model.AgentOutputData{ReplyText: "结论是 X"}},
	}
	msgs := buildWorkflowSummaryPrompt("写一份调研报告", nodes, model.TaskStatusCompleted)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != llm.RoleSystem {
		t.Errorf("first message should be system, got %v", msgs[0].Role)
	}
	// system prompt 必须禁止 LLM 生成下载链接/路径：否则汇总里会出现裸相对路径
	// （如 outputs/xxx.md），前端点不动。下载入口由 handler 从节点 OutputFiles 追加。
	if !strings.Contains(msgs[0].Content, "不要输出任何文件下载链接或文件路径") {
		t.Errorf("system prompt should forbid download links/paths, got:\n%s", msgs[0].Content)
	}
	user := msgs[1].Content
	for _, want := range []string{"写一份调研报告", "检索", "汇总", "找到候选 A/B/C", "结论是 X", "成功完成", "不要再罗列中间过程"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt should contain %q, got:\n%s", want, user)
		}
	}
}

// TestBuildWorkflowSummaryPrompt_Failed 有失败：指令要求给出失败位置与原因。
func TestBuildWorkflowSummaryPrompt_Failed(t *testing.T) {
	nodes := []model.WorkflowNodeData{
		{NodeID: "n1", Name: "检索", Status: model.TaskStatusCompleted,
			Output: &model.AgentOutputData{ReplyText: "OK"}},
		{NodeID: "n2", Name: "分析", Status: model.TaskStatusFailed},
	}
	msgs := buildWorkflowSummaryPrompt("目标", nodes, model.TaskStatusFailed)
	user := msgs[1].Content
	for _, want := range []string{"分析", "失败", "失败原因", "影响"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt should contain %q, got:\n%s", want, user)
		}
	}
	if strings.Contains(user, "成功完成") {
		t.Errorf("failed prompt should not carry success instruction, got:\n%s", user)
	}
}

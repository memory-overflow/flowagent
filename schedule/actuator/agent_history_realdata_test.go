package actuator

import (
	"context"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/dao"
)

// ============================================================
// 真实数据校验（需要 DB，默认跳过）
// ============================================================
//
//	CTX_COMPACT_DB_CHECK=1 go test ./schedule/actuator/ -run TestRealDataWorkflowSummary -v

// TestRealDataWorkflowSummaryInjected 工作流摘要必须真的落到回放历史里。
//
// 背景（chat 8a3c060c）：摘要本身生成成功（430 字符），但一个段都没注入，
// 于是上下文用量面板里 workflow_summary 恒为 0、模型也看不到此前的工作流。
//
// 根因是边界判定用「是否在子任务里」这个布尔量：该对话先有普通对话任务
// 派生的子任务（父任务 2957d8b4，非工作流，无摘要），把标志置起来后，
// 后面真工作流（5797135f）的边界就被吞掉了。改为按父任务身份判定。
func TestRealDataWorkflowSummaryInjected(t *testing.T) {
	realDataGuard(t)
	ctx := context.Background()
	turnDAO := &dao.TaskTurnDAO{}
	taskDAO := &dao.TaskDAO{}

	const (
		chatID = "8a3c060c-5acb-4f0c-99c2-c224f6234065"
		taskID = "7c09cf38-f394-4496-ac40-177928c91ced"
		wfID   = "5797135f-f2be-4196-b813-76c5536feb57"
	)

	segs, _, err := buildSegmentedHistory(ctx, turnDAO, taskDAO, chatID, taskID, nil)
	if err != nil {
		t.Fatalf("build segmented history: %v", err)
	}

	count := 0
	for _, s := range segs {
		if s.Kind != segKindWorkflowSummary {
			continue
		}
		count++
		if s.TaskID != wfID {
			t.Fatalf("摘要段归属错误: got %s want %s", s.TaskID, wfID)
		}
		if len(s.Messages) != 1 || !strings.Contains(s.Messages[0].Content, "【此前工作流执行摘要】") {
			t.Fatalf("摘要段内容异常: %+v", s.Messages)
		}
	}
	if count != 1 {
		t.Fatalf("工作流摘要应恰好注入 1 份，实际 %d 份（segs=%d）", count, len(segs))
	}
}

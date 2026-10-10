package actuator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// TestRealData_ChunkedCompactionUnderTinyWindow 用真实历史 + 极小窗口，
// 复现「从大窗口模型切换到小窗口模型」的场景。
//
// 关注两点：
//  1. 分块确实被触发（而非一次调用把素材全塞进去）
//  2. 每一次压缩调用的输入都落在预算内 —— 这是本次改造要守的硬约束
//
// 用法：CTX_COMPACT_DB_CHECK=1 go test ./schedule/actuator/ -run TestRealData_Chunked -v
func TestRealData_ChunkedCompactionUnderTinyWindow(t *testing.T) {
	realDataGuard(t)
	ctx := context.Background()
	turnDAO := &dao.TaskTurnDAO{}
	taskDAO := &dao.TaskDAO{}

	// 极小窗口：上限 8K → 压缩调用预算 0.6*8192 ≈ 4.9K
	// 真实历史随便一个长对话都能轻松超过这个量，必然触发分块。
	th := contextmgr.NewThresholds(8192)

	chats := listBusiestChats(ctx, t, 5)
	if len(chats) == 0 {
		t.Skip("库里没有 turn 数据")
	}

	counter := contextmgr.GetCounter()
	totalCalls, chatsChunked := 0, 0

	for _, chatID := range chats {
		turns, err := turnDAO.ListTurnsByChatID(ctx, chatID)
		if err != nil || len(turns) == 0 {
			continue
		}
		currentTaskID := turns[len(turns)-1].TaskID

		segs, _, err := buildSegmentedHistory(ctx, turnDAO, taskDAO, chatID, currentTaskID, nil)
		if err != nil {
			t.Errorf("chat %s: build history failed: %v", chatID, err)
			continue
		}
		// 只留可压缩的对话段（工作流摘要段不可压）
		var picked []historySegment
		detail := 0
		for i := range segs {
			segs[i].Tokens = counter.CountMessages(segs[i].Messages)
			if segs[i].Kind == segKindWorkflowSummary {
				continue
			}
			detail += segs[i].Tokens
			picked = append(picked, segs[i])
		}
		if len(picked) == 0 {
			continue
		}

		cli := &fakeCompactClient{}
		in := compactInput{
			Temp: &model.TaskTemp{
				Task: model.Task{TaskID: "t-real", ChatID: chatID},
			},
			Client:        cli,
			CompactBudget: th.CompactCall,
		}

		out, err := compactOnce(ctx, in, nil, picked, th.Summary)
		if err != nil {
			t.Errorf("chat %s: compactOnce failed: %v", chatID, err)
			continue
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("chat %s: empty summary", chatID)
		}
		if cli.maxTokens > int(float64(th.CompactCall)*1.1) {
			t.Errorf("chat %s: a compaction call used %d tokens, over budget %d",
				chatID, cli.maxTokens, th.CompactCall)
		}
		totalCalls += cli.calls
		if cli.calls > 1 {
			chatsChunked++
		}
		t.Logf("chat %s: detail=%d tokens -> %d compaction calls (budget=%d, maxCall=%d tokens)",
			chatID, detail, cli.calls, th.CompactCall, cli.maxTokens)
	}

	t.Logf("合计 %d 次压缩调用，%d/%d 个对话触发了分块",
		totalCalls, chatsChunked, len(chats))
	if chatsChunked == 0 {
		t.Errorf("没有对话触发分块，无法证明分块路径在真实数据上生效")
	}
}

// TestRealData_OversizedPrevSummaryIsSplittable 验证「旧摘要自身超限」也能切。
//
// 这是模型降级路径里最容易被忽略的一环：摘要是在大窗口模型下按 10%
// 生成的，换到小窗口模型后它可能已经超过新的块预算，而它是无结构纯文本，
// 只能按行切。不处理就会在第一次压缩调用时直接把 provider 打爆。
func TestRealData_OversizedPrevSummaryIsSplittable(t *testing.T) {
	realDataGuard(t)
	ctx := context.Background()

	counter := contextmgr.GetCounter()
	th := contextmgr.NewThresholds(32768)

	// 构造一份「按 1M 窗口生成的」旧摘要：10% * 1M = 100K token
	var sb strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&sb, "- 第 %d 项：改造了 pkg/module%d 下的实现，结论是保留旧口径并补充兜底分支。\n",
			i, i%17)
	}
	prev := &model.ContextSummary{Summary: sb.String()}
	prevTokens := counter.CountText(prev.Summary)

	cli := &fakeCompactClient{}
	in := compactInput{
		Temp: &model.TaskTemp{
			Task: model.Task{TaskID: "t-prev", ChatID: "c-prev"},
		},
		Client:        cli,
		CompactBudget: th.CompactCall,
	}

	out, err := compactOnce(ctx, in, prev, []historySegment{
		{Kind: segKindConversation, TaskID: "t1", Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "接下来继续处理遗留问题"},
		}},
	}, th.Summary)
	if err != nil {
		t.Fatalf("compactOnce failed: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("empty summary")
	}
	if cli.calls < 2 {
		t.Errorf("oversized prev summary should force chunking, got %d calls", cli.calls)
	}
	if cli.maxTokens > int(float64(th.CompactCall)*1.1) {
		t.Errorf("a compaction call used %d tokens, over budget %d", cli.maxTokens, th.CompactCall)
	}
	t.Logf("旧摘要 %d tokens（块预算 %d）-> 切成 %d 次调用，单次最大 %d tokens",
		prevTokens, th.CompactCall, cli.calls, cli.maxTokens)
}

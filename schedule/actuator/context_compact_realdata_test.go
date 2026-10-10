package actuator

import (
	"context"
	"os"
	"sort"
	"testing"

	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// ============================================================
// 真实数据校验（需要 DB，默认跳过）
// ============================================================
//
// 跑法：
//
//	CTX_COMPACT_DB_CHECK=1 go test ./schedule/actuator/ -run TestRealData -v
//	CTX_COMPACT_DB_CONFIG=/path/to/other/config.yaml CTX_COMPACT_DB_CHECK=1 go test ...
//
// 默认跳过的原因：单测不该依赖外部 MySQL。但「分段重建与原实现是否
// 逐条一致」这个不变量只有在真实历史（含工作流子任务、失败 task、
// 孤立 tool 结果等各种畸形数据）上才验得出来，构造用例覆盖不到。
//
// CTX_COMPACT_DB_CONFIG 用来指定另一份 conf.yaml：默认那份会带上
// config/db_config.json 的系统设置覆盖（现在指向本机库），而本机库一开始
// 是空的 —— 要跑云上那批历史数据，就指向一个目录里没有 db_config.json
// 的配置。

// realDataGuard 检查是否启用真实数据校验，并初始化 DB。
func realDataGuard(t *testing.T) {
	t.Helper()
	if os.Getenv("CTX_COMPACT_DB_CHECK") == "" {
		t.Skip("设置 CTX_COMPACT_DB_CHECK=1 启用真实数据校验")
	}
	cfgPath := os.Getenv("CTX_COMPACT_DB_CONFIG")
	if cfgPath == "" {
		cfgPath = "../../config/config.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := dao.Init(cfg.Database); err != nil {
		t.Fatalf("init db: %v", err)
	}
}

// listBusiestChats 取 turn 数最多的若干对话 —— 它们最可能包含各种畸形历史。
func listBusiestChats(ctx context.Context, t *testing.T, limit int) []string {
	t.Helper()
	var rows []struct {
		ChatID string
		N      int
	}
	if err := dao.DB().WithContext(ctx).
		Table("task_turn").
		Select("chat_id, COUNT(*) as n").
		Group("chat_id").
		Order("n DESC").
		Limit(limit).
		Scan(&rows).Error; err != nil {
		t.Fatalf("scan chats: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ChatID != "" {
			out = append(out, r.ChatID)
		}
	}
	return out
}

// TestRealData_SegmentedHistoryMatchesLegacy 核心回归：
// 未压缩时，分段重建拍平后必须与原始拍平重建逐条一致。
//
// 这是整个重构的安全网 —— buildFullHistory 被改写成「先分段再拍平」，
// 若两者产出不同，就意味着所有既有对话的历史回放都被悄悄改变了。
func TestRealData_SegmentedHistoryMatchesLegacy(t *testing.T) {
	realDataGuard(t)
	ctx := context.Background()
	turnDAO := &dao.TaskTurnDAO{}
	taskDAO := &dao.TaskDAO{}

	chats := listBusiestChats(ctx, t, 8)
	if len(chats) == 0 {
		t.Skip("库里没有 turn 数据")
	}

	for _, chatID := range chats {
		turns, err := turnDAO.ListTurnsByChatID(ctx, chatID)
		if err != nil || len(turns) == 0 {
			continue
		}
		// 用最后一条 turn 所属 task 当「当前 task」，最接近真实调用形态
		currentTaskID := turns[len(turns)-1].TaskID

		legacyBefore, legacyCurrent, err := buildFullHistory(ctx, turnDAO, taskDAO, chatID, currentTaskID)
		if err != nil {
			t.Errorf("chat %s: legacy build failed: %v", chatID, err)
			continue
		}
		segs, segCurrent, err := buildSegmentedHistory(ctx, turnDAO, taskDAO, chatID, currentTaskID, nil)
		if err != nil {
			t.Errorf("chat %s: segmented build failed: %v", chatID, err)
			continue
		}

		assertSameMessages(t, chatID+" before", legacyBefore, flattenSegments(segs))
		assertSameMessages(t, chatID+" current", legacyCurrent, segCurrent)

		t.Logf("chat %s: %d turns → %d 段 / %d before 条 / %d current 条 ✓",
			chatID, len(turns), len(segs), len(legacyBefore), len(legacyCurrent))
	}
}

// TestRealData_CoveredTasksAreExcluded 校验压缩水位线真的生效：
// 把前半批 task 标成 covered 后，重建出的历史必须不再含它们的 turn。
func TestRealData_CoveredTasksAreExcluded(t *testing.T) {
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

		full, _, err := buildSegmentedHistory(ctx, turnDAO, taskDAO, chatID, currentTaskID, nil)
		if err != nil {
			t.Errorf("chat %s: build failed: %v", chatID, err)
			continue
		}
		// 收集可压缩的对话段 taskID（工作流摘要段不参与压缩）
		var convTaskIDs []string
		for i := range full {
			if full[i].Kind == segKindConversation {
				convTaskIDs = append(convTaskIDs, full[i].TaskID)
			}
		}
		if len(convTaskIDs) < 2 {
			continue // 段太少，构不成「压一半」的场景
		}

		half := convTaskIDs[:len(convTaskIDs)/2]
		covered := make(map[string]bool, len(half))
		for _, id := range half {
			covered[id] = true
		}

		after, _, err := buildSegmentedHistory(ctx, turnDAO, taskDAO, chatID, currentTaskID, covered)
		if err != nil {
			t.Errorf("chat %s: build with covered failed: %v", chatID, err)
			continue
		}

		for i := range after {
			if after[i].Kind == segKindConversation && covered[after[i].TaskID] {
				t.Errorf("chat %s: task %s 已标记 covered，仍出现在重建历史里",
					chatID, after[i].TaskID)
			}
		}

		// 压缩后段数必须真的变少，否则水位线没起作用
		if len(after) >= len(full) {
			t.Errorf("chat %s: 标记 %d 个 task 为 covered 后段数未减少 (%d → %d)",
				chatID, len(half), len(full), len(after))
		} else {
			t.Logf("chat %s: covered %d 个 task → 段数 %d → %d ✓",
				chatID, len(half), len(full), len(after))
		}
	}
}

// TestRealData_TokenAccountingAndSelection 在真实历史上跑一遍计量与选取，
// 输出五块占比和「前 80%」的实际落点，供人工核对口径是否合理。
func TestRealData_TokenAccountingAndSelection(t *testing.T) {
	realDataGuard(t)
	ctx := context.Background()
	turnDAO := &dao.TaskTurnDAO{}
	taskDAO := &dao.TaskDAO{}
	counter := contextmgr.GetCounter()

	t.Logf("tiktoken degraded=%v", counter.Degraded())

	// 允许用环境变量指定对话/当前 task，便于复现特定的高水位场景
	chats := listBusiestChats(ctx, t, 5)
	if cid := os.Getenv("CTX_COMPACT_CHAT"); cid != "" {
		chats = []string{cid}
	}

	for _, chatID := range chats {
		turns, err := turnDAO.ListTurnsByChatID(ctx, chatID)
		if err != nil || len(turns) == 0 {
			continue
		}
		currentTaskID := turns[len(turns)-1].TaskID
		if tid := os.Getenv("CTX_COMPACT_TASK"); tid != "" {
			currentTaskID = tid
		}

		segs, current, err := buildSegmentedHistory(ctx, turnDAO, taskDAO, chatID, currentTaskID, nil)
		if err != nil {
			continue
		}

		var compressible []historySegment
		wfTokens := 0
		wfSeen := make(map[string]int)
		for i := range segs {
			segs[i].Tokens = counter.CountMessages(segs[i].Messages)
			if segs[i].Kind == segKindWorkflowSummary {
				wfTokens += segs[i].Tokens
				wfSeen[segs[i].TaskID]++
			} else {
				compressible = append(compressible, segs[i])
			}
		}
		// 不变量：同一条工作流的摘要只能出现一次。
		// 重复注入的来源见 agent_history.go 的跨入边界判定 —— 曾实测同一份
		// 摘要被塞 3 次（626 tok × 3），纯浪费不可压预算。
		for wfID, n := range wfSeen {
			if n > 1 {
				t.Errorf("chat %s: 工作流 %s 的摘要被注入 %d 次，应去重为 1 次", chatID, wfID, n)
			}
		}
		detail := 0
		for i := range compressible {
			detail += compressible[i].Tokens
		}
		curTokens := counter.CountMessages(current)

		if detail == 0 {
			continue
		}

		picked, pickedTokens := pickSegmentsToCompact(compressible, detail)

		// 不变量：选中的必须是前缀，且不能为空（否则压缩无法推进）
		if len(picked) == 0 {
			t.Errorf("chat %s: 有 %d tok 可压详情却选不出任何段", chatID, detail)
		}
		for i := range picked {
			if picked[i].TaskID != compressible[i].TaskID {
				t.Errorf("chat %s: 选取结果不是前缀，第 %d 段是 %s，应为 %s",
					chatID, i, picked[i].TaskID, compressible[i].TaskID)
			}
			if picked[i].Tokens != compressible[i].Tokens {
				t.Errorf("chat %s: 第 %d 段被拆分了（task 原子性破坏）", chatID, i)
			}
		}

		sizes := make([]int, 0, len(compressible))
		for i := range compressible {
			sizes = append(sizes, compressible[i].Tokens)
		}
		sorted := append([]int{}, sizes...)
		sort.Sort(sort.Reverse(sort.IntSlice(sorted)))

		total := detail + wfTokens + curTokens
		t.Logf("chat %s: 可压=%d段/%dtok 工作流摘要=%dtok 当前task=%dtok 合计=%dtok\n"+
			"          最大段=%dtok(占可压%.1f%%) 选取=%d/%d段 %dtok(%.1f%%)",
			chatID[:8], len(compressible), detail, wfTokens, curTokens, total,
			sorted[0], float64(sorted[0])/float64(detail)*100,
			len(picked), len(compressible), pickedTokens,
			float64(pickedTokens)/float64(detail)*100)

		for _, m := range []struct {
			Name  string
			Limit int
		}{
			{"hy4-preview(1M)", 1048576},
			{"gpt-5.6-sol(922K)", 922000},
			{"未配置(128K)", contextmgr.DefaultContextLimit},
		} {
			th := contextmgr.NewThresholds(m.Limit)
			mark := "不触发"
			if total > th.Trigger {
				mark = ">>> 触发压缩"
			}
			t.Logf("          水位[%s] trigger=%d → %s", m.Name, th.Trigger, mark)
		}
	}
}

// assertSameMessages 逐条比较两个消息序列。
func assertSameMessages(t *testing.T, label string, want, got []llm.Message) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("%s: 消息条数不同 %d vs %d", label, len(want), len(got))
		return
	}
	for i := range want {
		if want[i].Role != got[i].Role {
			t.Errorf("%s: 第 %d 条 role 不同 %s vs %s", label, i, want[i].Role, got[i].Role)
			return
		}
		if want[i].Content != got[i].Content {
			t.Errorf("%s: 第 %d 条 content 不同（长度 %d vs %d）",
				label, i, len(want[i].Content), len(got[i].Content))
			return
		}
		if want[i].ToolName != got[i].ToolName {
			t.Errorf("%s: 第 %d 条 tool_name 不同 %q vs %q",
				label, i, want[i].ToolName, got[i].ToolName)
			return
		}
		if len(want[i].ToolCalls) != len(got[i].ToolCalls) {
			t.Errorf("%s: 第 %d 条 tool_calls 数量不同 %d vs %d",
				label, i, len(want[i].ToolCalls), len(got[i].ToolCalls))
			return
		}
		for j := range want[i].ToolCalls {
			if want[i].ToolCalls[j].Name != got[i].ToolCalls[j].Name {
				t.Errorf("%s: 第 %d 条第 %d 个 tool_call 名称不同", label, i, j)
				return
			}
			if want[i].ToolCalls[j].Signature != got[i].ToolCalls[j].Signature {
				t.Errorf("%s: 第 %d 条第 %d 个 tool_call signature 不同", label, i, j)
				return
			}
		}
	}
}

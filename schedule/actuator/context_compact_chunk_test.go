package actuator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/contextmgr"
	"github.com/memory-overflow/flowagent/pkg/llm"
)

// fakeCompactClient 记录每次压缩调用的「素材长度」，返回固定长度摘要。
//
// 用它可以验证两条硬约束：
//  1. 每次调用的输入都不超预算（这是本次改造的目的）
//  2. 极端素材量下调用次数有界且最终能产出一份摘要
type fakeCompactClient struct {
	maxInputSeen int // 按字节记，仅作参考（中文 UTF-8 占 3 字节，别拿它当字符读）
	maxTokens    int // 按 token 记 —— 这才是要守的硬约束
	calls        int
	// failAt 第 N 次调用返回错误，用于验证「任一批失败即整体失败」
	failAt int
}

func (f *fakeCompactClient) ChatWithTools(
	_ context.Context, msgs []llm.Message, _ []llm.Tool, _ llm.ToolChoice, _ llm.Options,
) (*llm.ChatResult, error) {
	f.calls++
	if f.failAt > 0 && f.calls == f.failAt {
		return nil, errors.New("boom")
	}
	cnt := contextmgr.GetCounter()
	n, tk := 0, 0
	for _, m := range msgs {
		n += len(m.Content)
		tk += cnt.CountText(m.Content)
	}
	if n > f.maxInputSeen {
		f.maxInputSeen = n
	}
	if tk > f.maxTokens {
		f.maxTokens = tk
	}
	return &llm.ChatResult{Text: fmt.Sprintf("摘要#%d", f.calls)}, nil
}

func (f *fakeCompactClient) Chat(
	ctx context.Context, msgs []llm.Message, opts llm.Options,
) (string, error) {
	res, err := f.ChatWithTools(ctx, msgs, nil, llm.ToolChoiceNone, opts)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

func (f *fakeCompactClient) Name() string { return "fake" }

// chunkInput 构造一次压缩所需的输入。
func chunkInput(c *fakeCompactClient, budget int) compactInput {
	return compactInput{
		Temp: &model.TaskTemp{
			Task: model.Task{TaskID: "t-chunk", ChatID: "c-chunk"},
		},
		Client:        c,
		CompactBudget: budget,
	}
}

// bigSeg 构造一个「渲染后约 n 汉字」的段（每条约 100 字）。
func bigSeg(taskID string, nChars int) historySegment {
	var b strings.Builder
	msgs := make([]llm.Message, 0, nChars/100+1)
	for b.Len() < nChars {
		line := fmt.Sprintf("%s 这是一行用于撑大体量的测试文本，编号 %d。", taskID, b.Len())
		b.WriteString(line)
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: line})
	}
	return historySegment{
		TaskID:   taskID,
		Kind:     segKindConversation,
		Messages: msgs,
	}
}

// ============================================================
// 分块：单块内不超预算
// ============================================================

func TestGroupUnits_RespectsBudget(t *testing.T) {
	units := []compactUnit{
		{text: "a", tokens: 400},
		{text: "b", tokens: 400},
		{text: "c", tokens: 400},
		{text: "d", tokens: 100},
	}
	batches := groupUnits(units, 1000)
	for i, b := range batches {
		n := 0
		for _, u := range b {
			n += u.tokens
		}
		if n > 1000 {
			t.Errorf("batch %d has %d tokens, exceeds budget 1000", i, n)
		}
		if len(b) == 0 {
			t.Fatalf("batch %d is empty", i)
		}
	}
	if len(batches) != 2 {
		t.Errorf("want 2 batches, got %d", len(batches))
	}
}

func TestGroupUnits_NeverProducesEmptyBatch(t *testing.T) {
	// 单个单元就超预算：必须自成一个批次，不能产出空批后把素材丢掉
	batches := groupUnits([]compactUnit{{text: "huge", tokens: 5000}}, 1000)
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("want 1 batch with 1 unit, got %+v", batches)
	}
}

func TestGroupUnits_Empty(t *testing.T) {
	if b := groupUnits(nil, 100); b != nil {
		t.Errorf("want nil, got %+v", b)
	}
}

// ============================================================
// 段内切分：段放不下时退到消息粒度
// ============================================================

func TestSplitSegmentUnits_WholeWhenFits(t *testing.T) {
	c := contextmgr.GetCounter()
	seg := bigSeg("t1", 500)
	units := splitSegmentUnits(seg, 100000, c)
	if len(units) != 1 {
		t.Fatalf("want 1 unit when segment fits, got %d", len(units))
	}
}

func TestSplitSegmentUnits_SplitsByMessageWhenTooBig(t *testing.T) {
	c := contextmgr.GetCounter()
	seg := bigSeg("t1", 40000) // 约 400 条消息
	units := splitSegmentUnits(seg, 2000, c)
	if len(units) < 2 {
		t.Fatalf("want multiple units for oversized segment, got %d", len(units))
	}
	for i, u := range units {
		if u.tokens > 2000 {
			t.Errorf("unit %d has %d tokens, exceeds budget 2000", i, u.tokens)
		}
	}
	// 内容不能丢：切分后的总字符数应与原渲染结果一致
	want := renderSegmentsForCompaction([]historySegment{seg})
	got := 0
	for _, u := range units {
		got += len(u.text)
	}
	if got != len(want) {
		t.Errorf("content lost during split: %d -> %d chars", len(want), got)
	}
}

func TestSplitTextUnits_SplitsByLine(t *testing.T) {
	c := contextmgr.GetCounter()
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "第 %d 行：这是一段用于测试按行切分的文本内容。\n", i)
	}
	units := splitTextUnits(b.String(), 1000, c)
	if len(units) < 2 {
		t.Fatalf("want multiple units, got %d", len(units))
	}
	total := 0
	for _, u := range units {
		total += len(u.text)
		if u.tokens > 1000 {
			t.Errorf("unit has %d tokens, exceeds budget", u.tokens)
		}
	}
	if total != b.Len() {
		t.Errorf("content lost: %d -> %d", b.Len(), total)
	}
}

// ============================================================
// 端到端：分块压缩的调用次数与输入上限
// ============================================================

// TestCompactOnce_ChunksWhenPayloadExceedsBudget 是本次改造的核心用例：
// 素材远超单次调用预算时，必须分块，且**每一次调用的输入都不超预算**。
func TestCompactOnce_ChunksWhenPayloadExceedsBudget(t *testing.T) {
	c := &fakeCompactClient{}
	in := chunkInput(c, 3000)

	// 4 段 × 约 4000 字 ≈ 远超 3000 token 的预算
	segs := []historySegment{
		bigSeg("t1", 4000), bigSeg("t2", 4000),
		bigSeg("t3", 4000), bigSeg("t4", 4000),
	}
	out, err := compactOnce(context.Background(), in, nil, segs, 500)
	if err != nil {
		t.Fatalf("compactOnce failed: %v", err)
	}
	if !strings.Contains(out, "已压缩") {
		t.Errorf("unexpected output: %q", out)
	}
	if c.calls < 2 {
		t.Errorf("want multiple llm calls for oversized payload, got %d", c.calls)
	}
	// 核心不变量：不管素材多大，每一次调用的输入都必须在预算内
	if c.maxTokens > int(float64(in.CompactBudget)*1.1) {
		t.Errorf("a compaction call used %d tokens, over budget %d",
			c.maxTokens, in.CompactBudget)
	}
	t.Logf("calls=%d maxInputTokens=%d budget=%d", c.calls, c.maxTokens, in.CompactBudget)
}

// TestCompactOnce_SingleCallWhenFits 素材放得下时只调一次，不无谓分块。
func TestCompactOnce_SingleCallWhenFits(t *testing.T) {
	c := &fakeCompactClient{}
	in := chunkInput(c, 100000)
	segs := []historySegment{bigSeg("t1", 500)}
	if _, err := compactOnce(context.Background(), in, nil, segs, 500); err != nil {
		t.Fatalf("compactOnce failed: %v", err)
	}
	if c.calls != 1 {
		t.Errorf("want exactly 1 call when payload fits, got %d", c.calls)
	}
}

// TestCompactOnce_ShrinkingWindowIsTheRealScenario 复现用户提出的场景：
// 历史是在大窗口模型下攒的，切成小窗口模型后，压缩调用自身也会超限。
func TestCompactOnce_ShrinkingWindowIsTheRealScenario(t *testing.T) {
	// 小窗口：上限 32K → 压缩调用预算 0.6*32768 ≈ 19.6K
	th := contextmgr.NewThresholds(32768)
	c := &fakeCompactClient{}
	in := chunkInput(c, th.CompactCall)

	// 旧摘要按大窗口（1M）生成过，体量可达 100K token，
	// 换成 32K 窗口后它自己就超了新预算 —— 也必须能切。
	prev := &model.ContextSummary{}
	var sb strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&sb, "早期工作记录 %d：完成了某个模块的改造，文件路径 src/x%d.go。\n", i, i)
	}
	prev.Summary = sb.String()

	segs := []historySegment{bigSeg("t1", 20000), bigSeg("t2", 20000)}

	out, err := compactOnce(context.Background(), in, prev, segs, th.Summary)
	if err != nil {
		t.Fatalf("compactOnce failed: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("empty result")
	}
	if c.calls < 2 {
		t.Errorf("want multiple calls under a small window, got %d", c.calls)
	}
	t.Logf("small-window compaction: calls=%d maxInputChars=%d maxInputTokens=%d budget=%d summary=%d",
		c.calls, c.maxInputSeen, c.maxTokens, th.CompactCall, th.Summary)
	// 硬约束：每次压缩调用的输入都必须落在预算内（含 system prompt 与指令）
	if c.maxTokens > int(float64(th.CompactCall)*1.1) {
		t.Errorf("a compaction call used %d tokens, over budget %d", c.maxTokens, th.CompactCall)
	}
}

// TestCompactOnce_CallsAreBounded 极端素材量下调用次数必须有界，
// 否则一次用户请求会变成上百次压缩调用。
func TestCompactOnce_CallsAreBounded(t *testing.T) {
	c := &fakeCompactClient{}
	in := chunkInput(c, 1500)
	segs := make([]historySegment, 0, 40)
	for i := 0; i < 40; i++ {
		segs = append(segs, bigSeg(fmt.Sprintf("t%d", i), 3000))
	}
	out, err := compactOnce(context.Background(), in, nil, segs, 400)
	if err != nil {
		t.Fatalf("compactOnce failed: %v", err)
	}
	if c.calls > maxCompactLLMCalls {
		t.Errorf("llm calls %d exceeds hard cap %d", c.calls, maxCompactLLMCalls)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("empty result")
	}
	t.Logf("40 oversized segments -> %d calls", c.calls)
}

// TestCompactOnce_FailurePropagates 任一批失败必须整体失败。
// 部分成功会导致「摘要丢了中间一段历史」，比不压缩更糟。
func TestCompactOnce_FailurePropagates(t *testing.T) {
	c := &fakeCompactClient{failAt: 2}
	in := chunkInput(c, 3000)
	segs := []historySegment{
		bigSeg("t1", 4000), bigSeg("t2", 4000),
		bigSeg("t3", 4000), bigSeg("t4", 4000),
	}
	if _, err := compactOnce(context.Background(), in, nil, segs, 500); err == nil {
		t.Fatal("want error when one batch fails, got nil")
	}
}

// TestCompactOnce_NoClient 客户端缺失时必须报错，不能静默产出空摘要。
func TestCompactOnce_NoClient(t *testing.T) {
	in := chunkInput(&fakeCompactClient{}, 3000)
	in.Client = nil
	if _, err := compactOnce(context.Background(), in, nil,
		[]historySegment{bigSeg("t1", 500)}, 500); err == nil {
		t.Fatal("want error when client is nil")
	}
}

// TestCompactOnce_ResultWithinBudget 最终摘要必须受预算约束 ——
// 调用次数用尽时走的是「拼接 + 硬截断」路径，同样不能超。
func TestCompactOnce_ResultWithinBudget(t *testing.T) {
	c := &fakeCompactClient{}
	in := chunkInput(c, 1200)
	// 制造大量批次，触发调用次数上限
	segs := make([]historySegment, 0, 60)
	for i := 0; i < 60; i++ {
		segs = append(segs, bigSeg(fmt.Sprintf("t%d", i), 2000))
	}
	budget := 300
	out, err := compactOnce(context.Background(), in, nil, segs, budget)
	if err != nil {
		t.Fatalf("compactOnce failed: %v", err)
	}
	cnt := contextmgr.GetCounter()
	if n := cnt.CountText(out); n > budget*3 {
		// 容忍头部标题的固定开销，但不能是数量级失控
		t.Errorf("summary too large: %d tokens (budget %d)", n, budget)
	}
	t.Logf("budget=%d resultTokens=%d calls=%d", budget, cnt.CountText(out), c.calls)
}

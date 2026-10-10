package actuator

import (
	"context"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/schedule/container"
)

// TestContextCompactTurnType 压缩 turn 的类型语义。
//
// 两条都要守住：名字决定调试页显示什么，IsLLMCall 决定它会不会被
// 误当成「agent 这一轮调过模型」—— 后者会让幂等跳过逻辑吞掉真正
// 的 functional_call（压缩发生在组装阶段，正好夹在两次调用之间）。
func TestContextCompactTurnType(t *testing.T) {
	if got := model.TurnTypeContextCompact.String(); got != "context_compact" {
		t.Errorf("String() = %q, want context_compact", got)
	}
	if model.TurnTypeContextCompact.IsLLMCall() {
		t.Errorf("IsLLMCall() = true, want false（压缩不是 agent 循环里的一轮）")
	}
	if !model.TurnTypeFunctionalCall.IsLLMCall() {
		t.Errorf("functional_call 仍应算 LLM 调用")
	}
}

// TestCompactTurnExcludedFromHistory 压缩 turn 不得进入 agent 对话历史。
//
// 它是压缩动作自身的留痕，不是对话内容；进了历史就等于把「请压缩以下
// 内容」这类指令当成模型说过的话回放回去。
func TestCompactTurnExcludedFromHistory(t *testing.T) {
	turn := &model.TaskTurn{
		TurnType: model.TurnTypeContextCompact,
		Status:   model.TurnStatusSuccess,
		Output:   `{"text":"【此前对话的历史工作记录（已压缩）】"}`,
	}
	if _, ok := turnToMessage(turn); ok {
		t.Errorf("压缩 turn 被当成了历史消息")
	}
}

// TestCompactTurnSnapshot 压缩调用的输入快照结构。
//
// 对齐 model.TurnLLMInput 的约定：system 单列不重复进 messages，
// 素材作为一条 user 消息，关思考链的 extra_body 要能看见 ——
// 否则调试时无法确认「这次压缩到底有没有关掉深度思考」。
func TestCompactTurnSnapshot(t *testing.T) {
	in := compactInput{
		Client: &fakeCompactClient{},
		Temp:   &model.TaskTemp{Task: model.Task{TaskID: "t1", ChatID: "c1"}},
		CompactExtraBody: map[string]interface{}{
			"reasoning_effort": "none",
		},
	}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: compactSystemPrompt},
		{Role: llm.RoleUser, Content: "素材正文…" + "压缩指令"},
	}
	opts := llm.DefaultOptions()
	opts.Temperature = 0.2

	snap := compactTurnSnapshot(in, 2, msgs, opts)

	if snap.SystemPrompt != compactSystemPrompt {
		t.Errorf("SystemPrompt 未单独落列：%q", snap.SystemPrompt)
	}
	if len(snap.Messages) != 1 {
		t.Fatalf("messages = %d 条, want 1（system 不该重复进 messages）", len(snap.Messages))
	}
	if snap.Messages[0].Role != "user" {
		t.Errorf("role = %q, want user", snap.Messages[0].Role)
	}
	if !strings.Contains(snap.Messages[0].Content, "素材正文") {
		t.Errorf("素材正文丢失：%q", snap.Messages[0].Content)
	}
	if snap.Options["extra_body"] == nil {
		t.Errorf("extra_body 未落快照，无法确认关思考链是否下发")
	}
	// 压缩内部调用序号记在 Options 里（Round 已改为记 agent 循环轮次）
	if got, ok := snap.Options["compact_call_seq"].(int); !ok || got != 2 {
		t.Errorf("compact_call_seq = %v, want int 2", snap.Options["compact_call_seq"])
	}
	if snap.Model != "fake" {
		t.Errorf("model = %q, want fake", snap.Model)
	}
}

// TestCompactLoopRoundFollowsHint 压缩 turn 的轮次必须与 agent 循环同源。
//
// 有 hint 时取 hint.ToolCallRound —— 与同轮 functional_call / tool_call
// turn 的取值路径完全一致。否则调试页时间线上轮次会从 6 跳回 1 再跳回 7。
// （无 hint 分支走 DB 计数，只在 dao.Ready 后才会被调用，单测不覆盖。）
func TestCompactLoopRoundFollowsHint(t *testing.T) {
	const taskID = "compact-round-hint-test"
	store := container.GetTransitionStore()
	store.SetHint(taskID, &model.StageTransitionHint{ToolCallRound: 7})
	defer store.ClearHint(taskID)

	if got := compactLoopRound(context.Background(), taskID, model.PhaseExecuting); got != 7 {
		t.Errorf("compactLoopRound = %d, want 7（hint.ToolCallRound）", got)
	}
}

// TestBeginCompactTurnWithoutDB 无 DB 时留痕必须静默跳过。
//
// 单测环境没有数据库，若这里 panic，所有压缩测试都会挂。
func TestBeginCompactTurnWithoutDB(t *testing.T) {
	in := compactInput{
		Client: &fakeCompactClient{},
		Temp:   &model.TaskTemp{Task: model.Task{TaskID: "t1", ChatID: "c1"}},
	}
	ctx := context.Background()
	turn := beginCompactTurn(ctx, in, 1, nil, llm.DefaultOptions())
	if turn != nil {
		t.Errorf("无 DB 时应返回 nil，实际拿到 %+v", turn)
	}
	// 结束态的两个函数也必须能安全吃下 nil turn
	finishCompactTurn(ctx, nil, "", llm.Usage{})
	failCompactTurn(ctx, nil, nil)
}

package actuator

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestIsContextCanceled 验证 context 取消的判定能穿透多层 %w 包装，
// 且不会把普通错误 / 超时误判成取消。
//
// 这是「context canceled 不改变节点/子任务状态、静默重排等待恢复」这一
// 修复的关键判据：只有真正的取消（errors.Is(err, context.Canceled)）才走
// 静默重排分支，网络抖动 / 网关 5xx / 超时等真实失败仍走原来的失败路径。
func TestIsContextCanceled(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"裸取消", context.Canceled, true},
		{"azure http 包装", fmt.Errorf("azure http call: %w", context.Canceled), true},
		{"多层包装", fmt.Errorf("func-call-actuator: llm chat failed: %w",
			fmt.Errorf("azure http call: %w", context.Canceled)), true},
		{"普通错误", errors.New("connection refused"), false},
		{"超时不是取消", context.DeadlineExceeded, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isContextCanceled(c.err); got != c.want {
				t.Fatalf("isContextCanceled(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestContextCanceledErrorUnwrap 验证 contextCanceledError 的错误链，
// 确保 errors.As 能在 funcCallImpl 顶层识别它并走延后重排分支。
func TestContextCanceledErrorUnwrap(t *testing.T) {
	inner := context.Canceled
	e := &contextCanceledError{Cause: inner}

	if !errors.Is(e, context.Canceled) {
		t.Fatalf("contextCanceledError 应能被 errors.Is 识别为 context.Canceled")
	}
	var ce *contextCanceledError
	if !errors.As(e, &ce) {
		t.Fatalf("contextCanceledError 应能被 errors.As 识别")
	}
	if ce.Cause != inner {
		t.Fatalf("Cause 丢失: %v", ce.Cause)
	}
}

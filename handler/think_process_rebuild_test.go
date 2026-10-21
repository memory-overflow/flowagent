package handler

import (
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

// TestShouldRebuildThinkProcess 锁定「失败重试残留过期快照」的修复分支：
//   - running 任务：即使 task_output 已有非空 think_process（失败收口时写下的
//     旧快照），也必须重建 —— 否则前端一直卡在旧快照，看不到后续实时进展。
//   - 终态任务：已有非空 think_process 则不重建（actuator/容器写的是最完整一版）。
func TestShouldRebuildThinkProcess(t *testing.T) {
	withProcess := map[string]interface{}{
		"agent_output": model.AgentOutputData{
			ThinkProcess: []string{"#1 任务规划完成", "#2 模型调用失败：xxx"},
		},
	}
	empty := map[string]interface{}{}

	cases := []struct {
		name   string
		status model.TaskStatus
		detail map[string]interface{}
		want   bool
	}{
		{"running 已有快照必须重建", model.TaskStatusRunning, withProcess, true},
		{"running 无快照重建", model.TaskStatusRunning, empty, true},
		{"queued 已有快照必须重建", model.TaskStatusQueued, withProcess, true},
		{"completed 已有快照不重建", model.TaskStatusCompleted, withProcess, false},
		{"failed 已有快照不重建", model.TaskStatusFailed, withProcess, false},
		{"stopped 已有快照不重建", model.TaskStatusStopped, withProcess, false},
		{"partial_failed 已有快照不重建", model.TaskStatusPartialFailed, withProcess, false},
		{"completed 无快照重建", model.TaskStatusCompleted, empty, true},
		{"agent_output 类型不符重建", model.TaskStatusCompleted, map[string]interface{}{"agent_output": "not-an-output"}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := &model.Task{Status: c.status}
			if got := shouldRebuildThinkProcess(task, c.detail); got != c.want {
				t.Errorf("shouldRebuildThinkProcess(%v) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

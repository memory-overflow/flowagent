// Package schedule — 基于 light-task-scheduler 的 Stage 流转任务调度系统
//
// 架构参考 https://github.com/memory-overflow/light-task-scheduler 和
// weaver-backend/schedule 的模式：
//
//   - 每个 stage 运行一个独立的 light-task-scheduler 实例（MakeScheduler）
//   - container/ 下的 StageTaskContainer 实现 framework.TaskContainer
//   - actuator/ 下的 XXXActuator 实现 framework.TaskActuator
//   - 状态流转由model.TaskStage.NextStage(hint) 状态机动态决策
//
// 动态状态流转设计：
//  1. 容器只绑定自己负责扫描的 wait 阶段（scanWaitStage）
//  2. 执行器执行完毕后，通过 container.GetTransitionStore() 写入决策 hint
//  3. 容器在 ToRunningStatus / ToSuccessStatus 中读取 hint，
//     调用 currentStage.NextStage(hint) 计算下一阶段
//  4. 任何阶段失败 → ToFailedStatus 统一置为 TaskStatusFailed 终态
//
// 执行路径（由状态机自动分支）：
//
//	functional_call → [tool_call → functional_call]×N → finished
//
// 规划期与执行期共享 functional_call 阶段，由 Task.Phase 区分：
// 规划期（phase=planning）模型可调工具调研并最终 submit_plan，
// 提交后切到执行期（phase=executing）继续 agent loop 产出最终答案。
package schedule

import "github.com/memory-overflow/flowagent/model"

// 重新导出 model 包中的 TaskStage 类型和常量，方便外部调用方使用。
type TaskStage = model.TaskStage

const (
	StageNone                = model.StageNone
	StageFunctionalCallWait  = model.StageFunctionalCallWait
	StageFunctionalCallDoing = model.StageFunctionalCallDoing
	StageToolCallWait        = model.StageToolCallWait
	StageToolCallDoing       = model.StageToolCallDoing
	StageFinished            = model.StageFinished
)

// StageTransitionHint 重新导出流转决策类型
type StageTransitionHint = model.StageTransitionHint

// DefaultMaxToolCallRound 默认最大工具调用轮次
const DefaultMaxToolCallRound = model.DefaultMaxToolCallRound

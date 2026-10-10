package dao

import (
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

// ============================================================
// collectActiveDescendants — 停止级联的子树展开
// ============================================================

// fakeTaskTree 用邻接表构造一棵任务树，供 collectActiveDescendants 测试取数。
type fakeTaskTree map[string][]*model.Task

func (t fakeTaskTree) fetch(parentID string) ([]*model.Task, error) {
	return t[parentID], nil
}

func mkTask(id, parentID string, status model.TaskStatus) *model.Task {
	return &model.Task{TaskID: id, ParentTaskID: parentID, Status: status}
}

func ids(tasks []*model.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.TaskID)
	}
	return out
}

func TestCollectActiveDescendants_CoversGrandchildren(t *testing.T) {
	// 真实链路：工作流任务 → 节点子任务 → deep_search 子任务（孙层）。
	// 只扫一层会漏掉孙层，表现就是「工作流停了，搜索还在跑」。
	tree := fakeTaskTree{
		"wf": {
			mkTask("node-1", "wf", model.TaskStatusRunning),
			mkTask("node-2", "wf", model.TaskStatusCompleted),
		},
		"node-1": {mkTask("ds-1", "node-1", model.TaskStatusRunning)},
		"node-2": {mkTask("ds-2", "node-2", model.TaskStatusQueued)},
	}

	got := ids(mustCollect(t, tree.fetch, "wf"))
	want := map[string]bool{"node-1": true, "ds-1": true, "ds-2": true}
	if len(got) != len(want) {
		t.Fatalf("collect = %v, want exactly %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected task %s in result %v", id, got)
		}
	}
}

func TestCollectActiveDescendants_SkipsTerminal(t *testing.T) {
	tree := fakeTaskTree{
		"root": {
			mkTask("done", "root", model.TaskStatusCompleted),
			mkTask("stopped", "root", model.TaskStatusStopped),
			mkTask("failed", "root", model.TaskStatusFailed),
			mkTask("live", "root", model.TaskStatusRunning),
		},
	}
	got := ids(mustCollect(t, tree.fetch, "root"))
	if len(got) != 1 || got[0] != "live" {
		t.Fatalf("collect = %v, want [live]", got)
	}
}

func TestCollectActiveDescendants_DescendsThroughTerminal(t *testing.T) {
	// 已完成的节点子任务仍可能遗留一个还在跑的 deep_search 孙任务，
	// 不能因为父节点已终态就停止向下展开。
	tree := fakeTaskTree{
		"root": {mkTask("node", "root", model.TaskStatusCompleted)},
		"node": {mkTask("ds", "node", model.TaskStatusRunning)},
	}
	got := ids(mustCollect(t, tree.fetch, "root"))
	if len(got) != 1 || got[0] != "ds" {
		t.Fatalf("collect = %v, want [ds]", got)
	}
}

func TestCollectActiveDescendants_CycleSafe(t *testing.T) {
	// 脏数据成环（a → b → a）：靠 seen 去重掐断，不能死循环。
	// 根任务自身不算后代，回指到根的那一跳被去重跳过，结果只含 b。
	tree := fakeTaskTree{
		"a": {mkTask("b", "a", model.TaskStatusRunning)},
		"b": {mkTask("a", "b", model.TaskStatusRunning)},
	}
	got := ids(mustCollect(t, tree.fetch, "a"))
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("collect = %v, want [b]", got)
	}
}

func mustCollect(
	t *testing.T, fetch func(string) ([]*model.Task, error), root string,
) []*model.Task {
	t.Helper()
	got, err := collectActiveDescendants(fetch, root)
	if err != nil {
		t.Fatalf("collectActiveDescendants: %v", err)
	}
	return got
}

// ============================================================
// 启动自愈：孤儿任务判定（orphanTaskIDs）
// ============================================================

func TestOrphanTaskIDs_GrandchildWithStoppedParent(t *testing.T) {
	// 真实链路：工作流(stopped) → 节点子任务(stopped) → 搜索孙任务(running)
	nodes := []taskNode{
		{TaskID: "wf", ParentTaskID: "", Status: model.TaskStatusStopped},
		{TaskID: "node", ParentTaskID: "wf", Status: model.TaskStatusStopped},
		{TaskID: "search", ParentTaskID: "node", Status: model.TaskStatusRunning},
	}
	got := orphanTaskIDs(nodes)
	if len(got) != 1 || got[0] != "search" {
		t.Fatalf("orphanTaskIDs = %v, want [search]", got)
	}
}

func TestOrphanTaskIDs_ActiveChainNotOrphan(t *testing.T) {
	// 全链路都在跑：正常存活，一个都不该被收口
	nodes := []taskNode{
		{TaskID: "wf", ParentTaskID: "", Status: model.TaskStatusRunning},
		{TaskID: "node", ParentTaskID: "wf", Status: model.TaskStatusRunning},
		{TaskID: "search", ParentTaskID: "node", Status: model.TaskStatusRunning},
		{TaskID: "q", ParentTaskID: "node", Status: model.TaskStatusQueued},
	}
	if got := orphanTaskIDs(nodes); len(got) != 0 {
		t.Fatalf("orphanTaskIDs = %v, want empty", got)
	}
}

func TestOrphanTaskIDs_CompletedAncestorCounts(t *testing.T) {
	// 父任务正常完成（不是 stopped）也属于终态：
	// 父都跑完了还挂在 running 的子任务必然是残留。
	nodes := []taskNode{
		{TaskID: "p", ParentTaskID: "", Status: model.TaskStatusCompleted},
		{TaskID: "c", ParentTaskID: "p", Status: model.TaskStatusRunning},
	}
	if got := orphanTaskIDs(nodes); len(got) != 1 || got[0] != "c" {
		t.Fatalf("orphanTaskIDs = %v, want [c]", got)
	}
}

func TestOrphanTaskIDs_AbandonedBranchNotTouched(t *testing.T) {
	// 工作流里被剪枝的分支节点（abandoned=7）不算终态：
	// 它不是任务级终态，收口会破坏「续接规划时重新激活」的语义。
	nodes := []taskNode{
		{TaskID: "wf", ParentTaskID: "", Status: model.TaskStatusRunning},
		{TaskID: "ab", ParentTaskID: "wf", Status: model.TaskStatusAbandoned},
		{TaskID: "c", ParentTaskID: "ab", Status: model.TaskStatusRunning},
	}
	if got := orphanTaskIDs(nodes); len(got) != 0 {
		t.Fatalf("orphanTaskIDs = %v, want empty (abandoned is not terminal)", got)
	}
}

func TestOrphanTaskIDs_MissingParentIgnored(t *testing.T) {
	// 父记录缺失（已物理清理 / 跨库）：不猜，交给看门狗，避免误杀
	nodes := []taskNode{{TaskID: "c", ParentTaskID: "gone", Status: model.TaskStatusRunning}}
	if got := orphanTaskIDs(nodes); len(got) != 0 {
		t.Fatalf("orphanTaskIDs = %v, want empty", got)
	}
}

func TestOrphanTaskIDs_CycleSafe(t *testing.T) {
	nodes := []taskNode{
		{TaskID: "a", ParentTaskID: "b", Status: model.TaskStatusRunning},
		{TaskID: "b", ParentTaskID: "a", Status: model.TaskStatusRunning},
	}
	if got := orphanTaskIDs(nodes); len(got) != 0 {
		t.Fatalf("orphanTaskIDs = %v, want empty (cycle must not hang)", got)
	}
}

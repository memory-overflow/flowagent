package handler

import (
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

// TestCollectBoundOutputFiles 收集绑定节点产物：按 path 去重、跳过空 Path 与 nil Output。
func TestCollectBoundOutputFiles(t *testing.T) {
	nodes := []model.WorkflowNodeData{
		{NodeID: "n1", Output: &model.AgentOutputData{OutputFiles: []model.FileInfo{
			{Name: "分析框架.md", Path: "outputs/wf/x/分析框架.md", URL: "/api/v1/files/download?chat_id=c&path=x"},
			{Name: "报告.md", Path: "outputs/报告.md", URL: "/api/v1/files/download?chat_id=c&path=report"},
		}}},
		{NodeID: "n2", Output: &model.AgentOutputData{OutputFiles: []model.FileInfo{
			// 与 n1 的「报告.md」path 相同 → 应去重
			{Name: "报告.md", Path: "outputs/报告.md", URL: "/api/v1/files/download?chat_id=c&path=report"},
			// 空 Path → 跳过（没有相对路径就拼不出预览链接）
			{Name: "无路径.md", Path: "", URL: "/api/v1/files/download?chat_id=c&path=orphan"},
		}}},
		// 无 Output 的节点 → 跳过
		{NodeID: "n3"},
	}

	got := collectBoundOutputFiles(nodes)
	if len(got) != 2 {
		t.Fatalf("want 2 files (去重后 + 跳过空Path), got %d: %+v", len(got), got)
	}
	byPath := map[string]model.FileInfo{}
	for _, f := range got {
		byPath[f.Path] = f
	}
	if _, ok := byPath["outputs/wf/x/分析框架.md"]; !ok {
		t.Errorf("缺少分析框架.md, got %+v", got)
	}
	if f, ok := byPath["outputs/报告.md"]; !ok || f.Path == "" {
		t.Errorf("报告.md 应保留且带 Path, got %+v", got)
	}
	for _, f := range got {
		if f.Path == "" {
			t.Errorf("产物 Path 不应为空: %+v", f)
		}
	}
}

// TestCollectBoundOutputFiles_Empty 全空场景返回空切片（不 panic）。
func TestCollectBoundOutputFiles_Empty(t *testing.T) {
	if got := collectBoundOutputFiles(nil); len(got) != 0 {
		t.Fatalf("nil nodes should return empty, got %+v", got)
	}
	if got := collectBoundOutputFiles([]model.WorkflowNodeData{{NodeID: "x"}}); len(got) != 0 {
		t.Fatalf("node without output should return empty, got %+v", got)
	}
}

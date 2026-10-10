package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestParentTurnIDList_CompatOldSingleValue 老数据只有单值 parent_turn_id，
// 数组化后仍需读得出，且首元素就是它（调试入口位置不跳）。
func TestParentTurnIDList_CompatOldSingleValue(t *testing.T) {
	d := &TaskInputData{ParentTurnID: 2684}
	if got := d.ParentTurnIDList(); !reflect.DeepEqual(got, []int64{2684}) {
		t.Fatalf("old single value → got %v, want [2684]", got)
	}
}

// TestParentTurnIDList_MergeAndDedup 单值与数组共存时去重，单值恒为首个元素。
func TestParentTurnIDList_MergeAndDedup(t *testing.T) {
	d := &TaskInputData{ParentTurnID: 2684, ParentTurnIDs: []int64{2684, 2686, 0, 2684}}
	want := []int64{2684, 2686}
	if got := d.ParentTurnIDList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("merge → got %v, want %v", got, want)
	}
}

// TestParentTurnIDList_Empty 空输入返回空，handler 据此把子任务退到 unmapped 列表。
func TestParentTurnIDList_Empty(t *testing.T) {
	var nilData *TaskInputData
	if got := nilData.ParentTurnIDList(); got != nil {
		t.Fatalf("nil receiver → got %v, want nil", got)
	}
	if got := (&TaskInputData{}).ParentTurnIDList(); len(got) != 0 {
		t.Fatalf("empty struct → got %v, want empty", got)
	}
}

// TestAppendParentTurnID 追加语义：新增返回 true，已存在/零值返回 false（幂等）。
func TestAppendParentTurnID(t *testing.T) {
	d := &TaskInputData{ParentTurnID: 2684}

	if !d.AppendParentTurnID(2686) {
		t.Fatal("append new turn → want true")
	}
	if !reflect.DeepEqual(d.ParentTurnIDs, []int64{2686}) {
		t.Fatalf("ParentTurnIDs → got %v, want [2686]", d.ParentTurnIDs)
	}
	// 已存在（含单值里的那个）→ 不重复写
	if d.AppendParentTurnID(2686) || d.AppendParentTurnID(2684) {
		t.Fatal("append existing turn → want false")
	}
	if d.AppendParentTurnID(0) {
		t.Fatal("append zero → want false")
	}
	if got := d.ParentTurnIDList(); !reflect.DeepEqual(got, []int64{2684, 2686}) {
		t.Fatalf("list after appends → got %v, want [2684 2686]", got)
	}
}

// TestTaskInputData_JSONRoundTrip 新增字段的 JSON 序列化：omitempty 生效
// （老数据不写这两个字段），反序列化后可正确读回。
func TestTaskInputData_JSONRoundTrip(t *testing.T) {
	raw := `{"input_text":"hi","sub_task_kind":"deep_search","parent_turn_id":2684,"parent_turn_ids":[2684,2686],"deep_search_query":"q"}`
	var d TaskInputData
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.DeepSearchQuery != "q" {
		t.Fatalf("deep_search_query → got %q, want q", d.DeepSearchQuery)
	}
	if got := d.ParentTurnIDList(); !reflect.DeepEqual(got, []int64{2684, 2686}) {
		t.Fatalf("parent ids → got %v, want [2684 2686]", got)
	}

	// 无新字段的老 JSON 不应被写坏
	var old TaskInputData
	if err := json.Unmarshal([]byte(`{"input_text":"hi","parent_turn_id":7}`), &old); err != nil {
		t.Fatalf("unmarshal old: %v", err)
	}
	if old.ParentTurnIDs != nil || old.DeepSearchQuery != "" {
		t.Fatalf("old json → got ids=%v query=%q, want nil/\"\"", old.ParentTurnIDs, old.DeepSearchQuery)
	}
}

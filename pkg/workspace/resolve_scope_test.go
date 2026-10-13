package workspace

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// 只读工具放行项目根内的绝对路径；项目根外仍拒绝；写操作（resolveIn）
// 无论是否在项目根内都拒绝绝对路径，维持工作区沙箱。
func TestResolveInScopeProjectRoot(t *testing.T) {
	root := t.TempDir()                    // 项目根
	ws := filepath.Join(root, "workspace") // 工作区在项目根下
	wc := &Context{WorkspaceDir: ws, ProjectRoot: root}

	// 项目根内绝对路径 → 放行，返回项目根内绝对路径
	inRoot := filepath.Join(root, "skills", "pdfkit-py", "scripts")
	got, err := resolveInScope(wc, inRoot, true)
	if err != nil || got != inRoot {
		t.Errorf("项目根内绝对路径应放行: got %q err %v", got, err)
	}
	// 项目根本身也放行
	if got, err = resolveInScope(wc, root, true); err != nil || got != root {
		t.Errorf("项目根本身应放行: got %q err %v", got, err)
	}
	// 项目根外绝对路径 → 仍拒绝
	if _, err = resolveInScope(wc, filepath.Join(filepath.Dir(root), "elsewhere"), true); err == nil {
		t.Errorf("项目根外绝对路径应拒绝")
	}
	// 相对路径行为不变 → 解析到工作区内
	if got, err = resolveInScope(wc, "a.txt", true); err != nil || got != filepath.Join(ws, "a.txt") {
		t.Errorf("相对路径应解析到工作区: got %q err %v", got, err)
	}
	// 写操作 resolveIn → 即使项目根内也拒绝绝对路径
	if _, err = resolveIn(wc, inRoot); err == nil || !strings.Contains(fmt.Sprint(err), "拒绝绝对路径") {
		t.Errorf("写操作应拒绝绝对路径: err %v", err)
	}
}

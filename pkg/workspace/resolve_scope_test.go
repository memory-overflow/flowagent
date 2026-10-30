package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveInScopeAllowsAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	wc := &Context{WorkspaceDir: ws, ProjectRoot: root}

	outside := filepath.Join(t.TempDir(), "elsewhere", "file.txt")
	for _, resolve := range []struct {
		name string
		fn   func(*Context, string) (string, error)
	}{{"write", resolveIn}, {"read", resolveReadable}} {
		t.Run(resolve.name, func(t *testing.T) {
			got, err := resolve.fn(wc, outside)
			if err != nil || got != filepath.Clean(outside) {
				t.Fatalf("系统任意绝对路径应放行: got %q err %v", got, err)
			}
		})
	}

	got, err := resolveIn(wc, "a.txt")
	if err != nil || got != filepath.Join(ws, "a.txt") {
		t.Fatalf("相对路径应保留工作区语义: got %q err %v", got, err)
	}
	if _, err := resolveIn(wc, "../outside"); err == nil {
		t.Fatal("相对路径仍应阻止 .. 越界")
	}
}

func TestResolveInScopeExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wc := &Context{WorkspaceDir: t.TempDir()}
	got, err := resolveIn(wc, "~/flowagent-test.txt")
	if err != nil || got != filepath.Join(home, "flowagent-test.txt") {
		t.Fatalf("~ 路径展开错误: got %q err %v", got, err)
	}
	if _, err := resolveIn(wc, "~other/file"); err == nil {
		t.Fatal("不应支持其他用户的 ~user 路径")
	}
}

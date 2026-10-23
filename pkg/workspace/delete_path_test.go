package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeletePathFileAndDirectory(t *testing.T) {
	wc := &Context{WorkspaceDir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(wc.WorkspaceDir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(wc.WorkspaceDir, "nested")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := deletePathTool()
	for _, path := range []string{"file.txt", "nested"} {
		if _, err := tool.Handler(context.Background(), wc, map[string]interface{}{"path": path}); err != nil {
			t.Fatalf("删除 %s 失败: %v", path, err)
		}
		if _, err := os.Stat(filepath.Join(wc.WorkspaceDir, path)); !os.IsNotExist(err) {
			t.Fatalf("%s 应已删除，stat err=%v", path, err)
		}
	}
}

func TestDeletePathAbsoluteOutsideWorkspace(t *testing.T) {
	wc := &Context{WorkspaceDir: t.TempDir()}
	target := filepath.Join("/tmp", fmt.Sprintf("flowagent-delete-test-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := deletePathTool().Handler(context.Background(), wc, map[string]interface{}{"path": target}); err != nil {
		t.Fatalf("应允许删除工作区外普通绝对路径: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("目标应已删除: %v", err)
	}
}

func TestDeletePathRejectsDangerousTarget(t *testing.T) {
	wc := &Context{WorkspaceDir: t.TempDir()}
	for _, target := range []string{"/", "/*", "~", "/etc/passwd"} {
		_, err := deletePathTool().Handler(context.Background(), wc, map[string]interface{}{"path": target})
		if err == nil || !strings.Contains(err.Error(), "危险目标") {
			t.Fatalf("目标 %q 应被安全策略拦截，err=%v", target, err)
		}
	}
}

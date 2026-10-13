package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInitial 在工作区写一个初始文件（绕过 write_file，直接落盘）。
func writeInitial(t *testing.T, wc *Context, rel, content string) {
	t.Helper()
	abs := filepath.Join(wc.WorkspaceDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readBack(t *testing.T, wc *Context, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wc.WorkspaceDir, rel))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

// TestEditFileStrReplace 唯一匹配时精确替换，且返回改动上下文。
func TestEditFileStrReplace(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-edit")}
	writeInitial(t, wc, "a.txt", "hello\nworld\nfoo\n")
	run := editFileTool()

	out, err := run.Handler(context.Background(), wc, map[string]interface{}{
		"command": "str_replace", "path": "a.txt",
		"old_string": "world", "new_string": "WORLD",
	})
	if err != nil {
		t.Fatalf("str_replace 失败: %v", err)
	}
	if got := readBack(t, wc, "a.txt"); got != "hello\nWORLD\nfoo\n" {
		t.Fatalf("内容不符: %q", got)
	}
	if !strings.Contains(out, "WORLD") {
		t.Fatalf("返回值应含改动上下文，got %q", out)
	}
}

// TestEditFileStrReplaceNotUnique 目标文本多次出现时报不唯一。
func TestEditFileStrReplaceNotUnique(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-edit2")}
	writeInitial(t, wc, "a.txt", "x\nx\n")
	run := editFileTool()

	_, err := run.Handler(context.Background(), wc, map[string]interface{}{
		"command": "str_replace", "path": "a.txt",
		"old_string": "x", "new_string": "y",
	})
	if err == nil || !strings.Contains(err.Error(), "不唯一") {
		t.Fatalf("应报不唯一错误，got %v", err)
	}
}

// TestEditFileStrReplaceNotFound 目标文本不存在时报未找到。
func TestEditFileStrReplaceNotFound(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-edit3")}
	writeInitial(t, wc, "a.txt", "hello\n")
	run := editFileTool()

	_, err := run.Handler(context.Background(), wc, map[string]interface{}{
		"command": "str_replace", "path": "a.txt",
		"old_string": "不存在", "new_string": "y",
	})
	if err == nil || !strings.Contains(err.Error(), "未在文件") {
		t.Fatalf("应报未找到错误，got %v", err)
	}
}

// TestEditFileInsert 在锚点之后插入新内容。
func TestEditFileInsert(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-edit4")}
	writeInitial(t, wc, "a.txt", "aaa\nbbb\n")
	run := editFileTool()

	_, err := run.Handler(context.Background(), wc, map[string]interface{}{
		"command": "insert", "path": "a.txt",
		"anchor": "aaa", "new_string": "\nXXX",
	})
	if err != nil {
		t.Fatalf("insert 失败: %v", err)
	}
	if got := readBack(t, wc, "a.txt"); got != "aaa\nXXX\nbbb\n" {
		t.Fatalf("内容不符: %q", got)
	}
}

// TestEditFileInsertAppend anchor 留空时追加到文件末尾。
func TestEditFileInsertAppend(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-edit5")}
	writeInitial(t, wc, "a.txt", "aaa\n")
	run := editFileTool()

	_, err := run.Handler(context.Background(), wc, map[string]interface{}{
		"command": "insert", "path": "a.txt",
		"anchor": "", "new_string": "bbb\n",
	})
	if err != nil {
		t.Fatalf("append 失败: %v", err)
	}
	if got := readBack(t, wc, "a.txt"); got != "aaa\nbbb\n" {
		t.Fatalf("内容不符: %q", got)
	}
}

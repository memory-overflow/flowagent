package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveSandbox 验证路径沙箱：正常路径放行，越界/绝对路径拒绝。
func TestResolveSandbox(t *testing.T) {
	m := NewManager("./testdata-ws")
	wsDir := m.Dir("task-1")

	cases := []struct {
		rel    string
		wantOK bool
	}{
		{"src/main.go", true},
		{"a/b/../c.txt", true},   // Clean 后仍在区内
		{".", true},              // 工作区根
		{"sub/dir/", true},       // 目录
		{"../escape.txt", false}, // 越界
		{"../../etc/passwd", false},
		{"/etc/passwd", false},    // 绝对路径
		{"~/home/x", false},       // 家目录写法
		{"C:\\Windows\\x", false}, // Windows 盘符
	}
	for _, c := range cases {
		_, err := m.Resolve(wsDir, c.rel)
		if c.wantOK && err != nil {
			t.Errorf("Resolve(%q) 期望放行，实际拒绝: %v", c.rel, err)
		}
		if !c.wantOK && err == nil {
			t.Errorf("Resolve(%q) 期望拒绝，实际放行", c.rel)
		}
	}
}

// TestWriteReadRoundtrip 验证 write_file → read_file 往返与工作区隔离。
func TestWriteReadRoundtrip(t *testing.T) {
	m := NewManager(t.TempDir())
	wsDir := m.Dir("wf-1")
	wc := &Context{WorkspaceDir: wsDir}
	ctx := context.Background()

	write := writeFileTool()
	if _, err := write.Handler(ctx, wc, map[string]interface{}{
		"path":    "out/hello.txt",
		"content": "hello workflow",
	}); err != nil {
		t.Fatalf("write_file 失败: %v", err)
	}

	read := readFileTool()
	out, err := read.Handler(ctx, wc, map[string]interface{}{"path": "out/hello.txt"})
	if err != nil {
		t.Fatalf("read_file 失败: %v", err)
	}
	if out != "hello workflow" {
		t.Fatalf("read_file 内容不符: %q", out)
	}

	// 文件必须落在 wsDir 之下
	full := filepath.Join(wsDir, "out", "hello.txt")
	if _, err := os.Stat(full); err != nil {
		t.Fatalf("文件未落到工作区内: %v", err)
	}
}

// TestWriteFileRejectsEscape 验证 write_file 拒绝越界路径（不写盘）。
func TestWriteFileRejectsEscape(t *testing.T) {
	m := NewManager(t.TempDir())
	wsDir := m.Dir("wf-2")
	wc := &Context{WorkspaceDir: wsDir}
	ctx := context.Background()

	write := writeFileTool()
	_, err := write.Handler(ctx, wc, map[string]interface{}{
		"path":    "../escape.txt",
		"content": "x",
	})
	if err == nil {
		t.Fatal("write_file 应拒绝越界路径，实际放行")
	}
}

// TestRunCommandDangerousBlocked 验证 run_command 拦截危险删除命令。
func TestRunCommandDangerousBlocked(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-3")}
	ctx := context.Background()

	run := runCommandTool()
	_, err := run.Handler(ctx, wc, map[string]interface{}{
		"cmd": "rm -rf /",
	})
	if err == nil {
		t.Fatal("run_command 应拦截 rm -rf /，实际放行")
	}
	if !strings.Contains(err.Error(), "安全策略拦截") {
		t.Fatalf("拦截信息不符: %v", err)
	}
}

// TestRunCommandInWorkspace 验证 run_command 在工作区内执行并返回 stdout。
func TestRunCommandInWorkspace(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-4")}
	ctx := context.Background()

	run := runCommandTool()
	out, err := run.Handler(ctx, wc, map[string]interface{}{
		"cmd": "echo ok && pwd",
	})
	if err != nil {
		t.Fatalf("run_command 失败: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("stdout 缺失: %q", out)
	}
}

// TestRunCommandOutputTruncated 验证 run_command 对超长 stdout 做源头截断，
// 避免几百 MB 输出灌进内存（回归 run_command 读大日志撑爆 DB 的问题）。
func TestRunCommandOutputTruncated(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-trunc")}
	ctx := context.Background()

	run := runCommandTool()
	// 输出 200KB 的 'x'，远超 maxCommandOutputBytes(64KB)
	out, err := run.Handler(ctx, wc, map[string]interface{}{
		"cmd": `awk 'BEGIN{for(i=0;i<200000;i++)printf "x"}'`,
	})
	if err != nil {
		t.Fatalf("run_command 失败: %v", err)
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("超长输出应带截断标注: %q", out[:min(len(out), 80)])
	}
	if len(out) < maxCommandOutputBytes || len(out) > maxCommandOutputBytes+128 {
		t.Fatalf("输出未被正确截断，长度 %d 不在预期区间 [%d, %d]", len(out), maxCommandOutputBytes, maxCommandOutputBytes+128)
	}
}

// TestGetCurrentTime 验证时间工具返回三要素。
func TestGetCurrentTime(t *testing.T) {
	tool := getCurrentTimeTool()
	out, err := tool.Handler(context.Background(), &Context{}, nil)
	if err != nil {
		t.Fatalf("get_current_time 失败: %v", err)
	}
	for _, want := range []string{"本地时间", "UTC 时间", "Unix 时间戳"} {
		if !strings.Contains(out, want) {
			t.Errorf("get_current_time 缺少 %q: %s", want, out)
		}
	}
}

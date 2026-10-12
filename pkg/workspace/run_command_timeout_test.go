package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestResolveCommandTimeout 超时参数解析：默认 60s、上限 15 分钟。
func TestResolveCommandTimeout(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
		want time.Duration
	}{
		{"未指定用默认", map[string]interface{}{}, DefaultTimeout},
		{"显式指定", map[string]interface{}{"timeout_seconds": 120}, 120 * time.Second},
		{"JSON 浮点", map[string]interface{}{"timeout_seconds": float64(90)}, 90 * time.Second},
		{"超上限被夹住", map[string]interface{}{"timeout_seconds": 86400}, maxCommandTimeout},
		{"非法值回落默认", map[string]interface{}{"timeout_seconds": "abc"}, DefaultTimeout},
		{"非正数回落默认", map[string]interface{}{"timeout_seconds": 0}, DefaultTimeout},
	}
	for _, c := range cases {
		if got := resolveCommandTimeout(c.args); got != c.want {
			t.Errorf("%s: resolveCommandTimeout = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestRunCommandTimeoutReturns 超时后必须及时返回 —— 不能等孙进程自己结束。
//
// 这是本次修复的核心回归点：修复前用 exec.CommandContext，只 kill 直接子进程
// sh，而 `sh -c "sleep 30 & wait"` fork 出的 sleep 存活并持有 stdout pipe 写端，
// cmd.Wait() 要等 pipe EOF，函数会一路阻塞到 sleep 结束（实测 2s 超时 15s 才返回）。
func TestRunCommandTimeoutReturns(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-timeout")}
	run := runCommandTool()

	start := time.Now()
	_, err := run.Handler(context.Background(), wc, map[string]interface{}{
		"cmd":             `sleep 30 & wait`,
		"timeout_seconds": 2,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("超时应返回错误，实际返回成功")
	}
	if !strings.Contains(err.Error(), "被强制终止") {
		t.Fatalf("错误信息应说明被强制终止，got %v", err)
	}
	// 2s 超时 + 少量调度开销；若仍等到 30s 说明 pipe 阻塞问题没修好。
	if elapsed > 10*time.Second {
		t.Fatalf("超时后应立刻返回，实际耗时 %v（说明仍在等孙进程/pipe）", elapsed)
	}
}

// TestRunCommandTimeoutKillsChildren 超时后孙进程必须一起死，不能变孤儿继续跑。
func TestRunCommandTimeoutKillsChildren(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-orphan")}
	run := runCommandTool()

	// 用一个独特的 sleep 秒数当标记，便于事后 pgrep 确认进程是否残留。
	marker := "sleep 47"
	_, err := run.Handler(context.Background(), wc, map[string]interface{}{
		"cmd":             marker + " & wait",
		"timeout_seconds": 1,
	})
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	// 给 kill 一点落地时间
	time.Sleep(500 * time.Millisecond)

	// 用锚定 ^...$ 精确匹配「cmdline 恰好是 sleep 47」的孙进程：
	// 若用裸子串匹配，pgrep -f 会误匹配到「正在执行本检查的 sh -c 进程自己」
	// （它的 cmdline 里也含 "sleep 47"），Linux(dash) 下会一直算出 1 个残留。
	count := exec.Command("sh", "-c", fmt.Sprintf(`pgrep -f '^%s$' | wc -l`, marker))
	out, pErr := count.Output()
	if pErr != nil {
		t.Skipf("当前环境无法用 pgrep 校验残留进程: %v", pErr)
	}
	if n := strings.TrimSpace(string(out)); n != "0" {
		t.Fatalf("超时后仍有 %s 个残留进程（孙进程未被杀死）", n)
	}
}

// TestRunCommandTimeoutOutputKept 超时时应带上终止前已产出的输出，便于模型诊断。
func TestRunCommandTimeoutOutputKept(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-timeout-out")}
	run := runCommandTool()

	_, err := run.Handler(context.Background(), wc, map[string]interface{}{
		"cmd":             `echo "progress-marker"; sleep 30 & wait`,
		"timeout_seconds": 1,
	})
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if !strings.Contains(err.Error(), "progress-marker") {
		t.Fatalf("超时错误应带上已产出的输出，got %v", err)
	}
}

// TestRunCommandLongTimeoutAllowed 显式放宽后，长命令应能跑完（不被默认 60s 截断）。
func TestRunCommandLongTimeoutAllowed(t *testing.T) {
	m := NewManager(t.TempDir())
	wc := &Context{WorkspaceDir: m.Dir("wf-long")}
	run := runCommandTool()

	start := time.Now()
	out, err := run.Handler(context.Background(), wc, map[string]interface{}{
		"cmd":             `sleep 3; echo done`,
		"timeout_seconds": 30,
	})
	if err != nil {
		t.Fatalf("放宽超时后 3s 的命令应正常完成，got err: %v", err)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("输出不符: %q", out)
	}
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Fatalf("命令应真的执行了 3s，实际 %v", elapsed)
	}
}

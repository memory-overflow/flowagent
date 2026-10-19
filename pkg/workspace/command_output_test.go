package workspace

import (
	"errors"
	"os/exec"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// decodeOutputBytes 在 Windows 下要把 GBK 输出的中文正确解回 UTF-8，
// 否则 run_command 返回里中文全是乱码；UTF-8 与纯 ASCII 则原样放行。
func TestDecodeOutputBytesGBK(t *testing.T) {
	const zh = "驱动器 C 中的卷没有标签"
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(zh))
	if err != nil {
		t.Fatalf("encode GBK: %v", err)
	}

	// Windows + GBK 字节 → 正确解码回中文
	if got := decodeOutputBytes("windows", gbk); got != zh {
		t.Errorf("windows GBK 解码 = %q, want %q", got, zh)
	}
	// Windows + 合法 UTF-8 → 原样
	if got := decodeOutputBytes("windows", []byte(zh)); got != zh {
		t.Errorf("windows UTF-8 应原样 = %q, want %q", got, zh)
	}
	// Windows + 纯 ASCII → 原样
	if got := decodeOutputBytes("windows", []byte("ok 123")); got != "ok 123" {
		t.Errorf("windows ASCII 应原样 = %q, want %q", got, "ok 123")
	}
	// 非 Windows → 永不解码（GBK 字节原样当字符串）
	if got := decodeOutputBytes("darwin", gbk); got == zh {
		t.Errorf("darwin 不应做 GBK 解码")
	}
}

// exitCode 从 *exec.ExitError 提取退出码；非退出错误返回 -1。
func TestExitCode(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Run(); err == nil {
		t.Fatal("exit 7 应返回错误")
	} else if got := exitCode(err); got != 7 {
		t.Errorf("exitCode = %d, want 7", got)
	}
	if got := exitCode(errors.New("not an exit error")); got != -1 {
		t.Errorf("非退出错误 exitCode = %d, want -1", got)
	}
}

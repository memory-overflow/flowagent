package logger

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRotatingWriterRollover 验证：超过阈值触发滚动、备份文件后移、最旧备份被删除。
func TestRotatingWriterRollover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	w, err := NewRotatingWriter(path, 10, 2) // 阈值 10 字节，保留 2 个备份
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}

	// 写 8 字节：未超 10，不滚动。
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("expected no .1 yet, got err=%v", err)
	}

	// 再写 8 字节：8+8=16>10，触发滚动，当前内容滚到 .1。
	if _, err := w.Write([]byte("ABCDEFGH")); err != nil {
		t.Fatalf("write2: %v", err)
	}
	b1, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read .1: %v", err)
	}
	if string(b1) != "12345678" {
		t.Fatalf(".1 content = %q, want %q", string(b1), "12345678")
	}

	// 继续触发第二次滚动：.1 -> .2，当前(ABCDEFGH) -> .1。
	if _, err := w.Write([]byte("WXYZ")); err != nil {
		t.Fatalf("write3: %v", err)
	}
	b2, err := os.ReadFile(path + ".2")
	if err != nil {
		t.Fatalf("read .2: %v", err)
	}
	if string(b2) != "12345678" {
		t.Fatalf(".2 content = %q, want %q", string(b2), "12345678")
	}
	b1Again, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read .1 again: %v", err)
	}
	if string(b1Again) != "ABCDEFGH" {
		t.Fatalf(".1 content = %q, want %q", string(b1Again), "ABCDEFGH")
	}

	// 第三次滚动：写 10 字节（4+10=14>10）。maxBackups=2，最旧的 .2 被删除，
	// 原 .1(ABCDEFGH) -> .2，当前(WXYZ) -> .1。
	if _, err := w.Write([]byte("1234567890")); err != nil {
		t.Fatalf("write4: %v", err)
	}
	b2Again, err := os.ReadFile(path + ".2")
	if err != nil {
		t.Fatalf("read .2 after 3rd roll: %v", err)
	}
	if string(b2Again) != "ABCDEFGH" {
		t.Fatalf(".2 content = %q, want %q", string(b2Again), "ABCDEFGH")
	}
	b1Again2, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read .1 after 3rd roll: %v", err)
	}
	if string(b1Again2) != "WXYZ" {
		t.Fatalf(".1 content = %q, want %q", string(b1Again2), "WXYZ")
	}
}

// TestParseLevel 验证级别解析。
func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug":   LevelDebug,
		"DEBUG":   LevelDebug,
		"info":    LevelInfo,
		"":        LevelInfo,
		"warn":    LevelWarn,
		"warning": LevelWarn,
		"error":   LevelError,
		"bogus":   LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

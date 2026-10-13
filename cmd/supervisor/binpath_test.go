package main

import (
	"path/filepath"
	"testing"
)

// binPathFor 的 .exe 后缀规则：Windows 上必须补 .exe（Go 编译产物带扩展名，
// exec.Command 对含路径分隔符的参数不走 LookPath、不自动补），Unix 不带。
func TestBinPathExeSuffix(t *testing.T) {
	if got := binPathFor("windows", "bin", "agent-server"); got != filepath.Join("bin", "agent-server.exe") {
		t.Errorf("windows binPath = %q, want agent-server.exe", got)
	}
	if got := binPathFor("windows", "bin", "agent-scheduler"); got != filepath.Join("bin", "agent-scheduler.exe") {
		t.Errorf("windows binPath = %q, want agent-scheduler.exe", got)
	}
	if got := binPathFor("darwin", "bin", "agent-server"); got != filepath.Join("bin", "agent-server") {
		t.Errorf("darwin binPath = %q, want agent-server（无后缀）", got)
	}
	if got := binPathFor("linux", "bin", "agent-server"); got != filepath.Join("bin", "agent-server") {
		t.Errorf("linux binPath = %q, want agent-server（无后缀）", got)
	}
}

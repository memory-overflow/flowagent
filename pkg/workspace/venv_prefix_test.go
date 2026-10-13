package workspace

import (
	"strings"
	"testing"
)

// venv 激活前缀的跨平台约束：
//   - 末尾绝不能是裸换行 —— cmd.exe 的命令行不接受 LF（换行只在 .bat 文件里
//     有效），LF 之后的用户命令会被 cmd /c 整个丢弃，表现为「执行成功、无
//     stdout 输出」而实际什么都没跑（Windows 线上真实踩坑）；
//   - 因此 Windows 分支必须以 "& " 连接（cmd 的顺序执行符），Unix 分支的
//     "\n" 在 sh -c 里多行合法、可以保留。
//
// 注意：本测试在任意平台跑都覆盖两个分支（直接查字符串形态，不依赖 GOOS）。
func TestVenvActivatePrefixFormat(t *testing.T) {
	const root = `C:\proj`
	win := venvActivateForWindows(root)
	if !strings.HasPrefix(win, "( if exist ") || !strings.Contains(win, "activate.bat") {
		t.Errorf("windows 前缀形态不符: %q", win)
	}
	if !strings.HasSuffix(win, " ) & ") {
		t.Errorf("windows 前缀必须以 \") & \" 结尾（顺序连接用户命令），got %q", win)
	}
	if strings.ContainsAny(win, "\n\r") {
		t.Errorf("windows 前缀不能含换行符（cmd 命令行不接受 LF）: %q", win)
	}

	unix := venvActivateForUnix("/proj")
	if !strings.HasPrefix(unix, "[ -f ") || !strings.HasSuffix(unix, "\n") {
		t.Errorf("unix 前缀形态不符: %q", unix)
	}
}

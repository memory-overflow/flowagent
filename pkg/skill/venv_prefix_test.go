package skill

import (
	"strings"
	"testing"
)

// venv 激活前缀的跨平台约束（skill 包）：
// Windows 分支末尾绝不能是换行 —— cmd.exe 命令行不接受 LF，换行之后的
// 用户命令会被 cmd /c 整个丢弃；必须以 "& " 顺序连接。Unix 分支的 "\n"
// 在 sh 里合法，可保留。
func TestVenvActivatePrefixWindowsNoNewline(t *testing.T) {
	win := venvActivatePrefixFor("windows")
	if !strings.HasSuffix(win, " ) & ") {
		t.Errorf("windows 前缀必须以 \") & \" 结尾，got %q", win)
	}
	if strings.ContainsAny(win, "\n\r") {
		t.Errorf("windows 前缀不能含换行符（cmd 命令行不接受 LF）: %q", win)
	}

	unix := venvActivatePrefixFor("darwin")
	if !strings.HasPrefix(unix, "[ -f ") || !strings.HasSuffix(unix, "\n") {
		t.Errorf("unix 前缀形态不符: %q", unix)
	}
}

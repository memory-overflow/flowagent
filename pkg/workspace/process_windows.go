//go:build windows

package workspace

import (
	"os/exec"
	"syscall"
)

// buildShellCmd 构造在 cmd.exe 里执行用户命令的子进程。
//
// 关键：cmd.exe 不按 CRT argv 规则解析参数 —— 它直接拿原始命令行解析。
// exec.Command("cmd", "/c", userCmd) 会让 Go 的 EscapeArg 把 userCmd 里的
// 引号转义成 \"，而 cmd.exe 把 \" 拆成「字面反斜杠 + 引号」，引号配对被
// 破坏：模型生成 `python3 "C:\path\a.py" ...` 这类带引号参数的命令时，
// python 收到的 argv 里残留字面引号 → 路径被当成相对路径拼上 cwd →
// 路径含非法字符 " → [Errno 22] Invalid argument。
// SysProcAttr.CmdLine 直接指定传给 CreateProcess 的完整命令行，绕过
// EscapeArg，引号原样到达 cmd，由 cmd 自己的 /c 引号规则处理。
func buildShellCmd(userCmd string) *exec.Cmd {
	ec := exec.Command("cmd")
	ec.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd /c ` + userCmd}
	return ec
}

// setProcessGroup Windows 没有进程组概念（作业对象是另一套机制），这里退化为空操作：
// 超时时只能 kill 直接子进程 sh，孙进程可能残留。
//
// 与 Unix 版的差异属于平台能力差异，不是逻辑分支 —— run_command 的超时
// 控制在两个平台上走同一条代码路径（Start + select + kill），
// 只是 Windows 上「杀得没那么干净」。
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup Windows 上退化为只 kill 直接子进程。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

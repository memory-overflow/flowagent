//go:build windows

package workspace

import "os/exec"

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

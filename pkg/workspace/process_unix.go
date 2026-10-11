//go:build !windows

package workspace

import (
	"os/exec"
	"syscall"
)

// setProcessGroup 让命令自成一个进程组（PGID = 子进程 PID）。
//
// 为什么必须：run_command 跑的是 `sh -c <cmd>`，sh 会把管道里的 find/head、
// 后台任务等 fork 成孙进程。超时时若只 kill sh，孙进程会变孤儿继续跑，
// 并且它们继承着 stdout 的 pipe 写端 —— Go 的 cmd.Wait() 要等 pipe 关闭才返回，
// 于是「超时」形同虚设，函数会一直阻塞到孙进程自己结束。
// （实测：ctx=2s + `sh -c "sleep 15 & wait"`，cmd.Run() 15s 才返回。）
//
// 独立进程组后，超时可以一次性 kill 整组，孙进程一起死，pipe 随即关闭。
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup 向整个进程组发 SIGKILL（负 PID 表示按 PGID 寻址）。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

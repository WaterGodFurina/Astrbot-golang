//go:build !windows

package pysdk

import (
	"os/exec"
	"syscall"
)

// setupPipCmd 让 pip 及其子进程（build-isolation 的编译子进程）处于独立进程组，
// 便于超时 kill 整组，避免留下孤儿编译进程。
func setupPipCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killPipCmd 超时 kill 整个 pip 进程组（负 PID 表示组；SIGKILL 强杀）。
func killPipCmd(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

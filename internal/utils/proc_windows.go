//go:build windows

package utils

import (
	"os/exec"
	"strconv"
)

// Windows 没有进程组/会话概念：DetachProcess 与 SetProcessGroup 为 no-op；
// 杀进程/进程树用 taskkill（/F 强制，/T 连同子进程一起结束）。

// DetachProcess is a no-op on Windows.
func DetachProcess(cmd *exec.Cmd) {}

// SetProcessGroup is a no-op on Windows (no process groups).
func SetProcessGroup(cmd *exec.Cmd) {}

// KillProcess forcibly terminates a process by pid.
func KillProcess(pid int) error {
	return exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// ForceKillProcess forcibly terminates a process by pid.
func ForceKillProcess(pid int) error {
	return exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// KillProcessGroup terminates the process and all its descendants (taskkill /T).
func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}

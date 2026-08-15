//go:build !windows

package utils

import (
	"os/exec"
	"syscall"
)

// DetachProcess runs cmd in a new session (used for restarts so the new
// instance survives the old one exiting).
func DetachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// SetProcessGroup runs cmd in its own process group so a timeout can kill the
// whole group (sh -c "a; b &" leaves grandchildren running otherwise).
func SetProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillProcess sends a termination signal to a process by pid.
func KillProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// ForceKillProcess sends an uncatchable kill signal to a process by pid.
func ForceKillProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}

// KillProcessGroup kills the whole process group of cmd (grandchildren too).
func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

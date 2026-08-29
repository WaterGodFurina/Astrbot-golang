//go:build windows

package pysdk

import "os/exec"

// setupPipCmd Windows 无 POSIX 进程组，无需额外设置。
func setupPipCmd(cmd *exec.Cmd) {}

// killPipCmd Windows 上仅 kill 直接子进程（CommandContext 已处理）。
func killPipCmd(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

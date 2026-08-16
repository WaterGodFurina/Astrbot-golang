//go:build windows

package plugin

import "os/exec"

// Windows 没有 POSIX 进程组/内核级父进程死亡通知；Job Object 方案对
// go-plugin 子进程生命周期改造成本过高，这里保持现状：进程回收完全依赖
// go-plugin 的 autoKill 与 teardownInstance 里的 raw.Kill()（直接子进程）。
// 若未来需要整树回收，可在 Job Object 上启用 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE。
func setupChildProcess(cmd *exec.Cmd) {
	// no-op
}

// killProcessGroup 在 Windows 上无进程组可用，返回 false 让调用方回退
// raw.Kill() 原逻辑。
func killProcessGroup(inst *PluginInstance) bool {
	return false
}

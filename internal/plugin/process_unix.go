//go:build linux

package plugin

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// setupChildProcess puts the plugin subprocess in its own process group and
// asks the kernel to deliver SIGTERM to it when this host process dies
// (Pdeathsig). Together with killProcessGroup this guarantees no plugin
// process (nor anything it spawned) survives an unclean host exit
// (SIGKILL/panic), which previously left orphaned plugin processes with
// PPID=1 until the next startup's cleanupOrphanPlugins pass.
func setupChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}
}

// killProcessGroup terminates the plugin's whole process group (the direct
// child plus any process it spawned, e.g. Python bridge subprocesses):
// SIGTERM to every member, up to processGroupGrace for the direct child to
// exit (polling go-plugin's exit state), then SIGKILL the survivors. It
// returns false when there is nothing to signal (nil instance / pgid not
// recorded), in which case the caller falls back to raw.Kill().
func killProcessGroup(inst *PluginInstance) bool {
	if inst == nil {
		return false
	}
	inst.mu.Lock()
	pgid := inst.pgid
	raw := inst.raw
	inst.mu.Unlock()
	if pgid <= 0 {
		return false
	}
	if raw == nil {
		// 直接子进程不可观测（如握手失败路径）：短宽限后强制 SIGKILL。
		return terminateProcessGroup(pgid, nil)
	}
	return terminateProcessGroup(pgid, raw.Exited)
}

// terminateProcessGroup delivers SIGTERM to every process in group pgid
// (negative pid in kill(2)), waits up to processGroupGrace for the direct
// child to exit (via exited, when non-nil), then SIGKILLs any survivor.
// ESRCH (group already gone) counts as success.
func terminateProcessGroup(pgid int, exited func() bool) bool {
	if pgid <= 0 {
		return false
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if exited != nil {
		deadline := time.Now().Add(processGroupGrace)
		for time.Now().Before(deadline) && !exited() {
			time.Sleep(50 * time.Millisecond)
		}
	} else {
		time.Sleep(300 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	return true
}

// processGroupGrace is how long killProcessGroup waits after SIGTERM before
// force-killing the group (mirrors the host's orphanCleanupGrace).
const processGroupGrace = 1 * time.Second

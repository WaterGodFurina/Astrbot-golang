//go:build darwin

package plugin

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// setupChildProcess puts the plugin subprocess in its own process group.
// macOS has no Pdeathsig (syscall.SysProcAttr lacks the field), so host-death
// cleanup relies on killProcessGroup during teardown; the kernel-level
// orphan guarantee of Linux does not apply here.
func setupChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

// killProcessGroup terminates the plugin's whole process group (the direct
// child plus any process it spawned): SIGTERM to every member, up to
// processGroupGrace for the direct child to exit (polling go-plugin's exit
// state), then SIGKILL the survivors. It returns false when there is nothing
// to signal (nil instance / pgid not recorded), in which case the caller
// falls back to raw.Kill().
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

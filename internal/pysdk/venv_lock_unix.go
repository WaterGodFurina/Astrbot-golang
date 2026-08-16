//go:build linux || darwin

package pysdk

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// acquireVenvLock acquires an exclusive cross-process lock protecting venv
// provisioning (python -m venv + pip install). Uses flock, which the kernel
// releases automatically when the holder dies (no stale-lock problem). Blocks
// (polling LOCK_NB) until the lock is available or timeout elapses.
func acquireVenvLock(lockPath string, timeout time.Duration) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			_ = f.Close()
			return nil, fmt.Errorf("flock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("等待 venv 锁超时（%v）: %s", timeout, lockPath)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

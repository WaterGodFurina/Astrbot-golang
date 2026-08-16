//go:build windows

package pysdk

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// staleLockAge is how old a lock file must be before it is considered
// orphaned (holder crashed without releasing) and stolen. Must exceed the
// acquisition timeout so a live holder's lock is never stolen mid-install.
const staleLockAge = 30 * time.Minute

// acquireVenvLock acquires an exclusive cross-process lock protecting venv
// provisioning (python -m venv + pip install). Windows has no flock in the
// stdlib, so an O_CREATE|O_EXCL lock file is used with polling; a file older
// than staleLockAge (crash remnant) is removed and retried. Polls until the
// lock is available or timeout elapses.
func acquireVenvLock(lockPath string, timeout time.Duration) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create lock file: %w", err)
		}
		if info, serr := os.Stat(lockPath); serr == nil && time.Since(info.ModTime()) > staleLockAge {
			// 持有者崩溃残留的陈旧锁：删除后重试。
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待 venv 锁超时（%v）: %s", timeout, lockPath)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

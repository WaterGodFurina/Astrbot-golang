//go:build windows

package pysdk

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// staleLockAge is how old a lock file must be before it is considered
// orphaned (holder crashed without releasing) and stolen. Must exceed the
// acquisition timeout so a live holder's lock is never stolen mid-install.
const staleLockAge = 30 * time.Minute

// acquireVenvLock acquires an exclusive cross-process lock protecting venv
// provisioning (python -m venv + pip install). Windows has no flock in the
// stdlib, so an O_CREATE|O_EXCL lock file is used with polling; a file older
// than staleLockAge (crash remnant) is stolen. The steal is atomic: the lock
// file is renamed to a unique stale name (only one waiter's rename wins), so
// two waiters can never both "remove then recreate" and end up believing they
// hold the same lock. Polls until the lock is available or timeout elapses.
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
			ownedPath, ownerPID := lockPath, os.Getpid()
			return func() { releaseVenvLock(ownedPath, ownerPID) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create lock file: %w", err)
		}
		if info, serr := os.Stat(lockPath); serr == nil && time.Since(info.ModTime()) > staleLockAge {
			// 持有者崩溃残留的陈旧锁：原子重命名偷取。直接 Remove 存在
			// TOCTOU——两个等待者同时 remove+create 可能把对方刚建好的新锁
			// 删掉，双双以为自己持锁。rename 只能有一个成功，成功者取得
			// 偷取权后继续抢占 create；失败者继续轮询。
			stale := fmt.Sprintf("%s.stale.%d.%d", lockPath, os.Getpid(), time.Now().UnixNano())
			if rerr := os.Rename(lockPath, stale); rerr == nil {
				_ = os.Remove(stale)
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待 venv 锁超时（%v）: %s", timeout, lockPath)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// releaseVenvLock 仅在锁文件仍记录本进程 pid 时删除：锁可能已被他人原子
// 偷取并重建，盲目 Remove 会删掉别人持有的锁（归属校验缺失）。
func releaseVenvLock(lockPath string, pid int) {
	data, err := os.ReadFile(lockPath) // #nosec G304 -- lockPath is host-controlled under the cache dir
	if err != nil {
		return
	}
	got, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil || got != pid {
		return
	}
	_ = os.Remove(lockPath)
}

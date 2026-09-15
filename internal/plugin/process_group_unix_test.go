//go:build linux || darwin

package plugin

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProcessGroupLifecycle 验证进程组生命周期：Setpgid 生效（/proc/<pid>
// 的 pgrp 字段 == pid），卸载后整组进程真退出。
func TestProcessGroupLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("进程组测试仅支持 Linux")
	}
	requirePlugin(t)
	m := newTestManager(t)
	ctx := context.Background()

	inst, err := m.Load(ctx, "test", testPluginBin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inst.pgid <= 0 {
		t.Fatal("pgid 未记录（expected cmd.Process.Pid）")
	}
	// Setpgid 生效：插件进程的 pgrp 必须是它自己的 pid。
	pgrp, ok := processPGRP(inst.pgid)
	if !ok {
		t.Fatalf("读取 /proc/%d/stat 失败", inst.pgid)
	}
	if pgrp != inst.pgid {
		t.Fatalf("进程组未生效: pgrp=%d, 期望 %d (pid)", pgrp, inst.pgid)
	}

	if err := m.Unload("test"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	waitFor(t, 5*time.Second, "插件进程退出", func() bool {
		return !processAlive(inst.pgid)
	})
}

// TestTerminateProcessGroupKillsWholeTree 验证 killProcessGroup 的核心原语
// terminateProcessGroup：进程组内除直接子进程外再拉起的子进程（模拟 Python
// 桥再拉起子进程）也被一并回收。
func TestTerminateProcessGroupKillsWholeTree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("进程组测试仅支持 Linux")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh 不可用")
	}
	// sh 作为进程组组长，sleep 由它拉起并继承同组。
	cmd := exec.Command("sh", "-c", "sleep 100 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("启动 sh 失败: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pgid := cmd.Process.Pid

	waitFor(t, 3*time.Second, "组内出现 sh + sleep", func() bool {
		return len(processesInGroup(pgid)) >= 2
	})
	if got := processesInGroup(pgid); len(got) < 2 {
		t.Fatalf("期望组内至少 2 个进程，实际 %v", got)
	}

	if !terminateProcessGroup(pgid, nil) {
		t.Fatal("terminateProcessGroup 返回 false")
	}
	_ = cmd.Wait() // 直接子进程已被组信号回收
	waitFor(t, 3*time.Second, "进程组清空", func() bool {
		return len(processesInGroup(pgid)) == 0
	})
}

// processPGRP 读取 /proc/<pid>/stat 的第 5 字段（pgrp）。
func processPGRP(pid int) (int, bool) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	s := string(stat)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, false
	}
	fields := strings.Fields(s[idx+1:])
	if len(fields) < 3 {
		return 0, false
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, false
	}
	return pgrp, true
}

// processAlive 通过 kill(pid, 0) 探测进程是否存在（ESRCH = 已退出）。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processesInGroup 扫描 /proc 中所有 pgrp == pgid 的进程。
func processesInGroup(pgid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if g, ok := processPGRP(pid); ok && g == pgid {
			out = append(out, pid)
		}
	}
	return out
}

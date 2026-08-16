//go:build windows

package dashboard

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 没有 /proc：进程表用 Toolhelp32 快照（pid+ppid），各进程 RSS 用
// NtQueryInformationProcess(ProcessVmCounters) 的 WorkingSetSize 字节数。

// vmCounters mirrors PROCESS_MEMORY_COUNTERS_EX (x64 layout).
type vmCounters struct {
	PeakVirtualSize              uintptr
	VirtualSize                  uintptr
	PageFaultCount               uint32
	PeakWorkingSetSize           uintptr
	WorkingSetSize               uintptr
	QuotaPeakPagedPoolUsage      uintptr
	QuotaPagedPoolUsage          uintptr
	QuotaPeakNonPagedPoolUsage   uintptr
	QuotaNonPagedPoolUsage       uintptr
	PagefileUsage                uintptr
	PeakPagefileUsage            uintptr
}

// processRSSBytes returns the working set size of the given process in bytes,
// or 0 when it cannot be queried (exited / access denied).
func processRSSBytes(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var vc vmCounters
	err = windows.NtQueryInformationProcess(h, 3 /*ProcessVmCounters*/, unsafe.Pointer(&vc),
		uint32(unsafe.Sizeof(vc)), nil)
	if err != nil {
		return 0
	}
	return int64(vc.WorkingSetSize)
}

// enumProcessTree returns pid -> children built from a Toolhelp snapshot.
func enumProcessTree() map[int][]int {
	snapshot, err := windows.CreateToolhelp32Snapshot(0x00000002 /*TH32CS_SNAPPROCESS*/, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot)

	children := map[int][]int{}
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return children
	}
	for {
		children[int(entry.ParentProcessID)] = append(children[int(entry.ParentProcessID)], int(entry.ProcessID))
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}
	return children
}

// processMemoryMB returns the process resident (working) set size in MB.
func processMemoryMB() int {
	return int(processRSSBytes(os.Getpid()) >> 20)
}

// childProcessMemoryMB sums the working set of every descendant process of
// the current process (Go/Python plugin subprocesses and their children).
func childProcessMemoryMB() int {
	children := enumProcessTree()
	root := os.Getpid()
	desc := map[int]bool{}
	queue := children[root]
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if desc[pid] {
			continue
		}
		desc[pid] = true
		queue = append(queue, children[pid]...)
	}
	var total int64
	for pid := range desc {
		total += processRSSBytes(pid)
	}
	return int(total >> 20)
}

// systemMemoryMB returns total physical RAM in MB (GlobalMemoryStatusEx).
func systemMemoryMB() int {
	// MEMORYSTATUSEX: dwLength(4) + dwMemoryLoad(4) + 7×DWORDLONG(56) = 64.
	var buf [64]byte
	*(*uint32)(unsafe.Pointer(&buf[0])) = uint32(len(buf))
	k32 := syscall.NewLazyDLL("kernel32.dll")
	proc := k32.NewProc("GlobalMemoryStatusEx")
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(&buf[0])))
	if r == 0 {
		return 0
	}
	totalPhys := *(*uint64)(unsafe.Pointer(&buf[8])) // ullTotalPhys
	return int(totalPhys >> 20)
}

//go:build linux

package dashboard

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processMemoryMB returns the process resident set size in MB (Linux /proc).
func processMemoryMB() int {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return int(pages * 4096 >> 20)
}

// processRSSMB returns the resident set size of the given OS process in MB
// (Linux /proc/<pid>/statm), or 0 when unreadable (process exited/not ours).
func processRSSMB(pid int) int {
	if pid <= 0 {
		return 0
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return int(pages * 4096 >> 20)
}

// childProcessMemoryMB sums the RSS of every descendant process of the current
// process by scanning /proc/<pid>/stat for the PPid chain. 这覆盖全部插件
// 子进程——Go 插件二进制、Python 插件解释器（及其再拉起的子进程）——不依赖
// go-plugin 内部接口，也不区分插件语言。进程在扫描间隙退出时按 0 处理。
func childProcessMemoryMB() int {
	root := os.Getpid()
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		s := string(data)
		// comm 可能含空格/括号（"python (foo bar)"），从最后一个 ')' 后解析：
		// rest[0]=state, rest[1]=ppid。
		idx := strings.LastIndex(s, ")")
		if idx < 0 {
			continue
		}
		rest := strings.Fields(s[idx+1:])
		if len(rest) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(rest[1])
		if err != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}

	// BFS 收集整棵后代树（插件进程再拉起的子进程也计入）。
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

	total := 0
	for pid := range desc {
		total += processRSSMB(pid)
	}
	return total
}

// systemMemoryMB returns total system RAM in MB (Linux /proc/meminfo).
func systemMemoryMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return int(kb >> 10)
				}
			}
		}
	}
	return 0
}

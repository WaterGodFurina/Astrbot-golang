//go:build darwin

package dashboard

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// macOS 没有 /proc：进程表与 RSS 通过 `ps -axo pid=,ppid=,rss=` 一次取得
// （RSS 单位 KB），无需 cgo。ps 输出顺序不保证，但每行自带 pid/ppid。

// processMemoryMB returns the process resident set size in MB (via ps).
func processMemoryMB() int {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	kb, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return kb >> 10
}

// childProcessMemoryMB sums the RSS of every descendant process of the current
// process (Go/Python plugin subprocesses and their children).
func childProcessMemoryMB() int {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,rss=").Output()
	if err != nil {
		return 0
	}
	root := os.Getpid()
	children := map[int][]int{}
	rssKB := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		kb, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil || pid <= 0 {
			continue
		}
		children[ppid] = append(children[ppid], pid)
		rssKB[pid] = kb
	}

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
		total += rssKB[pid]
	}
	return total >> 10 // KB → MB
}

// systemMemoryMB returns total system RAM in MB (sysctl hw.memsize).
func systemMemoryMB() int {
	b, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0
	}
	s := strings.TrimRight(strings.TrimSpace(string(b)), "\x00")
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return int(n >> 20)
}

//go:build !linux && !darwin && !windows

package dashboard

// 其他平台（BSD 等）：内存统计暂无实现，返回 0（与旧行为一致——此前
// 所有平台都只读 Linux /proc，非 Linux 一律为 0）。

func processMemoryMB() int      { return 0 }
func systemMemoryMB() int       { return 0 }
func childProcessMemoryMB() int { return 0 }

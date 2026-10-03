//go:build linux

package plugin

import (
	"debug/elf"
	"strings"
)

// detectMusl inspects the running host executable's ELF interpreter to decide
// whether it was linked against musl: a glibc host's PT_INTERP is typically
// "/lib64/ld-linux-x86-64.so.2", a musl host's is "/lib/ld-musl-*.so.1".
// A statically linked binary (no PT_INTERP) is treated as glibc.
func detectMusl() bool {
	exe, err := elf.Open("/proc/self/exe")
	if err != nil {
		return false
	}
	defer exe.Close()
	for _, prog := range exe.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		data := make([]byte, prog.Filesz)
		if _, err := prog.ReadAt(data, 0); err != nil {
			return false
		}
		return strings.Contains(string(data), "ld-musl")
	}
	return false
}

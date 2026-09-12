//go:build windows

package pipeline

import "os"

// fileHardlinks on Windows: NTFS reports no POSIX nlink via stat, and hardlink aliases are governed by ACLs the sandbox already denies; mirror Python's POSIX-only guard by never blocking (missing files tolerated).
func fileHardlinks(path string) (int, bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return 1, st.Mode().IsRegular(), nil
}

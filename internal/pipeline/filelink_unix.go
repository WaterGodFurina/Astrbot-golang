//go:build !windows

package pipeline

import "syscall"

// fileHardlinks returns (nlink, isRegularFile) for the path; error when the stat itself fails for a non-missing reason (missing files report nlink=0, nil).
func fileHardlinks(path string) (int, bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		if isNotExistErr(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return int(st.Nlink), st.Mode&syscall.S_IFREG == syscall.S_IFREG, nil
}

func isNotExistErr(err error) bool {
	return err == syscall.ENOENT || err == syscall.ENOTDIR
}

package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitTreeHash snapshots the directory's tracked content and returns a git tree
// hash. The repo is initialized on first use. Returns "" on any failure.
func gitTreeHash(dir string) string {
	if dir == "" {
		return ""
	}
	gitInitIfNeeded(dir)
	_ = gitRun(dir, "add", "-A")
	out, err := gitRunOut(dir, "write-tree")
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	return strings.TrimSpace(out)
}

// gitDiffTree returns the patch between a previously captured tree hash and the
// current working tree ("" when identical or on failure).
func gitDiffTree(dir, oldHash string) string {
	if dir == "" || oldHash == "" {
		return ""
	}
	out, err := gitRunOut(dir, "diff", oldHash)
	if err != nil {
		return ""
	}
	return out
}

// gitRun runs a git command in dir, ignoring output.
func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// gitRunOut runs a git command in dir and returns its stdout.
func gitRunOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// gitInitIfNeeded initializes a repo in dir when it is not already one.
func gitInitIfNeeded(dir string) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return
	}
	_ = gitRun(dir, "init", "-q")
	_ = gitRun(dir, "config", "user.email", "astrbot@local")
	_ = gitRun(dir, "config", "user.name", "astrbot")
}

// snapshotFileMutation captures a git snapshot before a file-mutating tool and
// records the resulting patch on the returned result. Returns the result text
// with an appended patch note when the snapshot system is available.
func snapshotFileMutation(ws, toolName, result string) string {
	if ws == "" {
		return result
	}
	before := gitTreeHash(ws)
	if before == "" {
		return result
	}
	// After the tool ran, capture the patch.
	patch := gitDiffTree(ws, before)
	if patch == "" {
		return result
	}
	truncated := patch
	const maxPatch = 800
	if len(truncated) > maxPatch {
		truncated = truncated[:maxPatch] + "\n...(截断)"
	}
	logger.I18nInfo("工具 %s 修改了工作区文件（快照变更）:\n%s", toolName, truncated)
	return result + "\n\n[工作区快照] 工具 " + toolName + " 修改了文件:\n" + truncated
}

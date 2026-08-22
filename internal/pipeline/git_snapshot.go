package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// wsGitMu 按工作区串行化 git 操作：并发 git add 会争抢 index.lock 互相
// 阻塞或失败。条目（dir → *sync.Mutex）常驻，数量 = 使用过快照的工作区数。
var wsGitMu sync.Map

// wsGitLock returns the per-workspace mutex for dir.
func wsGitLock(dir string) *sync.Mutex {
	mu, _ := wsGitMu.LoadOrStore(dir, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// gitTreeHash snapshots the directory's tracked content and returns a git tree
// hash. The repo is initialized on first use. Returns "" on any failure.
func gitTreeHash(dir string) string {
	if dir == "" {
		return ""
	}
	mu := wsGitLock(dir)
	mu.Lock()
	defer mu.Unlock()
	gitInitIfNeeded(dir)
	if err := gitRun(dir, "add", "-A"); err != nil {
		logger.I18nWarn("工作区 %s 快照 git add 失败: %v", dir, err)
	}
	out, err := gitRunOut(dir, "write-tree")
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	return strings.TrimSpace(out)
}

// gitDiffTree returns the patch between a previously captured tree hash and the
// current working tree ("" when identical or on failure). The working tree is
// staged first so mutations made after the `before` hash was captured (which
// are not in the index yet) are included in the diff.
func gitDiffTree(dir, oldHash string) string {
	if dir == "" || oldHash == "" {
		return ""
	}
	mu := wsGitLock(dir)
	mu.Lock()
	defer mu.Unlock()
	if err := gitRun(dir, "add", "-A"); err != nil {
		logger.I18nWarn("工作区 %s 快照 git add 失败: %v", dir, err)
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

// snapshotFileMutation records the patch produced by a file-mutating tool on
// the returned result. `before` must be the git tree hash captured BEFORE the
// tool ran (see gitTreeHash); the patch is the diff between that tree and the
// current working tree. Returns the result text with an appended patch note
// when the snapshot system is available.
func snapshotFileMutation(ws, before, toolName, result string) string {
	if ws == "" || before == "" {
		return result
	}
	// The tool already ran; capture the patch against the pre-execution tree.
	patch := gitDiffTree(ws, before)
	if patch == "" {
		return result
	}
	const maxPatch = 800
	// 按 rune 截断（truncateRunes），避免字节切分切断 UTF-8 多字节字符
	// 产生非法 UTF-8（中文 diff 超长时必然触发）。
	truncated := truncateRunes(patch, maxPatch)
	if len(patch) > len(truncated) {
		truncated += "\n...(截断)"
	}
	logger.I18nInfo("工具 %s 修改了工作区文件（快照变更）:\n%s", toolName, truncated)
	return result + "\n\n[工作区快照] 工具 " + toolName + " 修改了文件:\n" + truncated
}

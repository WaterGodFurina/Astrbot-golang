package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/sandbox"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
)

// computerUseAllowed reports whether the event's sender is allowed to use the
// computer-use local runtime tools. The allow list comes from
// provider_settings.computer_use_allowed_senders; an empty list denies everyone.
func computerUseAllowed(cfg map[string]interface{}, event *core.Event) bool {
	ps, _ := cfg["provider_settings"].(map[string]interface{})
	allow, _ := ps["computer_use_allowed_senders"].([]interface{})
	for _, v := range allow {
		if s, ok := v.(string); ok && s == event.GetSenderID() {
			return true
		}
	}
	return false
}

// errStopGrep stops a directory walk once the result limit is reached.
var errStopGrep = fmt.Errorf("grep result limit reached")

// workspaceSanitizeRe sanitizes a conversation id for use as a directory name.
var workspaceSanitizeRe = regexp.MustCompile(`[^A-Za-z0-9_.\-]`)

// --- sandbox executors ---
// These route computer-use operations into the Docker-backed sandbox runtime.
// Paths are relative to the sandbox's /workspace.

const sandboxWorkdir = "/workspace"

// sandboxResolvePath resolves a tool-supplied path for the sandbox runtime.
// Mirroring Python's fs.py rule "In sandbox runtime, relative paths are passed
// through unchanged", relative paths resolve under the sandbox working
// directory (/workspace); absolute paths are used as-is. Paths are normalized
// to POSIX separators so they are safe to hand to the container.
func sandboxResolvePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = sandboxWorkdir + "/" + p
	}
	return p
}

func sandboxShell(ctx context.Context, mgr *sandbox.Manager, command string) string {
	if strings.TrimSpace(command) == "" {
		return "Error executing command: `command` must be a non-empty string."
	}
	stdout, stderr, code, err := mgr.Exec(ctx, "sh", []string{"-c", command}, sandboxWorkdir)
	if err != nil && code < 0 {
		return "Error executing command in sandbox: " + err.Error()
	}
	out := stdout + stderr
	if code != 0 {
		return fmt.Sprintf("Command failed with exit code %d.\nOutput:\n%s", code, out)
	}
	if strings.TrimSpace(out) == "" {
		return "Command completed with exit code 0 (no output)."
	}
	return fmt.Sprintf("Command completed with exit code 0.\nOutput:\n%s", out)
}

func sandboxPython(ctx context.Context, mgr *sandbox.Manager, code string) string {
	if strings.TrimSpace(code) == "" {
		return "Error executing code: `code` must be a non-empty string."
	}
	stdout, stderr, exitCode, err := mgr.Exec(ctx, "python3", []string{"-c", code}, sandboxWorkdir)
	if err != nil && exitCode < 0 {
		return "error: code execution failed in sandbox: " + err.Error()
	}
	if exitCode != 0 {
		return fmt.Sprintf("error: code execution failed with exit code %d\n%s", exitCode, stdout+stderr)
	}
	return stdout
}

func sandboxFileRead(ctx context.Context, mgr *sandbox.Manager, path string) string {
	path = sandboxResolvePath(path)
	if path == "" {
		return "Error reading file: `path` must be a non-empty string."
	}
	content, err := mgr.ReadFile(ctx, path)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	return fmt.Sprintf("Content of %s:\n%s", path, content)
}

func sandboxFileWrite(ctx context.Context, mgr *sandbox.Manager, path, content string) string {
	path = sandboxResolvePath(path)
	if path == "" {
		return "Error writing file: `path` must be a non-empty string."
	}
	if err := mgr.WriteFile(ctx, path, content); err != nil {
		return "Error writing file: " + err.Error()
	}
	return "File written successfully: " + path
}

func sandboxFileEdit(ctx context.Context, mgr *sandbox.Manager, path, old, new string, replaceAll bool) string {
	path = sandboxResolvePath(path)
	if path == "" || old == "" {
		return "Error editing file: `path` and `old` must be non-empty."
	}
	content, err := mgr.ReadFile(ctx, path)
	if err != nil {
		return "Error editing file: " + err.Error()
	}
	replacements := 0
	if replaceAll {
		replacements = strings.Count(content, old)
		content = strings.ReplaceAll(content, old, new)
	} else {
		idx := strings.Index(content, old)
		if idx < 0 {
			return "Error editing file: old string not found in " + path + "."
		}
		replacements = 1
		content = content[:idx] + new + content[idx+len(old):]
	}
	if err := mgr.WriteFile(ctx, path, content); err != nil {
		return "Error editing file: " + err.Error()
	}
	modeText := "first match"
	if replaceAll {
		modeText = "all matches"
	}
	return fmt.Sprintf("Edited %s. Replaced %d occurrence(s) using %s mode.", path, replacements, modeText)
}

func sandboxGrep(ctx context.Context, mgr *sandbox.Manager, pattern, path string) string {
	if strings.TrimSpace(pattern) == "" {
		return "Error: `pattern` must be a non-empty string."
	}
	if path == "" {
		path = "."
	}
	stdout, stderr, code, err := mgr.Exec(ctx, "sh", []string{"-c", "grep -rn " + "'" + strings.ReplaceAll(pattern, "'", "'\\''") + "' " + "'" + strings.ReplaceAll(path, "'", "'\\''") + "' 2>/dev/null"}, sandboxWorkdir)
	if err != nil && code < 0 {
		return "Error searching in sandbox: " + err.Error()
	}
	out := stdout + stderr
	if strings.TrimSpace(out) == "" {
		return "No matches found."
	}
	return strings.TrimRight(out, "\n")
}

// Computer-Use "local" runtime tools.
//
// Ported from astrbot/core/tools/computer_tools/ (shell.py, python.py, fs.py).
// When provider_settings.computer_use_runtime == "local", these tools are
// injected into the LLM request so skills can actually be executed on the host.

// workspaceRoot returns the per-conversation workspace directory used as the
// cwd for shell/python tools and the base for relative file paths.
//
// The returned path is always absolute so that file tools, shell and python
// executors all agree on the same base regardless of the process working
// directory at call time.
func workspaceRoot(umo string) string {
	sanitized := workspaceSanitizeRe.ReplaceAllString(umo, "_")
	dir := filepath.Join("data", "workspaces", sanitized)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	_ = os.MkdirAll(dir, 0o750)
	return dir
}

// localTempRoots lists the temporary directories (mirroring Python's
// get_astrbot_temp_path and get_astrbot_system_tmp_path) that file tools may
// read from and write to.
func localTempRoots() []string {
	return []string{
		filepath.Join("data", "temp"),
		filepath.Join(os.TempDir(), ".astrbot"),
	}
}

// allowedReadRoots lists directories file tools may read from.
func allowedReadRoots(umo string) []string {
	return append([]string{
		workspaceRoot(umo),
		filepath.Join("data", "skills"),
		filepath.Join("data", "plugins"),
	}, localTempRoots()...)
}

// allowedWriteRoots lists directories file tools may write to (workspace and
// temporary directories only; skills and plugins are read-only).
func allowedWriteRoots(umo string) []string {
	return append([]string{
		workspaceRoot(umo),
	}, localTempRoots()...)
}

// expandHome expands a leading ~ or ~/ to the current user's home directory,
// mirroring Python's pathlib.Path.expanduser used in _resolve_tool_path.
func expandHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// resolveLocalPath resolves a tool-supplied path relative to the workspace
// root, expanding ~ and normalizing to an absolute path, then enforces it
// stays within the allowed roots. Returning an absolute path (like Python's
// Path.resolve) lets the model feed the reported path straight back into
// another tool without it being re-anchored under the workspace again.
func resolveLocalPath(path, umo string, write bool) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("`path` must be a non-empty string")
	}
	expanded := expandHome(path)
	resolved := expanded
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(workspaceRoot(umo), expanded)
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	resolved = filepath.Clean(resolved)
	roots := allowedReadRoots(umo)
	if write {
		roots = allowedWriteRoots(umo)
	}
	for _, root := range roots {
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if within, err := pathWithin(rootAbs, resolved); err == nil && within {
			if err := enforceRealPathWithin(resolved, roots); err != nil {
				return "", err
			}
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed workspace and skill directories", path)
}

// enforceRealPathWithin resolves symlinks on the given path (falling back to
// the deepest existing ancestor for paths that do not exist yet) and rejects
// it when its real location is outside the allowed roots. This closes the
// lexical-check gap that lets a symlink inside the workspace point at files
// elsewhere on the host. A path that does not exist anywhere yet keeps the
// lexical result, so the check never breaks create-first flows.
func enforceRealPathWithin(resolved string, roots []string) error {
	cur := resolved
	var tail []string
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for _, comp := range tail {
				real = filepath.Join(real, comp)
			}
			for _, root := range roots {
				rootAbs, aerr := filepath.Abs(root)
				if aerr != nil {
					continue
				}
				rootReal := rootAbs
				if r, rerr := filepath.EvalSymlinks(rootAbs); rerr == nil {
					rootReal = r
				}
				if within, werr := pathWithin(rootReal, real); werr == nil && within {
					return nil
				}
			}
			return fmt.Errorf("path %q resolves outside the allowed workspace and skill directories", resolved)
		}
		parent := filepath.Dir(cur)
		if parent == cur || parent == "." {
			return nil
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

func pathWithin(root, target string) (bool, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false, err
	}
	if rel == "." {
		return true, nil
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..", nil
}

// localModePrompt mirrors Python's _build_local_mode_prompt(). It announces
// the host working directory so the model knows where shell/python/file tools
// run and prefers relative paths (matching the tool descriptions that say
// "If relative, will be in workspace root").
func localModePrompt(workspacePath string) string {
	osName := runtime.GOOS
	workspaceNote := "Your working directory is `" + workspacePath + "`. " +
		"Shell and Python commands run with this as their current directory " +
		"(equivalent to `cd <working_dir> && <your command>`). Prefer relative paths; " +
		"`astrbot_file_*` tools resolve relative paths from here and reject absolute " +
		"paths outside the workspace, skills, plugins and temporary directories."
	if osName == "windows" {
		return "You have access to the host local environment and can execute shell commands and Python code. " +
			"Current operating system: " + osName + ". " +
			"The runtime shell is Windows PowerShell 5.1 (powershell.exe). " +
			"Use Windows PowerShell 5.1-compatible syntax and cmdlets; do not use " +
			"PowerShell 7-only syntax or assume Unix commands like cat/ls/grep are available. " +
			workspaceNote + " " +
			"Local shell commands automatically return a managed session when they " +
			"outlive the initial wait. Use `astrbot_shell_session` to list, poll, " +
			"write raw text or complete lines to, interrupt, or terminate those sessions. " +
			"Use its `write_line` action for line-oriented programs so the session receives " +
			"a real line feed. Do not add `&`, `nohup`, or another detachment wrapper for " +
			"ordinary long-running commands."
	}
	return "You have access to the host local environment and can execute shell commands and Python code. " +
		"Current operating system: " + osName + ". " +
		"The runtime shell is Unix-like. Use POSIX-compatible shell commands. " +
		workspaceNote + " " +
		"Local shell commands automatically return a managed session when they " +
		"outlive the initial wait. Use `astrbot_shell_session` to list, poll, " +
		"write raw text or complete lines to, interrupt, or terminate those sessions. " +
		"Use its `write_line` action for line-oriented programs so the session receives " +
		"a real line feed. Do not add `&`, `nohup`, or another detachment wrapper for " +
		"ordinary long-running commands."
}

// sandboxModePrompt mirrors Python's sandbox environment announcement.
func sandboxModePrompt() string {
	return "You are running inside an isolated sandbox container. " +
		"You can execute shell commands and Python code inside the sandbox. " +
		"The sandbox root workspace is `/workspace`. " +
		"`astrbot_execute_shell` and `astrbot_execute_python` use it as their working directory. " +
		"`astrbot_file_read_tool`, `astrbot_file_write_tool`, `astrbot_file_edit_tool`, and " +
		"`astrbot_grep_tool` resolve relative paths from it. " +
		"Prefer relative paths within `/workspace`."
}

// collectSandboxTools builds the OpenAI tool schemas for the sandbox runtime.
func collectSandboxTools() []map[string]interface{} {
	return collectLocalTools()
}

// collectLocalTools builds the OpenAI tool schemas for the local runtime.
func collectLocalTools() []map[string]interface{} {
	schemas := []map[string]interface{}{
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "astrbot_execute_shell",
				"description": "The shell command to execute in the current runtime shell (for example, powershell.exe on Windows). Equivalent to `cd {working_dir} && {command}` where {working_dir} is the conversation workspace. Prefer relative paths. 注意：该命令直接在宿主机（运行 AstrBot 的服务器）上执行，未被沙箱隔离，请谨慎使用，避免破坏性或危险操作。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{
							"type":        "string",
							"description": "The shell command to execute in the current workspace.",
						},
						"background": map[string]interface{}{
							"type":        "boolean",
							"description": "Run the command in the background. Use the file read tool to read the output later. For long running commands, using this option.",
						},
						"timeout": map[string]interface{}{
							"type":        "integer",
							"description": "Optional timeout in seconds for the command execution.",
						},
					},
					"required": []interface{}{"command"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name": "astrbot_shell_session",
				"description": "List, poll, write raw text or complete lines to, interrupt, or terminate managed shell sessions. " +
					"Sessions are isolated to the current conversation and sender.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"action": map[string]interface{}{
							"type":        "string",
							"enum":        []interface{}{"list", "poll", "write", "write_line", "interrupt", "terminate"},
							"description": "Session operation to perform.",
						},
						"session_id": map[string]interface{}{
							"type":        "string",
							"description": "The managed shell session id.",
						},
						"data": map[string]interface{}{
							"type":        "string",
							"description": "The text to write to the session (for write/write_line).",
						},
					},
					"required": []interface{}{"action"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "astrbot_execute_python",
				"description": "Execute codes in a Python environment. Current OS: " + runtime.GOOS + ". Use system-compatible commands. 注意：该代码直接在宿主机（运行 AstrBot 的服务器）上执行，未被沙箱隔离，请谨慎使用，避免破坏性或危险操作。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"code": map[string]interface{}{
							"type":        "string",
							"description": "The Python code to execute.",
						},
						"timeout": map[string]interface{}{
							"type":        "integer",
							"description": "Optional timeout in seconds for code execution.",
						},
					},
					"required": []interface{}{"code"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "astrbot_file_read_tool",
				"description": "read file content. Supports text, image, and PDF (text extraction), docx and epub files.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path of the file to read. If relative, will be in workspace root.",
						},
						"offset": map[string]interface{}{
							"type":        "integer",
							"description": "Optional line offset to start reading from. 0-based index.",
						},
						"limit": map[string]interface{}{
							"type":        "integer",
							"description": "Optional maximum number of lines to read.",
						},
					},
					"required": []interface{}{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "astrbot_file_write_tool",
				"description": "Write UTF-8 text content to a file.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path of the file to write. If relative, will be in workspace root.",
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The content to write to the file",
						},
					},
					"required": []interface{}{"path", "content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "astrbot_file_edit_tool",
				"description": "Editing files.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Path of the file to edit. If relative, will be in workspace root.",
						},
						"old": map[string]interface{}{
							"type":        "string",
							"description": "The exact old text to replace.",
						},
						"new": map[string]interface{}{
							"type":        "string",
							"description": "The replacement text.",
						},
						"replace_all": map[string]interface{}{
							"type":        "boolean",
							"description": "Whether to replace all matches. Defaults to false.",
						},
					},
					"required": []interface{}{"path", "old", "new"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "astrbot_grep_tool",
				"description": "Search and read file contents using ripgrep.",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"pattern": map[string]interface{}{
							"type":        "string",
							"description": "The expression pattern to search for in file contents.",
						},
						"path": map[string]interface{}{
							"type":        "string",
							"description": "File or directory to search in (rg PATH). If relative, will be in workspace root.",
						},
						"glob": map[string]interface{}{
							"type":        "string",
							"description": "Optional glob filter such as `*.py`, `*.{ts,tsx}`.",
						},
						"result_limit": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum number of result groups returned by the tool. Defaults to 100.",
						},
					},
					"required": []interface{}{"pattern"},
				},
			},
		},
	}
	return schemas
}

// --- local tool executors ---

func argString(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argBool(args map[string]interface{}, key string) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}

func argInt(args map[string]interface{}, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// shellSession tracks a background shell process.
type shellSession struct {
	ID         string
	Command    string
	Cwd        string
	OutputFile string
	StartTime  time.Time
	Cmd        *exec.Cmd
	Stdin      io.WriteCloser
	Owner      string

	// exitMu guards the exit state written by the Wait goroutine and read by
	// status(); reading Cmd.ProcessState directly would race with cmd.Wait().
	exitMu   sync.Mutex
	done     bool
	success  bool
	exitCode int
}

var (
	shellSessionsMu sync.Mutex
	shellSessions   = map[string]*shellSession{}
	// shellSessionTTL is how long a completed session stays listed so
	// poll/status can still report its exit info before it is reaped (L-17).
	shellSessionTTL = 30 * time.Minute
)

func (s *shellSession) status() map[string]interface{} {
	state := "running"
	exitCode := 0
	s.exitMu.Lock()
	if s.done {
		state = "completed"
		if !s.success {
			exitCode = s.exitCode
		}
	}
	s.exitMu.Unlock()
	return map[string]interface{}{
		"session_id":  s.ID,
		"command":     s.Command,
		"cwd":         s.Cwd,
		"status":      state,
		"exit_code":   exitCode,
		"output_file": s.OutputFile,
		"started_at":  s.StartTime.Format(time.RFC3339),
		"owner":       s.Owner,
	}
}

// cappedWriter caps buffered output at max bytes, flagging truncation instead
// of growing without bound.
type cappedWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.buf.Len() >= w.max {
		w.truncated = true
		return len(p), nil
	}
	n := len(p)
	if room := w.max - w.buf.Len(); room < n {
		n = room
		w.truncated = true
	}
	w.buf.Write(p[:n])
	return len(p), nil
}

// Bytes returns the buffered output (a byte slice sharing the underlying
// buffer; callers must not mutate it).
func (w *cappedWriter) Bytes() []byte { return w.buf.Bytes() }

// maxShellOutput caps how much of a synchronous shell command's output is
// buffered in memory.
const maxShellOutput = 1 << 20

// sessionOwnedBy reports whether the session belongs to the given conversation
// (umo) AND the given sender who started it.
func sessionOwnedBy(s *shellSession, umo, senderID string) bool {
	return s.Owner == umo+"\x00"+senderID
}

func executeLocalShell(umo, senderID, command string, background bool, timeout int) string {
	if strings.TrimSpace(command) == "" {
		return "Error executing command: `command` must be a non-empty string."
	}
	ws := workspaceRoot(umo)
	_ = os.MkdirAll(ws, 0o750)

	if timeout <= 0 {
		timeout = 300
	}
	if timeout > 600 {
		timeout = 600 // 与沙箱路径一致，防止被提示注入的模型让宿主机进程长期驻留
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-Command", command) // #nosec G204 -- astrbot_execute_shell 工具核心：执行 AI 指令给定的 shell 命令（host 本地运行，功能明确，前端已警示）; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	} else {
		cmd = exec.Command("sh", "-c", command) // #nosec G204 -- astrbot_execute_shell 工具核心：执行 AI 指令给定的 shell 命令（host 本地运行，功能明确，前端已警示）; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	}
	cmd.Dir = ws

	if background {
		logDir := filepath.Join(ws, ".astrbot")
		_ = os.MkdirAll(logDir, 0o750)
		outFile := filepath.Join(logDir, "astrbot_shell_stdout_"+randHex(4)+".log")
		f, err := os.OpenFile(outFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return "Error executing command: " + err.Error()
		}
		cmd.Stdout = f
		cmd.Stderr = f
		stdin, stdinErr := cmd.StdinPipe()
		// 后台会话同样放入独立进程组，terminate 时按组回收整棵进程树。
		utils.SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			_ = f.Close()
			if stdin != nil {
				_ = stdin.Close()
			}
			return "Error executing command: " + err.Error()
		}
		if stdinErr != nil {
			logger.I18nWarn("shell session stdin pipe creation failed: %v", stdinErr)
			stdin = nil
		}
		id := randHex(8)
		ses := &shellSession{
			ID:         id,
			Command:    command,
			Cwd:        ws,
			OutputFile: outFile,
			StartTime:  time.Now(),
			Cmd:        cmd,
			Stdin:      stdin,
			Owner:      umo + "\x00" + senderID,
		}
		shellSessionsMu.Lock()
		shellSessions[id] = ses
		shellSessionsMu.Unlock()
		go func() {
			err := cmd.Wait()
			if stdin != nil {
				_ = stdin.Close()
			}
			_ = f.Close()
			ses.exitMu.Lock()
			ses.done = true
			ses.success = err == nil
			if ee, ok := err.(*exec.ExitError); ok {
				ses.exitCode = ee.ExitCode()
			}
			ses.exitMu.Unlock()
			// Reap the completed session after the TTL so poll/status can still
			// report its exit info in the meantime.
			time.AfterFunc(shellSessionTTL, func() {
				shellSessionsMu.Lock()
				if cur := shellSessions[id]; cur == ses {
					delete(shellSessions, id)
				}
				shellSessionsMu.Unlock()
			})
		}()
		return fmt.Sprintf("Command started in background (session id: %s). "+
			"Output written to `%s`. Use astrbot_file_read_tool to read it, "+
			"or astrbot_shell_session to manage the session.", id, outFile)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	cmd = exec.CommandContext(ctx, shellPath(), "-c", command) // #nosec G204 -- astrbot_execute_shell 工具核心（前端同步执行路径）：执行 AI 指令给定的 shell 命令; nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Dir = ws
	// Run the shell in its own process group so a timeout kills the whole
	// group: sh -c "a; b &" leaves grandchildren running otherwise (L-22).
	// Windows 无进程组，SetProcessGroup 为 no-op（taskkill /T 兜底）。
	utils.SetProcessGroup(cmd)
	// CommandContext only kills the direct child on timeout; grandchildren that
	// inherit the output pipes would keep CombinedOutput blocked and survive as
	// orphans. Start first so cmd.Process is set before the kill goroutine
	// reads it, then kill the whole process group the moment the timeout fires.
	var outCap cappedWriter
	outCap.max = maxShellOutput
	cmd.Stdout = &outCap
	cmd.Stderr = &outCap
	if err := cmd.Start(); err != nil {
		return "Error executing command: " + err.Error()
	}
	groupDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = utils.KillProcessGroup(cmd)
		case <-groupDone:
		}
	}()
	err := cmd.Wait()
	close(groupDone)
	out := outCap.Bytes()
	if outCap.truncated {
		out = append(out, []byte("\n...(output truncated)\n")...)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Command timed out after %d seconds. Output so far:\n%s", timeout, string(out))
	}
	status := "Command completed"
	prefix := "Command completed with exit code 0"
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			prefix = fmt.Sprintf("Command failed with exit code %d", ee.ExitCode())
		} else {
			prefix = "Command failed"
		}
	}
	if len(out) == 0 {
		return prefix + " (no output)."
	}
	return status + ".\n" + prefix + ".\nOutput:\n" + string(out)
}

func shellPath() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "/bin/sh"
}

func executeShellSession(umo, senderID, action, sessionID, data string) string {
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case "list":
		shellSessionsMu.Lock()
		items := make([]map[string]interface{}, 0, len(shellSessions))
		for _, s := range shellSessions {
			if sessionOwnedBy(s, umo, senderID) {
				items = append(items, s.status())
			}
		}
		shellSessionsMu.Unlock()
		sort.Slice(items, func(i, j int) bool {
			return items[i]["started_at"].(string) < items[j]["started_at"].(string)
		})
		out, _ := json.Marshal(items)
		return string(out)
	case "poll", "get", "status":
		return shellSessionPoll(sessionID, umo, senderID)
	case "write":
		return shellSessionWrite(sessionID, data, umo, senderID, false)
	case "write_line":
		return shellSessionWrite(sessionID, data, umo, senderID, true)
	case "interrupt":
		return shellSessionSignal(sessionID, false, umo, senderID)
	case "terminate", "kill":
		return shellSessionSignal(sessionID, true, umo, senderID)
	default:
		return fmt.Sprintf("Unknown action %q. Valid actions: list, poll, write, write_line, interrupt, terminate.", action)
	}
}

func shellSessionPoll(sessionID, umo, senderID string) string {
	shellSessionsMu.Lock()
	s, ok := shellSessions[sessionID]
	shellSessionsMu.Unlock()
	if !ok {
		return "Session not found: " + sessionID
	}
	if !sessionOwnedBy(s, umo, senderID) {
		return "Session " + sessionID + " does not belong to the current user."
	}
	tail := ""
	if data, err := os.ReadFile(s.OutputFile); err == nil {
		if len(data) > 20000 {
			data = data[len(data)-20000:]
			tail = "...(truncated)\n"
		}
		tail += string(data)
	}
	st, _ := json.Marshal(s.status())
	return string(st) + "\nOutput:\n" + tail
}

// shellSessionWrite writes raw data to a session's stdin. addNewline appends
// a real line feed (the write_line action) so the session receives it.
func shellSessionWrite(sessionID, data, umo, senderID string, addNewline bool) string {
	shellSessionsMu.Lock()
	s, ok := shellSessions[sessionID]
	shellSessionsMu.Unlock()
	if !ok {
		return "Session not found: " + sessionID
	}
	if !sessionOwnedBy(s, umo, senderID) {
		return "Session " + sessionID + " does not belong to the current user."
	}
	if s.Stdin == nil {
		return "Session " + sessionID + " does not accept input."
	}
	if addNewline {
		data += "\n"
	}
	if _, err := io.WriteString(s.Stdin, data); err != nil {
		return "Error writing to session: " + err.Error()
	}
	return "Written to session " + sessionID + "."
}

func shellSessionSignal(sessionID string, kill bool, umo, senderID string) string {
	shellSessionsMu.Lock()
	s, ok := shellSessions[sessionID]
	shellSessionsMu.Unlock()
	if !ok {
		return "Session not found: " + sessionID
	}
	if !sessionOwnedBy(s, umo, senderID) {
		return "Session " + sessionID + " does not belong to the current user."
	}
	if s.Cmd == nil || s.Cmd.Process == nil {
		return "Session " + sessionID + " is not running."
	}
	if kill {
		// 按进程组终止，回收整棵进程树（含后台再拉起的子进程）。
		if err := utils.KillProcessGroup(s.Cmd); err != nil {
			return "Error terminating session: " + err.Error()
		}
		return "Session " + sessionID + " terminated."
	}
	if err := s.Cmd.Process.Signal(os.Interrupt); err != nil {
		return "Error interrupting session: " + err.Error()
	}
	return "Session " + sessionID + " interrupted."
}

func executeLocalPython(umo, code string, timeout int) string {
	if strings.TrimSpace(code) == "" {
		return "Error executing code: `code` must be a non-empty string."
	}
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > 600 {
		timeout = 600 // 与沙箱路径一致
	}
	ws := workspaceRoot(umo)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", code) // #nosec G204 -- astrbot_execute_python 工具核心：执行 AI 指令给定的 Python 代码（host 本地运行，功能明确，前端已警示）
	cmd.Dir = ws
	// 与同步 shell 路径一致：输出经 cappedWriter 限幅，防止超时窗口内
	// print 风暴把宿主进程 OOM。
	cw := &cappedWriter{max: maxShellOutput}
	cmd.Stdout = cw
	cmd.Stderr = cw
	if err := cmd.Start(); err != nil {
		return "error: code execution failed to start: " + err.Error()
	}
	waitErr := cmd.Wait()
	out := string(cw.Bytes())
	if cw.truncated {
		out += fmt.Sprintf("\n[输出超过 %d 字节已截断]", maxShellOutput)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Code execution timed out after %d seconds. Output so far:\n%s", timeout, out)
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return fmt.Sprintf("error: code execution failed with exit code %d\n%s", ee.ExitCode(), out)
		}
		return "error: code execution failed: " + waitErr.Error()
	}
	return out
}

// maxFileReadBytes caps how much of a file executeFileRead loads into memory
// at once; larger files must be read in chunks via offset/limit.
const maxFileReadBytes = 1 << 20

func executeFileRead(path, umo string, offset, limit int) string {
	resolved, err := resolveLocalPath(path, umo, false)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	if info.IsDir() {
		return fmt.Sprintf("Error: '%s' is a directory, not a file. "+
			"Use a file path instead, or use 'astrbot_execute_shell' to list directory contents.", resolved)
	}
	// 先 stat 判断大小：超限文件拒绝整体读入内存。带 offset/limit 时走
	// 分页路径（按行流式读取，不整体载入内存），否则提示用 offset/limit。
	if info.Size() > maxFileReadBytes {
		if offset <= 0 && limit <= 0 {
			return fmt.Sprintf("Error reading file: %s is %d bytes, exceeds the %d-byte read limit. "+
				"Use offset/limit to read a portion of the file.", resolved, info.Size(), maxFileReadBytes)
		}
		f, err := os.Open(resolved)
		if err != nil {
			return "Error reading file: " + err.Error()
		}
		defer f.Close()
		var lines []string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for i := 0; sc.Scan() && (limit <= 0 || i < offset+limit); i++ {
			if i >= offset {
				lines = append(lines, sc.Text())
			}
		}
		return fmt.Sprintf("Read %d lines from %s:\n%s", len(lines), resolved, strings.Join(lines, "\n"))
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	content := string(data)
	if offset > 0 || limit > 0 {
		lines := strings.Split(content, "\n")
		if offset < 0 {
			offset = 0
		}
		if offset > len(lines) {
			offset = len(lines)
		}
		end := len(lines)
		if limit > 0 && offset+limit < end {
			end = offset + limit
		}
		content = strings.Join(lines[offset:end], "\n")
	}
	return fmt.Sprintf("Read %d bytes from %s:\n%s", info.Size(), resolved, content)
}

func executeFileWrite(path, content, umo string) string {
	resolved, err := resolveLocalPath(path, umo, true)
	if err != nil {
		return "Error writing file: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o750); err != nil {
		return "Error writing file: " + err.Error()
	}
	if err := os.WriteFile(resolved, []byte(content), 0o600); err != nil {
		return "Error writing file: " + err.Error()
	}
	return "File written successfully: " + resolved
}

func executeFileEdit(path, old, new string, replaceAll bool, umo string) string {
	resolved, err := resolveLocalPath(path, umo, true)
	if err != nil {
		return "Error editing file: " + err.Error()
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "Error editing file: " + err.Error()
	}
	content := string(data)
	if old == "" {
		return "Error editing file: `old` must be a non-empty string."
	}
	replacements := 0
	if replaceAll {
		replacements = strings.Count(content, old)
		content = strings.ReplaceAll(content, old, new)
	} else {
		idx := strings.Index(content, old)
		if idx < 0 {
			return "Error editing file: old string not found in " + resolved + "."
		}
		replacements = 1
		content = content[:idx] + new + content[idx+len(old):]
	}
	if err := os.WriteFile(resolved, []byte(content), 0o600); err != nil {
		return "Error editing file: " + err.Error()
	}
	modeText := "first match"
	if replaceAll {
		modeText = "all matches"
	}
	return fmt.Sprintf("Edited %s. Replaced %d occurrence(s) using %s mode.", resolved, replacements, modeText)
}

func executeGrep(pattern, path, glob string, resultLimit int, umo string) string {
	if strings.TrimSpace(pattern) == "" {
		return "Error: `pattern` must be a non-empty string."
	}
	if resultLimit <= 0 {
		resultLimit = 100
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "Error: invalid pattern: " + err.Error()
	}
	searchPath := path
	if searchPath == "" {
		searchPath = "."
	}
	resolved, err := resolveLocalPath(searchPath, umo, false)
	if err != nil {
		return "Error searching: " + err.Error()
	}

	var matches []string
	_ = filepath.WalkDir(resolved, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		// 每文件限读：只扫描前 1MB，超大文件不会整体读入内存。
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		data, err := io.ReadAll(io.LimitReader(f, maxFileReadBytes))
		_ = f.Close()
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", p, i+1, line))
				if len(matches) >= resultLimit {
					return errStopGrep
				}
			}
		}
		return nil
	})
	if len(matches) == 0 {
		return "No matches found."
	}
	return strings.Join(matches, "\n")
}

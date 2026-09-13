package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/knowledgebase"
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

func sandboxShell(ctx context.Context, mgr *sandbox.Manager, sessionID, command string) string {
	if strings.TrimSpace(command) == "" {
		return "Error executing command: `command` must be a non-empty string."
	}
	stdout, stderr, code, err := mgr.Exec(ctx, sessionID, "sh", []string{"-c", command}, sandboxWorkdir)
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

func sandboxPython(ctx context.Context, mgr *sandbox.Manager, sessionID, code string) string {
	if strings.TrimSpace(code) == "" {
		return "Error executing code: `code` must be a non-empty string."
	}
	stdout, stderr, exitCode, err := mgr.Exec(ctx, sessionID, "python3", []string{"-c", code}, sandboxWorkdir)
	if err != nil && exitCode < 0 {
		return "error: code execution failed in sandbox: " + err.Error()
	}
	if exitCode != 0 {
		return fmt.Sprintf("error: code execution failed with exit code %d\n%s", exitCode, stdout+stderr)
	}
	return stdout
}

// sandboxFileRead reads a sandbox file; images are returned as base64 for the multimodal channel (py probe→image: text channel corrupts binary, so both probe and full read ride `base64 -w0`, mirroring py _build_probe_script/_build_image_read_script over a byte-safe transport).
func sandboxFileRead(ctx context.Context, mgr *sandbox.Manager, sessionID, path string, offset, limit int) (string, string, string) {
	path = sandboxResolvePath(path)
	if path == "" {
		return "Error reading file: `path` must be a non-empty string.", "", ""
	}
	q := strings.ReplaceAll(path, "'", "'\\''")
	isDoc := docExtSet[strings.ToLower(pathExt(path))]
	maxBytes := int64(maxToolImageBytes)
	if isDoc {
		maxBytes = maxDocExtractBytes
	}
	probe := "n=$(wc -c < '" + q + "' 2>/dev/null) || exit 1; if [ \"$n\" -le " + fmt.Sprintf("%d", maxBytes) + " ]; then base64 -w0 '" + q + "'; else echo OVERSIZE; fi"
	if out, _, code, err := mgr.Exec(ctx, sessionID, "sh", []string{"-c", probe}, sandboxWorkdir); err == nil && code == 0 {
		out = strings.TrimSpace(out)
		if out == "OVERSIZE" {
			return fmt.Sprintf("Error reading file: exceeds the %d-byte sandbox read limit. Use the shell tool (head/sed/python) for targeted extraction.", maxBytes), "", ""
		}
		if out != "" {
			if raw, derr := base64.StdEncoding.DecodeString(out); derr == nil && len(raw) > 0 {
				if mime, ok := imageSniffMime(raw); ok {
					return "", base64.StdEncoding.EncodeToString(raw), mime
				}
				if isDoc {
					if text, ok := extractDocumentText(raw, pathBase(path)); ok {
						return formatDocumentRead(text, path, sessionID, offset, limit, raw), "", ""
					}
					if looksBinarySample(raw) {
						return "Error reading file: binary files are not supported by this tool.", "", ""
					}
				}
			}
		}
	}
	content, err := mgr.ReadFile(ctx, sessionID, path)
	if err != nil {
		return "Error reading file: " + err.Error(), "", ""
	}
	return windowedFileRead(content, fmt.Sprintf("Content of %s:", path), offset, limit), "", ""
}

// pathExt/pathBase work on POSIX sandbox paths regardless of host OS.
func pathExt(p string) string { return filepath.Ext(strings.ReplaceAll(p, "\\", "/")) }
func pathBase(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func sandboxFileWrite(ctx context.Context, mgr *sandbox.Manager, sessionID, path, content string) string {
	path = sandboxResolvePath(path)
	if path == "" {
		return "Error writing file: `path` must be a non-empty string."
	}
	if err := mgr.WriteFile(ctx, sessionID, path, content); err != nil {
		return "Error writing file: " + err.Error()
	}
	return "File written successfully: " + path
}

// stageFileIntoSandbox 把宿主附件自动复制进沙盒 /workspace（同名复用），返回沙盒路径；非沙盒模式/未配置/任何一步失败返回 ""（调用方回退宿主路径+引导文案）。
func (s *ProcessStage) stageFileIntoSandbox(ctx context.Context, sessionID, hostPath string, sandboxMode bool) string {
	if !sandboxMode || s.sandboxMgr == nil || strings.TrimSpace(hostPath) == "" {
		return ""
	}
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return ""
	}
	dst := sandboxWorkdir + "/" + filepath.Base(hostPath)
	if err := s.sandboxMgr.WriteFile(ctx, sessionID, dst, string(data)); err != nil {
		return ""
	}
	return dst
}

// sandboxNeoBackend reports whether the sandbox booter config targets shipyard_neo (the only backend with browser/neo-skill APIs). Mirrors py _SHIPYARD_NEO_TOOL_CONFIG matching.
func sandboxNeoBackend(config map[string]interface{}) bool {
	ps, _ := config["provider_settings"].(map[string]interface{})
	sb, _ := ps["sandbox"].(map[string]interface{})
	bt, _ := sb["booter"].(string)
	bt = strings.ToLower(strings.TrimSpace(bt))
	return bt == "" || bt == "shipyard_neo"
}

// neoToolAvailability decides browser/neo tool exposure (py astr_main_agent conservative rule: capabilities unknown → register browser; shipyard_neo always registers neo lifecycle tools).
func (s *ProcessStage) neoToolAvailability(umo string) (browser, neo bool) {
	if s.sandboxMgr == nil || !sandboxNeoBackend(s.config) {
		return false, false
	}
	caps := s.sandboxMgr.SessionCapsNow(umo)
	if caps == nil {
		return true, true
	}
	for _, c := range caps {
		if c == "browser" {
			return true, true
		}
	}
	return false, true
}

// computerAdminDenied mirrors Python check_admin_permission for computer-use tools: with provider_settings.computer_use_require_admin (default true) only admins may execute them.
func (s *ProcessStage) computerAdminDenied(event *core.Event, operation string) string {
	requireAdmin := true
	if s.providerConf != nil && s.providerConf.ComputerUseRequireAdmin != nil {
		requireAdmin = *s.providerConf.ComputerUseRequireAdmin
	}
	if !requireAdmin || event.Role == "admin" {
		return ""
	}
	return "error: Permission denied. " + operation + " is only allowed for admin users. Tell user to set admins in `AstrBot WebUI -> Config -> General Config` by adding their user ID to the admins list if they need this feature. User's ID is: " + event.GetSenderID() + ". User's ID can be found by using /sid command."
}

// sandboxUploadFile transfers a file FROM the host machine INTO the sandbox
// workspace so sandbox tools (read/shell/python) can access it. Mirrors
// astrbot-py's FileUploadTool (astrbot_upload_file): local_path is an absolute
// host path, and the file is written into the sandbox working directory using
// its basename.
func sandboxUploadFile(ctx context.Context, mgr *sandbox.Manager, sessionID, localPath string) string {
	if strings.TrimSpace(localPath) == "" {
		return "Error uploading file: `local_path` must be a non-empty string."
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return "Error: File does not exist: " + localPath
	}
	if info.IsDir() {
		return "Error: Path is not a file: " + localPath
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "Error uploading file: " + err.Error()
	}
	name := filepath.Base(localPath)
	dst := sandboxWorkdir + "/" + name
	if err := mgr.WriteFile(ctx, sessionID, dst, string(data)); err != nil {
		return "Error uploading file: " + err.Error()
	}
	return "File uploaded successfully to " + dst + " (size: " + fmt.Sprintf("%d", len(data)) + " bytes). " +
		"Use astrbot_file_read_tool to read it."
}

// sandboxDownloadFile transfers a file FROM the sandbox OUT to the host,
// writing it into the host temp directory and returning the local path.
// Mirrors astrbot-py's FileDownloadTool (astrbot_download_file): remote_path
// is the path inside the sandbox to copy out.
func sandboxDownloadFile(ctx context.Context, mgr *sandbox.Manager, sessionID, remotePath string) string {
	if strings.TrimSpace(remotePath) == "" {
		return "Error downloading file: `remote_path` must be a non-empty string."
	}
	content, err := mgr.ReadFile(ctx, sessionID, remotePath)
	if err != nil {
		return "Error downloading file: " + err.Error()
	}
	name := filepath.Base(strings.TrimSuffix(sandboxResolvePath(remotePath), "/"))
	if name == "" || name == "." || name == "/" {
		name = "downloaded_file"
	}
	tmp, err := os.CreateTemp("", "astrbot-sandbox-*"+name)
	if err != nil {
		return "Error downloading file: " + err.Error()
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	if err := os.WriteFile(tmpName, []byte(content), 0o600); err != nil {
		os.Remove(tmpName)
		return "Error downloading file: " + err.Error()
	}
	return "File downloaded successfully to " + tmpName + " (size: " + fmt.Sprintf("%d", len(content)) + " bytes)."
}

// validHTTPURL reports whether raw is a parseable http(s) URL with a non-empty
// host. OneBot 的 get_group_file_url 可能返回空 host 的伪 URL（如
// "https:///ftn_handler//?fname="），这类链接不可下载，必须提前拦截。
func validHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func sandboxFileEdit(ctx context.Context, mgr *sandbox.Manager, sessionID, path, old, new string, replaceAll bool) string {
	path = sandboxResolvePath(path)
	if path == "" || old == "" {
		return "Error editing file: `path` and `old` must be non-empty."
	}
	content, err := mgr.ReadFile(ctx, sessionID, path)
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
	if err := mgr.WriteFile(ctx, sessionID, path, content); err != nil {
		return "Error editing file: " + err.Error()
	}
	modeText := "first match"
	if replaceAll {
		modeText = "all matches"
	}
	return fmt.Sprintf("Edited %s. Replaced %d occurrence(s) using %s mode.", path, replacements, modeText)
}

func sandboxGrep(ctx context.Context, mgr *sandbox.Manager, sessionID, pattern, path string) string {
	if strings.TrimSpace(pattern) == "" {
		return "Error: `pattern` must be a non-empty string."
	}
	if path == "" {
		path = "."
	}
	stdout, stderr, code, err := mgr.Exec(ctx, sessionID, "sh", []string{"-c", "grep -rn " + "'" + strings.ReplaceAll(pattern, "'", "'\\''") + "' " + "'" + strings.ReplaceAll(path, "'", "'\\''") + "' 2>/dev/null"}, sandboxWorkdir)
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
	}, pluginSkillRoots()...)
}

// pluginSkillRoots lists <plugin>/skills dirs shipped with installed plugins (py _plugin_skill_roots: members may read plugin SKILL.md, not plugin source/config).
func pluginSkillRoots() []string {
	var out []string
	entries, err := os.ReadDir(filepath.Join("data", "plugins"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillsDir := filepath.Join("data", "plugins", e.Name(), "skills")
		if st, err := os.Stat(skillsDir); err == nil && st.IsDir() {
			out = append(out, skillsDir)
		}
	}
	return out
}

// restrictedPathLabels mirrors py _restricted_env_path_labels: human-readable allowed dirs for the denial message.
func restrictedPathLabels(umo string, write bool) []string {
	if !write {
		return []string{"data/skills", "data/plugins/*/skills", workspaceRoot(umo), filepath.Join(os.TempDir(), ".astrbot"), filepath.Join("data", "temp")}
	}
	return []string{workspaceRoot(umo), filepath.Join(os.TempDir(), ".astrbot"), filepath.Join("data", "temp")}
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

// resolveLocalPath resolves a tool-supplied path relative to the workspace root, expanding ~ and normalizing to an absolute path. enforce (py _is_restricted_env: local runtime + require_admin + non-admin role) additionally restricts the result to the allowed roots and rejects multi-hardlink aliases; admins keep the unrestricted py behavior.
func resolveLocalPath(path, umo string, write, enforce bool) (string, error) {
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
	if !enforce {
		return resolved, nil
	}
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
			if err := rejectMultiLinkFile(resolved); err != nil {
				return "", err
			}
			return resolved, nil
		}
	}
	access := "Read"
	if write {
		access = "Write"
	}
	return "", fmt.Errorf("%s access is restricted for this user. Allowed directories: %s. Blocked path: %s.", access, strings.Join(restrictedPathLabels(umo, write), ", "), resolved)
}

// rejectMultiLinkFile blocks regular files with nlink>1 in restricted mode: a hardlink could alias content from outside the allowed roots (py _reject_multi_link_file). Platform nlink lookup lives in filelink_{unix,windows}.go.
func rejectMultiLinkFile(path string) error {
	nlink, regular, err := fileHardlinks(path)
	if err != nil {
		return fmt.Errorf("Access denied: unable to inspect restricted path link count. Blocked path: %s.", path)
	}
	if regular && nlink > 1 {
		return fmt.Errorf("Access denied: file has multiple hard links and may alias content outside allowed directories. Link count: %d. Blocked path: %s.", nlink, path)
	}
	return nil
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
		"Prefer relative paths within `/workspace`. " +
		"The sandbox container has network access, so you can fetch remote files inside it " +
		"with `curl`/`wget`. User-sent/quoted files are downloaded to the host and shown in " +
		"context as `[File Attachment: name X, path Y]`; use `astrbot_upload_file` to copy " +
		"the host file into the sandbox before reading it."
}

// collectSandboxTools builds the OpenAI tool schemas for the sandbox runtime.
func collectSandboxTools(browser, neo bool) []map[string]interface{} {
	return collectComputerTools(true, browser, neo)
}

// collectLocalTools builds the OpenAI tool schemas for the local runtime.
func collectLocalTools() []map[string]interface{} {
	return collectComputerTools(false, false, false)
}

// collectComputerTools builds the computer-use tool schemas. 沙盒与本地运行时
// 使用同一组工具但语义不同：沙盒内 shell/python 在隔离容器 /workspace 执行、
// 且容器有网络可 curl 下载；本地运行时的描述则强调直接操作宿主机需谨慎。
func collectComputerTools(sandbox, browser, neo bool) []map[string]interface{} {
	shellDesc := "The shell command to execute in the current runtime shell (for example, powershell.exe on Windows). Equivalent to `cd {working_dir} && {command}` where {working_dir} is the conversation workspace. If the output is very large it will be truncated with a preview and the FULL output saved to a file — read that file in windows (astrbot_file_read_tool offset/limit) or grep it, and summarize each segment before continuing. 注意：该命令直接在宿主机（运行 AstrBot 的服务器）上执行，未被沙箱隔离，请谨慎使用，避免破坏性或危险操作。当用户发送或引用了文件消息（上下文里显示为 [File Attachment ...]）时，请先使用 astrbot_upload_file 把文件上传到工作区再处理，而不是自行猜测文件路径。"
	pythonDesc := "Execute codes in a Python environment. Current OS: " + runtime.GOOS + ". Use system-compatible commands. 注意：该代码直接在宿主机（运行 AstrBot 的服务器）上执行，未被沙箱隔离，请谨慎使用，避免破坏性或危险操作。"
	uploadDesc := "Transfer a file FROM the host machine INTO the current runtime so that code can access it. Use this when the user sends/attaches a file and you need to process it. The local_path must point to an existing file on the host filesystem (e.g. a [File Attachment: ...] path shown in the context)."
	downloadDesc := "Transfer a file OUT of the current runtime to the host machine. Use this only when the user asks to retrieve/export a file that was created or modified inside the runtime."
	if sandbox {
		shellDesc = "Execute a shell command inside the sandbox container. Working directory is `/workspace`; relative paths resolve there. The sandbox has network access, so you can download files directly with curl/wget, e.g. `curl -L -o bug.md '<url>'`. If the output is very large it is truncated with a preview and the FULL output saved in the sandbox workspace — read it in windows with astrbot_file_read_tool (offset/limit) or search with astrbot_grep_tool, digesting each segment before continuing. 当用户发送或引用了文件消息（上下文显示为 [File Attachment ...]）时，请先用 astrbot_upload_file 把宿主侧的文件路径上传到 /workspace 再读取分析，而不是自行猜测文件路径或到处 ls 找文件。"
		pythonDesc = "Execute Python code inside the sandbox container. An IPython kernel is used and variables/state persist across calls within the same sandbox session. Working directory is `/workspace`."
		uploadDesc = "Transfer a file FROM the host machine INTO the sandbox so that sandbox code can access it. Use this when the user sends/attaches a file (shown in context as [File Attachment: name X, path Y]) and you need to process it inside the sandbox. The local_path must be the host path from the [File Attachment ...] line."
		downloadDesc = "Transfer a file FROM the sandbox OUT to the host. Use this ONLY when the user asks to retrieve/export a file that was created or modified inside the sandbox."
	}

	schemas := []map[string]interface{}{
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "astrbot_execute_shell",
				"description": shellDesc,
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
				"description": pythonDesc,
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
				"description": "read file content. Supports text, image, and PDF (text extraction), docx, xlsx, pptx and epub files. Text reads are windowed: up to 2000 lines / 50 KB per call, output is line-numbered and ends with a continuation hint (use offset=<line> for the next window); do NOT try to read huge files at once — read and digest one window at a time, or use astrbot_grep_tool to locate content first.",
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
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "astrbot_upload_file",
				"description": uploadDesc,
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"local_path": map[string]interface{}{
							"type":        "string",
							"description": "Absolute path to the file on the host filesystem that will be copied into the sandbox.",
						},
					},
					"required": []interface{}{"local_path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "astrbot_download_file",
				"description": downloadDesc,
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"remote_path": map[string]interface{}{
							"type":        "string",
							"description": "Path of the file inside the sandbox to copy out to the host.",
						},
					},
					"required": []interface{}{"remote_path"},
				},
			},
		},
	}
	schemas = append(schemas, browserToolSchemas(browser)...)
	schemas = append(schemas, neoSkillToolSchemas(neo)...)
	return schemas
}

// browserToolSchemas builds the 3 browser automation tool schemas (py shipyard_neo/browser.py), exposed only when the sandbox profile advertises the browser capability (conservatively exposed when capabilities are not yet known).
func browserToolSchemas(browser bool) []map[string]interface{} {
	if !browser {
		return nil
	}
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	boolp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	intp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc}
	}
	common := map[string]interface{}{
		"description":   str("Optional execution description."),
		"tags":          str("Optional tags."),
		"learn":         boolp("Whether to mark execution as learn evidence."),
		"include_trace": boolp("Whether to include trace_ref in response."),
	}
	wrap := func(name, desc string, props map[string]interface{}, required []string) map[string]interface{} {
		p := map[string]interface{}{}
		for k, v := range props {
			p[k] = v
		}
		for k, v := range common {
			p[k] = v
		}
		req := make([]interface{}, 0, len(required))
		for _, r := range required {
			req = append(req, r)
		}
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": desc,
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": p,
					"required":   req,
				},
			},
		}
	}
	return []map[string]interface{}{
		wrap("astrbot_execute_browser", "Execute one browser automation command in the sandbox.", map[string]interface{}{
			"cmd":     str("Browser command to execute."),
			"timeout": intp("Execution timeout in seconds (1-300, default 30)."),
		}, []string{"cmd"}),
		wrap("astrbot_execute_browser_batch", "Execute a browser command batch in the sandbox.", map[string]interface{}{
			"commands": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "Ordered browser commands.",
			},
			"timeout":       intp("Overall timeout in seconds for all commands (default 60)."),
			"stop_on_error": boolp("Whether to stop on first failure (default true)."),
		}, []string{"commands"}),
		wrap("astrbot_run_browser_skill", "Run a released browser skill in the sandbox by skill_key.", map[string]interface{}{
			"skill_key":     str("Released browser skill key."),
			"timeout":       intp("Overall timeout in seconds (default 60)."),
			"stop_on_error": boolp("Whether to stop on first failure (default true)."),
		}, []string{"skill_key"}),
	}
}

// neoSkillToolSchemas builds the 11 Neo skill lifecycle tool schemas (py shipyard_neo/neo_skills.py), exposed only for the shipyard_neo sandbox backend.
func neoSkillToolSchemas(neo bool) []map[string]interface{} {
	if !neo {
		return nil
	}
	wrap := func(name, desc string, props map[string]interface{}, required []string) map[string]interface{} {
		req := make([]interface{}, 0, len(required))
		for _, r := range required {
			req = append(req, r)
		}
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": name, "description": desc,
				"parameters": map[string]interface{}{"type": "object", "properties": props, "required": req},
			},
		}
	}
	str := func(d string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": d}
	}
	num := func(d string) map[string]interface{} {
		return map[string]interface{}{"type": "number", "description": d}
	}
	bl := func(d string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": d}
	}
	obj := func(d string) map[string]interface{} {
		return map[string]interface{}{"type": "object", "description": d}
	}
	return []map[string]interface{}{
		wrap("astrbot_get_execution_history", "List sandbox execution history records (browser/shell runs) for this session.", map[string]interface{}{
			"exec_type": str("Filter by execution type (e.g. browser)."),
			"limit":     map[string]interface{}{"type": "integer", "description": "Page size (default 20)."},
			"offset":    map[string]interface{}{"type": "integer", "description": "Page offset."},
			"tags":      str("Filter by tags."),
		}, []string{}),
		wrap("astrbot_annotate_execution", "Annotate one execution history record (description/tags/notes) so later skill mining can use it.", map[string]interface{}{
			"execution_id": str("Execution id from get_execution_history."),
			"description":  str("Optional description."),
			"tags":         str("Optional tags."),
			"notes":        str("Optional notes."),
		}, []string{"execution_id"}),
		wrap("astrbot_create_skill_payload", "Store canonical skill payload (JSON with skill_markdown etc.) and return payload_ref.", map[string]interface{}{
			"payload": obj("The payload object to store."),
			"kind":    str("Payload kind (default 'skill')."),
		}, []string{"payload"}),
		wrap("astrbot_get_skill_payload", "Fetch a stored skill payload by payload_ref.", map[string]interface{}{
			"payload_ref": str("Payload reference returned by create_skill_payload."),
		}, []string{"payload_ref"}),
		wrap("astrbot_create_skill_candidate", "Create a Neo skill candidate from payload + source execution ids.", map[string]interface{}{
			"skill_key":            str("Stable skill key."),
			"payload_ref":          str("Optional payload reference."),
			"source_execution_ids": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Evidence execution ids."},
			"scenario_key":         str("Optional scenario key."),
			"summary":              str("Optional candidate summary."),
			"usage_notes":          str("Optional usage notes."),
		}, []string{"skill_key"}),
		wrap("astrbot_list_skill_candidates", "List Neo skill candidates (filter by status/skill_key).", map[string]interface{}{
			"status":    str("Filter by status (proposed/evaluating/released/rejected)."),
			"skill_key": str("Filter by skill key."),
			"limit":     map[string]interface{}{"type": "integer", "description": "Page size (default 20)."},
			"offset":    map[string]interface{}{"type": "integer", "description": "Page offset."},
		}, []string{}),
		wrap("astrbot_evaluate_skill_candidate", "Record an evaluation result (passed/score/report) for a candidate.", map[string]interface{}{
			"candidate_id": str("Candidate id."),
			"passed":       bl("Whether the evaluation passed."),
			"score":        num("Optional numeric score."),
			"benchmark_id": str("Optional benchmark id."),
			"report":       str("Optional evaluation report."),
		}, []string{"candidate_id", "passed"}),
		wrap("astrbot_promote_skill_candidate", "Promote a candidate to a release (stage canary/stable). For stable set sync_to_local=true to sync SKILL.md.", map[string]interface{}{
			"candidate_id":  str("Candidate id."),
			"stage":         str("Release stage: canary or stable (default stable)."),
			"sync_to_local": bl("Also sync payload skill_markdown into the local skills directory (stable)."),
		}, []string{"candidate_id"}),
		wrap("astrbot_list_skill_releases", "List Neo skill releases (filter by skill_key/stage; active_only skips rolled-back).", map[string]interface{}{
			"skill_key":   str("Filter by skill key."),
			"stage":       str("Filter by stage (canary/stable)."),
			"active_only": bl("Only active releases (default true)."),
			"limit":       map[string]interface{}{"type": "integer", "description": "Page size (default 20)."},
			"offset":      map[string]interface{}{"type": "integer", "description": "Page offset."},
		}, []string{}),
		wrap("astrbot_rollback_skill_release", "Roll back an active skill release.", map[string]interface{}{
			"release_id": str("Release id."),
		}, []string{"release_id"}),
		wrap("astrbot_sync_skill_release", "Re-sync a release to the local skill workspace (by release_id or skill_key; require_stable gates to stable stage).", map[string]interface{}{
			"release_id":     str("Release id (preferred)."),
			"skill_key":      str("Skill key when release_id is absent."),
			"require_stable": bl("Only sync stable-stage releases (default false)."),
		}, []string{}),
	}
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

	// pollMu guards the incremental read cursor (py poll_session cursor semantics: every byte is delivered exactly once; nothing is skipped or replayed).
	pollMu     sync.Mutex
	pollCursor int64

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
	outCap := &spoolWriter{max: maxShellOutput, dir: filepath.Join(ws, ".astrbot-outputs")}
	cmd.Stdout = outCap
	cmd.Stderr = outCap
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
	out := outCap.Head()
	if sp := outCap.Close(); sp != "" {
		out = append(out, []byte(spoolNotice(sp, outCap.total))...)
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
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	chunk := ""
	size := int64(0)
	if st, err := os.Stat(s.OutputFile); err == nil {
		size = st.Size()
	}
	if s.pollCursor > size {
		s.pollCursor = 0 // log rotated/replaced: restart the stream
	}
	if size > s.pollCursor {
		// #nosec G304 -- OutputFile is a host-generated session log path.
		if f, err := os.Open(s.OutputFile); err == nil {
			defer f.Close()
			if _, err := f.Seek(s.pollCursor, io.SeekStart); err == nil {
				buf := make([]byte, pollWindowBytes)
				if n, _ := f.Read(buf); n > 0 {
					if s.pollCursor+int64(n) < size {
						if li := bytes.LastIndexByte(buf[:n], '\n'); li >= 0 {
							n = li + 1 // stop at a line boundary so the next poll continues cleanly
						}
					}
					s.pollCursor += int64(n)
					chunk = string(buf[:n])
				}
			}
		}
	}
	st, _ := json.Marshal(s.status())
	out := string(st) + "\nOutput:\n" + chunk
	if s.pollCursor < size {
		out += fmt.Sprintf("\n\n(output continues: %d of %d bytes consumed — poll again for the next segment, or read `%s` with astrbot_file_read_tool offset/limit)", s.pollCursor, size, s.OutputFile)
	}
	return out
}

// pollWindowBytes bounds one shell_session poll segment (py max_output_chars=10000, doubled for multibyte safety).
const pollWindowBytes = 20000

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
	// 与同步 shell 路径一致：输出限幅 + 全量 spool 落盘（超限不丢数据）。
	cw := &spoolWriter{max: maxShellOutput, dir: filepath.Join(ws, ".astrbot-outputs")}
	cmd.Stdout = cw
	cmd.Stderr = cw
	if err := cmd.Start(); err != nil {
		return "error: code execution failed to start: " + err.Error()
	}
	waitErr := cmd.Wait()
	out := string(cw.Head())
	if sp := cw.Close(); sp != "" {
		out += spoolNotice(sp, cw.total)
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

func executeFileRead(path, umo string, offset, limit int, restricted bool) (string, string, string) {
	resolved, err := resolveLocalPath(path, umo, false, restricted)
	if err != nil {
		return "Error reading file: " + err.Error(), "", ""
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "Error reading file: " + err.Error(), "", ""
	}
	if info.IsDir() {
		return fmt.Sprintf("Error: '%s' is a directory, not a file. "+
			"Use a file path instead, or use 'astrbot_execute_shell' to list directory contents.", resolved), "", ""
	}
	// 图片嗅探（py read_file_utils probe→image 分支）：命中则整体读入并以 base64 交给多模态通道。
	if mime, ok := sniffLocalImage(resolved); ok {
		if info.Size() > maxToolImageBytes {
			return fmt.Sprintf("Error reading file: image %s is %d bytes, exceeds the %d-byte image read limit.", resolved, info.Size(), maxToolImageBytes), "", ""
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return "Error reading file: " + err.Error(), "", ""
		}
		return "", base64.StdEncoding.EncodeToString(data), mime
	}
	// 文档抽取（pdf/docx/xlsx/pptx/epub/xls）复用知识库解析层（py probe→_parse_local_supported_document）。
	if docExtSet[strings.ToLower(filepath.Ext(resolved))] {
		if info.Size() > maxDocExtractBytes {
			return fmt.Sprintf("Error reading file: document %s is %d bytes, exceeds the %d-byte document read limit.", resolved, info.Size(), maxDocExtractBytes), "", ""
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return "Error reading file: " + err.Error(), "", ""
		}
		if text, ok := extractDocumentText(data, filepath.Base(resolved)); ok {
			return formatDocumentRead(text, resolved, umo, offset, limit, data), "", ""
		}
		return "Error reading file: binary files are not supported by this tool.", "", ""
	}
	// 大文件（>整读上限）流式窗口：只扫到 offset+窗口行数，避免载入全文件。
	if info.Size() > maxFileReadBytes {
		effLimit := limit
		if effLimit <= 0 {
			effLimit = defaultReadLines
		}
		f, err := os.Open(resolved)
		if err != nil {
			return "Error reading file: " + err.Error(), "", ""
		}
		defer f.Close()
		var out []string
		more := false
		cut := false
		bytes := 0
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for i := 0; sc.Scan(); i++ {
			if i < offset {
				continue
			}
			if len(out) >= effLimit {
				more = true
				break
			}
			line := sc.Text()
			if r := []rune(line); len(r) > maxReadLineChars {
				line = string(r[:maxReadLineChars]) + fmt.Sprintf("... (line truncated to %d chars)", maxReadLineChars)
			}
			size := len(line) + 3
			if bytes > 0 {
				size++
			}
			if bytes+size > maxReadWindowBytes {
				cut = true
				break
			}
			out = append(out, fmt.Sprintf("%d: %s", i+1, line))
			bytes += size
		}
		header := fmt.Sprintf("Read %s (%d bytes):", resolved, info.Size())
		last := offset + len(out)
		body := strings.Join(out, "\n")
		switch {
		case cut:
			return header + "\n" + body + fmt.Sprintf("\n\n(Output capped at %d KB. Showing lines %d-%d. Use offset=%d to continue.)", maxReadWindowBytes/1024, offset+1, last, last), "", ""
		case more:
			return header + "\n" + body + fmt.Sprintf("\n\n(Showing lines %d-%d. Use offset=%d to continue.)", offset+1, last, last), "", ""
		default:
			return header + "\n" + body + fmt.Sprintf("\n\n(End of file - total %d lines)", last), "", ""
		}
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "Error reading file: " + err.Error(), "", ""
	}
	if looksBinarySample(data) {
		return "Error reading file: binary files are not supported by this tool.", "", ""
	}
	return windowedFileRead(string(data), fmt.Sprintf("Read %s (%d bytes):", resolved, info.Size()), offset, limit), "", ""
}

func executeFileWrite(path, content, umo string, restricted bool) string {
	resolved, err := resolveLocalPath(path, umo, true, restricted)
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

// executeLocalUpload copies a host file into the conversation workspace on the
// local runtime (the workspace IS the host filesystem). Mirrors Python's
// ast rbot_upload_file for the local computer-use runtime.
func executeLocalUpload(localPath, umo string, restricted bool) string {
	if strings.TrimSpace(localPath) == "" {
		return "Error uploading file: `local_path` must be a non-empty string."
	}
	info, err := os.Stat(localPath)
	if err != nil || info.IsDir() {
		return "Error: File does not exist or is not a file: " + localPath
	}
	name := filepath.Base(localPath)
	dst := filepath.Join(workspaceRoot(umo), name)
	if err := copyLocalFile(localPath, dst); err != nil {
		return "Error uploading file: " + err.Error()
	}
	return "File uploaded successfully to " + dst
}

// executeLocalDownload copies a workspace file out to the host temp directory
// on the local runtime (the workspace IS the host filesystem). Mirrors Python's
// ast rbot_download_file for the local computer-use runtime.
func executeLocalDownload(remotePath, umo string, restricted bool) string {
	if strings.TrimSpace(remotePath) == "" {
		return "Error downloading file: `remote_path` must be a non-empty string."
	}
	resolved, err := resolveLocalPath(remotePath, umo, false, restricted)
	if err != nil {
		return "Error downloading file: " + err.Error()
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "Error downloading file: " + err.Error()
	}
	name := filepath.Base(resolved)
	tmp, err := os.CreateTemp("", "astrbot-sandbox-*"+name)
	if err != nil {
		return "Error downloading file: " + err.Error()
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	if err := os.WriteFile(tmpName, data, 0o600); err != nil {
		os.Remove(tmpName)
		return "Error downloading file: " + err.Error()
	}
	return "File downloaded successfully to " + tmpName + " (size: " + fmt.Sprintf("%d", len(data)) + " bytes)."
}

// copyLocalFile copies src to dst, preserving the source file mode.
func copyLocalFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

func executeFileEdit(path, old, new string, replaceAll bool, umo string, restricted bool) string {
	resolved, err := resolveLocalPath(path, umo, true, restricted)
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

func executeGrep(pattern, path, glob string, resultLimit int, umo string, restricted bool) string {
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
	resolved, err := resolveLocalPath(searchPath, umo, false, restricted)
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

// argStringOpt returns (value, present) for an optional string arg.
func argStringOpt(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, t != ""
	case nil:
		return "", false
	default:
		return fmt.Sprint(t), true
	}
}

func jsonOrError(data map[string]interface{}, err error) string {
	if err != nil {
		return "Error: " + err.Error()
	}
	b, jerr := json.Marshal(data)
	if jerr != nil {
		return fmt.Sprint(data)
	}
	return string(b)
}

func jsonTextOrError(text string, err error) string {
	if err != nil {
		return "Error: " + err.Error()
	}
	return text
}

// browserBody builds the Bay browser exec request body (SDK _BrowserExecRequest semantics: omit empty optional strings, always send learn/include_trace/timeout).
func browserBody(args map[string]interface{}, defaults map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{}
	for k, def := range defaults {
		body[k] = def
	}
	for _, k := range []string{"cmd", "description", "tags"} {
		if v, ok := argStringOpt(args, k); ok {
			body[k] = v
		}
	}
	if v, ok := args["timeout"]; ok {
		body["timeout"] = v
	} else {
		body["timeout"] = defaults["timeout"]
	}
	for _, k := range []string{"learn", "include_trace", "stop_on_error"} {
		if v, ok := args[k].(bool); ok {
			body[k] = v
		} else if _, exists := defaults[k]; !exists {
			body[k] = false
		}
	}
	if raw, ok := args["commands"].([]interface{}); ok {
		cmds := make([]string, 0, len(raw))
		for _, r := range raw {
			if cmds2, ok := r.(string); ok && strings.TrimSpace(cmds2) != "" {
				cmds = append(cmds, cmds2)
			}
		}
		body["commands"] = cmds
	}
	return body
}

// executeNeoLifecycleTool dispatches the 11 Neo skill lifecycle tools: payload/candidate/release go through the host NeoStore (same instance the dashboard API uses), execution history/annotate go through the session's Bay sandbox.
func (s *ProcessStage) executeNeoLifecycleTool(ctx context.Context, sessionID, name string, args map[string]interface{}) string {
	switch name {
	case "astrbot_execute_browser":
		data, err := s.sandboxMgr.BrowserExec(ctx, sessionID, browserBody(args, map[string]interface{}{"timeout": 30, "learn": false, "include_trace": false}), argInt(args, "timeout", 30))
		return browserResult("browser command", data, err)
	case "astrbot_execute_browser_batch":
		data, err := s.sandboxMgr.BrowserExecBatch(ctx, sessionID, browserBody(args, map[string]interface{}{"timeout": 60, "stop_on_error": true, "learn": false, "include_trace": false}))
		return browserResult("browser batch", data, err)
	case "astrbot_run_browser_skill":
		key := argString(args, "skill_key")
		if strings.TrimSpace(key) == "" {
			return "Error running browser skill: `skill_key` is required."
		}
		data, err := s.sandboxMgr.BrowserRunSkill(ctx, sessionID, key, browserBody(args, map[string]interface{}{"timeout": 60, "stop_on_error": true, "include_trace": false}))
		return browserResult("browser skill", data, err)
	case "astrbot_get_execution_history":
		params := map[string]string{}
		for _, k := range []string{"exec_type", "tags"} {
			if v, ok := argStringOpt(args, k); ok {
				params[k] = v
			}
		}
		if v := argInt(args, "limit", 0); v > 0 {
			params["limit"] = strconv.Itoa(v)
		}
		if v := argInt(args, "offset", 0); v > 0 {
			params["offset"] = strconv.Itoa(v)
		}
		data, err := s.sandboxMgr.GetExecutionHistory(ctx, sessionID, params)
		return jsonTextOrError(data, err)
	case "astrbot_annotate_execution":
		eid := argString(args, "execution_id")
		if strings.TrimSpace(eid) == "" {
			return "Error: `execution_id` is required."
		}
		body := map[string]interface{}{}
		for _, k := range []string{"description", "tags", "notes"} {
			if v, ok := argStringOpt(args, k); ok {
				body[k] = v
			}
		}
		if len(body) == 0 {
			return "Error: provide at least one of description/tags/notes."
		}
		data, err := s.sandboxMgr.AnnotateExecution(ctx, sessionID, eid, body)
		return jsonTextOrError(data, err)
	}
	// Host-side lifecycle (payload/candidate/release) — needs the shared NeoStore.
	if s.neoStore == nil {
		return "Error: Neo skill lifecycle store is not initialized."
	}
	switch name {
	case "astrbot_create_skill_payload":
		data, err := s.neoStore.PutPayload(args["payload"], argString(args, "kind"))
		return jsonOrError(data, err)
	case "astrbot_get_skill_payload":
		data, err := s.neoStore.GetPayload(argString(args, "payload_ref"))
		return jsonOrError(data, err)
	case "astrbot_create_skill_candidate":
		ids := []string{}
		if raw, ok := args["source_execution_ids"].([]interface{}); ok {
			for _, r := range raw {
				if sv, ok := r.(string); ok {
					ids = append(ids, sv)
				}
			}
		}
		c, err := s.neoStore.AddCandidate(argString(args, "skill_key"), ids, argString(args, "scenario_key"), argString(args, "payload_ref"), argString(args, "summary"), argString(args, "usage_notes"))
		if err != nil {
			return "Error: " + err.Error()
		}
		b, jerr := json.Marshal(c)
		if jerr != nil {
			return fmt.Sprint(c)
		}
		return string(b)
	case "astrbot_list_skill_candidates":
		return marshalable(s.neoStore.ListCandidates(argString(args, "status"), argString(args, "skill_key"), argInt(args, "limit", 20), argInt(args, "offset", 0)))
	case "astrbot_evaluate_skill_candidate":
		var score *float64
		if f, ok := args["score"].(float64); ok {
			score = &f
		}
		data, err := s.neoStore.EvaluateCandidate(argString(args, "candidate_id"), argBool(args, "passed"), score, argString(args, "benchmark_id"), argString(args, "report"))
		return jsonOrError(data, err)
	case "astrbot_promote_skill_candidate":
		data, err := s.neoStore.PromoteCandidate(argString(args, "candidate_id"), argString(args, "stage"), argBool(args, "sync_to_local"))
		return jsonOrError(data, err)
	case "astrbot_list_skill_releases":
		activeOnly := true
		if v, ok := args["active_only"].(bool); ok {
			activeOnly = v
		}
		return marshalable(s.neoStore.ListReleases(argString(args, "skill_key"), argString(args, "stage"), activeOnly, argInt(args, "limit", 20), argInt(args, "offset", 0)))
	case "astrbot_rollback_skill_release":
		r, err := s.neoStore.RollbackRelease(argString(args, "release_id"))
		if err != nil {
			return "Error: " + err.Error()
		}
		b, _ := json.Marshal(r)
		return string(b)
	case "astrbot_sync_skill_release":
		data, err := s.neoStore.SyncRelease(argString(args, "release_id"), argString(args, "skill_key"), argBool(args, "require_stable"))
		return jsonOrError(data, err)
	}
	return "Error: unknown neo skill tool " + name
}

func marshalable(v map[string]interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func browserResult(kind, data string, err error) string {
	if err != nil {
		return "Error executing " + kind + ": " + err.Error()
	}
	return data
}

// neoLifecycleToolSet marks tools dispatched through executeNeoLifecycleTool (host NeoStore + Bay history).
var neoLifecycleToolSet = map[string]bool{
	"astrbot_execute_browser": true, "astrbot_execute_browser_batch": true, "astrbot_run_browser_skill": true,
	"astrbot_get_execution_history": true, "astrbot_annotate_execution": true,
	"astrbot_create_skill_payload": true, "astrbot_get_skill_payload": true,
	"astrbot_create_skill_candidate": true, "astrbot_list_skill_candidates": true,
	"astrbot_evaluate_skill_candidate": true, "astrbot_promote_skill_candidate": true,
	"astrbot_list_skill_releases": true, "astrbot_rollback_skill_release": true, "astrbot_sync_skill_release": true,
}

// neoLifecycleHostSet: Bay-independent skill lifecycle tools (host NeoStore only).
var neoLifecycleHostSet = map[string]bool{
	"astrbot_create_skill_payload": true, "astrbot_get_skill_payload": true,
	"astrbot_create_skill_candidate": true, "astrbot_list_skill_candidates": true,
	"astrbot_evaluate_skill_candidate": true, "astrbot_promote_skill_candidate": true,
	"astrbot_list_skill_releases": true, "astrbot_rollback_skill_release": true, "astrbot_sync_skill_release": true,
}

// neoModePrompt mirrors Python's shipyard_neo-only system prompt blocks: workspace-relative path rule + the skill lifecycle workflow guidance.
func neoModePrompt() string {
	return "[Shipyard Neo File Path Rule]\n" +
		"When using sandbox filesystem tools (upload/download/read/write/list/delete), " +
		"always pass paths relative to the sandbox workspace root. " +
		"Example: use `baidu_homepage.png` instead of `/workspace/baidu_homepage.png`.\n\n" +
		"[Neo Skill Lifecycle Workflow]\n" +
		"When user asks to create/update a reusable skill in Neo mode, use lifecycle tools instead of directly writing local skill folders.\n" +
		"Preferred sequence:\n" +
		"1) Use `astrbot_create_skill_payload` to store canonical payload content and get `payload_ref`.\n" +
		"2) Use `astrbot_create_skill_candidate` with `skill_key` + `source_execution_ids` (and optional `payload_ref`) to create a candidate.\n" +
		"3) Use `astrbot_promote_skill_candidate` to release: `stage=canary` for trial; `stage=stable` for production.\n" +
		"For stable release, set `sync_to_local=true` to sync `payload.skill_markdown` into local `SKILL.md`.\n" +
		"Do not treat ad-hoc generated files as reusable Neo skills unless they are captured via payload/candidate/release.\n" +
		"To update an existing skill, create a new payload/candidate and promote a new release version; avoid patching old local folders directly.\n"
}

// computerUseRestricted mirrors Python fs._is_restricted_env: local-runtime file tools whitelist enforcement for non-admin users when provider_settings.computer_use_require_admin (default true). Call sites must gate on runtime=="local" themselves.
func (s *ProcessStage) computerUseRestricted(event *core.Event) bool {
	requireAdmin := true
	if s.providerConf != nil && s.providerConf.ComputerUseRequireAdmin != nil {
		requireAdmin = *s.providerConf.ComputerUseRequireAdmin
	}
	return requireAdmin && event.Role != "admin"
}

// imageSniffMime detects common image formats from magic bytes (py file_read_utils _probe_file subset: image kinds).
func imageSniffMime(sample []byte) (string, bool) {
	switch {
	case len(sample) >= 8 && string(sample[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", true
	case len(sample) >= 3 && sample[0] == 0xFF && sample[1] == 0xD8 && sample[2] == 0xFF:
		return "image/jpeg", true
	case len(sample) >= 6 && (string(sample[:6]) == "GIF87a" || string(sample[:6]) == "GIF89a"):
		return "image/gif", true
	case len(sample) >= 12 && string(sample[:4]) == "RIFF" && string(sample[8:12]) == "WEBP":
		return "image/webp", true
	case len(sample) >= 2 && string(sample[:2]) == "BM":
		return "image/bmp", true
	}
	return "", false
}

// toolImageSinkKeys: pending images collected per event during a tool round (mirrors py ToolImageCache + from_cached_image flow).
const toolImageSinkKey = "tool_images_pending"

type pendingToolImage struct {
	Mime   string
	Base64 string
	Path   string
}

// registerToolImage caches a tool-produced image (compressed per provider settings) into data/temp/tool_images and stashes it on the event for the agent loop to inject as a follow-up user message; returns the py-compatible tool text.
func (s *ProcessStage) registerToolImage(event *core.Event, rawB64, mime, toolName string) string {
	data, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		return "Error reading file: image payload is empty."
	}
	dir := filepath.Join("data", "temp", "tool_images")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "Error reading file: failed to cache image: " + err.Error()
	}
	path := filepath.Join(dir, fmt.Sprintf("astrbot-toolimg-%d-%s", time.Now().UnixNano(), sanitizeFileName(toolName)))
	ext := ".img"
	switch mime {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	case "image/bmp":
		ext = ".bmp"
	}
	path += ext
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "Error reading file: failed to cache image: " + err.Error()
	}
	if compressed := s.compressImageForProvider(path); compressed != path {
		if cb, err := os.ReadFile(compressed); err == nil {
			data = cb
			mime = "image/jpeg" // compressImageForProvider 统一 JPEG 输出（透明压平白底，与 provider 图片通道同语义）。
		}
	}
	if event.Metadata == nil {
		event.Metadata = map[string]interface{}{}
	}
	pending, _ := event.Metadata[toolImageSinkKey].(*[]pendingToolImage)
	if pending == nil {
		pending = &[]pendingToolImage{}
		event.Metadata[toolImageSinkKey] = pending
	}
	*pending = append(*pending, pendingToolImage{Mime: mime, Base64: base64.StdEncoding.EncodeToString(data), Path: path})
	return fmt.Sprintf("Image returned and cached at path='%s'. Review the image below. Use send_message_to_user to send it to the user if satisfied, with type='image' and path='%s'.", path, path)
}

// providerSupportsImages mirrors py's modalities gate: empty list = unconfigured = assume image input is supported.
func providerSupportsImages(providerCfg map[string]interface{}) bool {
	mods := providerModalities(providerCfg)
	if len(mods) == 0 {
		return true
	}
	for _, m := range mods {
		if m == "image" {
			return true
		}
	}
	return false
}

// drainToolImages returns and clears the event's pending tool images.
func drainToolImages(event *core.Event) []pendingToolImage {
	if event.Metadata == nil {
		return nil
	}
	pending, _ := event.Metadata[toolImageSinkKey].(*[]pendingToolImage)
	if pending == nil || len(*pending) == 0 {
		return nil
	}
	out := *pending
	event.Metadata[toolImageSinkKey] = &[]pendingToolImage{}
	return out
}

func sanitizeFileName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "tool"
	}
	return string(out)
}

// maxToolImageBytes bounds raw image payloads captured from file tools (base64 inflates ~33%).
const maxToolImageBytes = 20 << 20

// sniffLocalImage reports the mime type when the first bytes of the file look like a supported image.
func sniffLocalImage(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", false
	}
	return imageSniffMime(buf[:n])
}

// docExtSet lists binary document extensions routed through the knowledge-base extractor (py probe → _parse_local_supported_document).
var docExtSet = map[string]bool{".pdf": true, ".docx": true, ".xlsx": true, ".pptx": true, ".epub": true, ".xls": true}

// maxDocExtractBytes bounds raw document bytes fed to the extractor (converted text is re-checked against maxFileReadBytes).
const maxDocExtractBytes = 64 << 20

// extractDocumentText reuses knowledgebase.ExtractKBText; false when the extension is not a binary document or extraction yields nothing (py: parse None → fall through to text/binary handling).
func extractDocumentText(data []byte, name string) (string, bool) {
	if !docExtSet[strings.ToLower(filepath.Ext(name))] {
		return "", false
	}
	text, err := knowledgebase.ExtractKBText(data, name, "")
	if err != nil || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// sliceTextLines returns lines[offset:offset+limit] like py _slice_text_by_lines.
func sliceTextLines(text string, offset, limit int) string {
	lines := strings.Split(text, "\n")
	if offset < 0 {
		offset = 0
	}
	if offset > len(lines) {
		return ""
	}
	end := len(lines)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return strings.Join(lines[offset:end], "\n")
}

// formatDocumentRead mirrors py _read_local_supported_document_result: text within the read limit is returned (line-sliced by offset/limit); larger text is stored under the workspace converted_files/ directory and a notice with that path is returned so the model can grep/read it in narrow windows.
func formatDocumentRead(text, resolved, umo string, offset, limit int, rawBytes []byte) string {
	if int64(len(text)) <= maxFileReadBytes {
		return windowedFileRead(text, fmt.Sprintf("Extracted text from %s:", resolved), offset, limit)
	}
	convertedPath := storeConvertedText(text, resolved, umo, rawBytes)
	if convertedPath == "" {
		return "Error reading file: parsed document exceeds the read output limit and no workspace is available for storing converted text."
	}
	if offset <= 0 && limit <= 0 {
		return fmt.Sprintf("Converted text was saved to `%s` because the parsed document is too large to return directly. Read or grep that file with a narrow window.", convertedPath)
	}
	selected := sliceTextLines(text, offset, limit)
	if selected == "" {
		return "No content found at the requested line offset."
	}
	if int64(len(selected)) > maxFileReadBytes {
		return fmt.Sprintf("Converted text was saved to `%s`. The requested output is still too large to return directly. Read or grep that file with a narrower window.", convertedPath)
	}
	return fmt.Sprintf("%s\n\nFull converted text is also available at `%s`. Read or grep that file with a narrow window for additional reads.", selected, convertedPath)
}

// storeConvertedText writes the extracted document text into <workspace>/converted_files/<name>_<md5[6]>/text.txt (py _store_converted_text_for_workspace).
func storeConvertedText(text, resolved, umo string, rawBytes []byte) string {
	sum := md5.Sum(rawBytes)
	name := fmt.Sprintf("%s_%x", filepath.Base(resolved), sum[len(sum)-6:])
	dir := filepath.Join(workspaceRoot(umo), "converted_files", name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ""
	}
	target := filepath.Join(dir, "text.txt")
	if err := os.WriteFile(target, []byte(text), 0o600); err != nil {
		return ""
	}
	return target
}

// looksBinarySample rejects NUL-bearing samples from the plain-text read path (py probe kind == binary → "binary files are not supported").
func looksBinarySample(data []byte) bool {
	n := len(data)
	if n > 512 {
		n = 512
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

// Windowed file reading ported from opencode tool/read.ts: DEFAULT_READ_LIMIT=2000 lines, MAX_LINE_LENGTH=2000 chars per line, MAX_BYTES=50KB per response, numbered output, and a trailing hint (capped / showing X-Y of N / end of file) so the model continues with offset instead of blindly re-reading whole files.
const (
	defaultReadLines   = 2000
	maxReadLineChars   = 2000
	maxReadWindowBytes = 50 * 1024
)

// windowedFileRead renders content in one window. offset is 0-based (tool contract), display line numbers 1-based (opencode style). header is the tool-specific title line.
func windowedFileRead(content, header string, offset, limit int) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	total := len(lines)
	if limit <= 0 {
		limit = defaultReadLines
	}
	if offset > 0 && offset >= total && total > 0 {
		return fmt.Sprintf("Error: offset %d is out of range for this file (%d lines).", offset, total)
	}
	if offset < 0 {
		offset = 0
	}
	var out []string
	bytes := 0
	cut := false
	more := false
	for i := offset; i < total; i++ {
		if len(out) >= limit {
			more = true
			break
		}
		line := lines[i]
		if r := []rune(line); len(r) > maxReadLineChars {
			line = string(r[:maxReadLineChars]) + fmt.Sprintf("... (line truncated to %d chars)", maxReadLineChars)
		}
		size := len(line) + 2 + fmtLen(i+offset+1)
		if bytes > 0 {
			size++
		}
		if bytes+size > maxReadWindowBytes {
			cut = true
			break
		}
		out = append(out, fmt.Sprintf("%d: %s", i+offset+1, line))
		bytes += size
	}
	if len(out) == 0 && !cut && !more {
		return header + "\n(End of file - file has no content at this offset.)"
	}
	lastShown := offset + len(out)
	body := strings.Join(out, "\n")
	var suffix string
	switch {
	case cut:
		suffix = fmt.Sprintf("\n\n(Output capped at %d KB. Showing lines %d-%d%s. Use offset=%d to continue.)",
			maxReadWindowBytes/1024, offset+1, lastShown, ofTotal(total), lastShown)
	case more:
		suffix = fmt.Sprintf("\n\n(Showing lines %d-%d of %d. Use offset=%d to continue.)", offset+1, lastShown, total, lastShown)
	default:
		suffix = fmt.Sprintf("\n\n(End of file - total %d lines)", total)
	}
	return header + "\n" + body + suffix
}

func ofTotal(total int) string {
	return fmt.Sprintf(" of %d", total)
}

// fmtLen reports the digit count of n (≥1) for width accounting of "N: " prefixes.
func fmtLen(n int) int {
	if n < 1 {
		n = 1
	}
	l := 0
	for n > 0 {
		n /= 10
		l++
	}
	return l
}

// spoolWriter caps the in-memory head at max bytes but keeps the FULL output on disk (lazily opened spool file under dir), so oversized command results are never silently lost — the caller reports the spool path with read-window instructions (opencode truncate semantics at the source, where the data actually streams past).
type spoolWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	headLen int
	max     int
	dir     string
	f       *os.File
	path    string
	total   int
}

func (w *spoolWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += len(p)
	if w.f != nil {
		_, _ = w.f.Write(p)
		return len(p), nil
	}
	room := w.max - w.headLen
	if len(p) <= room {
		w.buf.Write(p)
		w.headLen += len(p)
		return len(p), nil
	}
	if room > 0 {
		w.buf.Write(p[:room])
		w.headLen += room
	}
	w.buf.Write(p[room:]) // tail beyond the head cap: buffer == full spool content at open time
	if err := w.openSpoolLocked(); err != nil {
		return len(p), nil // spool unavailable: pure head-cap behavior
	}
	return len(p), nil
}

func (w *spoolWriter) openSpoolLocked() error {
	if err := os.MkdirAll(w.dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(w.dir, "exec-*.log")
	if err != nil {
		return err
	}
	if _, err := f.Write(w.buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	w.f = f
	w.path = f.Name()
	return nil
}

func (w *spoolWriter) Close() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	return w.path
}

func (w *spoolWriter) Head() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Bytes()[:w.headLen]
}

// spoolNotice renders the tail hint appended when output spilled to disk.
func spoolNotice(spoolPath string, total int) string {
	return fmt.Sprintf("\n\n...(output capped at %d bytes of %d total; FULL OUTPUT saved to: %s. Use astrbot_grep_tool to search it or astrbot_file_read_tool with offset/limit to read windows — digest each segment before reading the next)...", maxShellOutput, total, spoolPath)
}

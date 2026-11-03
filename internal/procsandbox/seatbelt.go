package procsandbox

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// SeatbeltSandbox is the macOS restricted-process launcher (sandbox-exec).
type SeatbeltSandbox struct{}

const seatbeltProfileTemplate = `
(version 1)
(deny default)
(deny mach-priv-host-port)
(import "system.sb")

(allow process-fork)
(allow process-exec)
(allow process-info* (target self))
(deny process-exec
    (literal "/usr/bin/open")
    (literal "/usr/bin/osascript"))
(deny appleevent-send)
(deny mach-lookup
    (global-name "com.apple.coreservices.launchservicesd")
    (global-name "com.apple.lsd.mapdb")
    (global-name "com.apple.lsd.modifydb")
    (global-name "com.apple.lsd.open")
    (global-name "com.apple.lsd.xpc"))

(allow file-read-metadata file-test-existence)
(allow file-read* file-test-existence
    (subpath "/bin")
    (subpath "/usr/bin")
    (subpath "/usr/libexec")
    (literal (param "EXECUTABLE"))
    (subpath (param "WORKSPACE"))
    (subpath (param "PYTHON_PREFIX"))
    (subpath (param "PYTHON_BASE_PREFIX")))
(allow file-map-executable
    (subpath "/bin")
    (subpath "/usr/bin")
    (subpath "/usr/libexec")
    (literal (param "EXECUTABLE"))
    (subpath (param "WORKSPACE"))
    (subpath (param "PYTHON_PREFIX"))
    (subpath (param "PYTHON_BASE_PREFIX")))
__WORKSPACE_WRITE__

(deny file-read*
    (literal "/private/etc/master.passwd")
    (literal "/private/etc/passwd"))
(deny network*)
`

const seatbeltWorkspaceWrite = `(allow file-write*
    (subpath (param "WORKSPACE")))`

// BuildCommand builds the Seatbelt argv for validated inputs.
func (s *SeatbeltSandbox) BuildCommand(argv []string, spec Spec, env map[string]string) ([]string, error) {
	argv, workspace, env, err := prepareCommand(argv, spec, env)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("Seatbelt is only available on macOS")
	}
	st, err := os.Stat("/usr/bin/sandbox-exec")
	if err != nil || st.IsDir() {
		return nil, fmt.Errorf("Seatbelt (`/usr/bin/sandbox-exec`) is required for restricted Local execution on macOS")
	}

	writeClause := seatbeltWorkspaceWrite
	if !spec.WorkspaceWritable {
		writeClause = "(deny file-write*)"
	}
	profile := strings.Replace(seatbeltProfileTemplate, "__WORKSPACE_WRITE__", writeClause, 1)
	if spec.FilesystemScope == "host" {
		profile = strings.Replace(profile, `(import "system.sb")`,
			"(import \"system.sb\")\n\n(allow file-read* file-write* file-test-existence file-read-metadata file-map-executable)", 1)
		profile = strings.Replace(profile,
			"(deny file-read*\n    (literal \"/private/etc/master.passwd\")\n    (literal \"/private/etc/passwd\"))\n", "", 1)
	}
	if spec.AllowNetwork {
		profile = strings.Replace(profile, "(deny network*)", "(allow network*)", 1)
	}

	var defs []string
	if spec.FilesystemScope == "workspace" {
		writable := map[string]bool{}
		for _, r := range spec.WritableRoots {
			writable[resolvePath(r)] = true
		}
		readable := map[string]bool{}
		for _, r := range spec.ReadableRoots {
			readable[resolvePath(r)] = true
		}
		for r := range writable {
			readable[r] = true
		}
		roots := make([]string, 0, len(readable))
		for r := range readable {
			if st, err := os.Stat(r); err == nil && st.IsDir() {
				roots = append(roots, r)
			}
		}
		sort.Strings(roots)
		for i, root := range roots {
			param := fmt.Sprintf("ALLOWED_ROOT_%d", i)
			defs = append(defs, "-D", param+"="+root)
			ops := "file-read* file-map-executable"
			if writable[root] {
				ops += " file-write*"
			}
			profile += fmt.Sprintf("\n(allow %s (subpath (param %q)))\n", ops, param)
		}
	}

	// Replace the EXECUTABLE literal with the executable rules.
	exe := resolvePath(argv[0])
	profile = strings.ReplaceAll(profile, `(literal (param "EXECUTABLE"))`, fmt.Sprintf("(literal %q)", exe))

	envList := make([]string, 0, len(env)+4)
	keys := sortedKeys(env)
	for _, k := range keys {
		envList = append(envList, k+"="+env[k])
	}
	exeDir := "/usr/bin"
	if exe != "" {
		if d := dirOf(exe); d != "" {
			exeDir = d
		}
	}
	envList = append(envList,
		"PATH="+exeDir+":/usr/bin:/bin",
		"HOME="+workspace,
		"TMPDIR="+workspace,
		"LANG=C.UTF-8",
	)

	cmd := []string{"/usr/bin/sandbox-exec", "-D", "WORKSPACE=" + workspace}
	cmd = append(cmd, defs...)
	cmd = append(cmd,
		"-D", "PYTHON_PREFIX="+exeDir,
		"-D", "PYTHON_BASE_PREFIX="+exeDir,
		"-p", profile,
		"/usr/bin/env", "-i",
	)
	cmd = append(cmd, envList...)
	cmd = append(cmd, buildResourceLimitedArgv(argv, spec.Limits)...)
	return cmd, nil
}

// Run executes a command synchronously through Seatbelt.
func (s *SeatbeltSandbox) Run(argv []string, spec Spec, env map[string]string, timeout time.Duration, outputLimit int, discardStdout bool) (RunResult, error) {
	return runSandbox(s, argv, spec, env, timeout, outputLimit, discardStdout)
}

// SpawnShell starts `sh -c command` through Seatbelt.
func (s *SeatbeltSandbox) SpawnShell(command string, spec Spec, env map[string]string) (*exec.Cmd, error) {
	return spawnShellSandbox(s, command, spec, env)
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return ""
}

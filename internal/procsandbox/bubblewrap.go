package procsandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const bubblewrapTmpBytes = 256 * 1024 * 1024

// BubblewrapSandbox is the Linux restricted-process launcher (bubblewrap).
type BubblewrapSandbox struct{}

var bubblewrapReadonlyPaths = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib64",
	"/etc/alternatives", "/etc/ld.so.cache", "/etc/ld.so.conf",
	"/etc/ld.so.conf.d", "/etc/localtime", "/etc/nsswitch.conf",
	"/etc/passwd", "/etc/group",
}

var bubblewrapNetworkPaths = []string{
	"/etc/resolv.conf", "/etc/hosts", "/etc/host.conf", "/etc/gai.conf",
}

// BuildCommand builds the bubblewrap argv for validated inputs.
func (s *BubblewrapSandbox) BuildCommand(argv []string, spec Spec, env map[string]string) ([]string, error) {
	argv, workspace, env, err := prepareCommand(argv, spec, env)
	if err != nil {
		return nil, err
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap (`bwrap`) is required for restricted Local execution")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		return nil, fmt.Errorf("the Local bubblewrap sandbox requires /bin/sh")
	}

	cmd := []string{bwrap, "--unshare-all", "--new-session", "--die-with-parent", "--clearenv"}
	if spec.AllowNetwork {
		cmd = append(cmd, "--share-net")
	}

	if spec.FilesystemScope == "host" {
		cmd = append(cmd, "--bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "--chdir", workspace)
	} else {
		cmd = append(cmd, "--dir", "/tmp", "--size", fmt.Sprint(bubblewrapTmpBytes), "--tmpfs", "/tmp")

		readonly := map[string]bool{}
		for _, p := range bubblewrapReadonlyPaths {
			readonly[p] = true
		}
		if spec.AllowNetwork {
			for _, p := range bubblewrapNetworkPaths {
				readonly[p] = true
			}
		}
		for _, r := range spec.ReadableRoots {
			readonly[resolvePath(r)] = true
		}
		writable := map[string]bool{}
		for _, r := range spec.WritableRoots {
			writable[resolvePath(r)] = true
		}
		if spec.WorkspaceWritable {
			writable[workspace] = true
		} else {
			readonly[workspace] = true
		}
		for w := range writable {
			delete(readonly, w)
		}
		// Ensure the executable path is readable.
		if exe := resolvePath(argv[0]); exe != "" {
			readonly[exe] = true
		}

		dirs := map[string]bool{"/proc": true, "/dev": true}
		for p := range readonly {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				dirs[p] = true
			}
		}
		for p := range writable {
			dirs[p] = true
		}
		for d := range dirs {
			if d != "/proc" && d != "/dev" {
				cmd = append(cmd, "--dir", d)
			}
		}
		cmd = append(cmd, "--proc", "/proc", "--dev", "/dev")

		for _, p := range sortedByDepth(readonly) {
			if _, err := os.Stat(p); err != nil {
				continue
			}
			cmd = append(cmd, "--ro-bind", p, p)
		}
		for _, p := range sortedByDepth(writable) {
			cmd = append(cmd, "--bind", p, p)
		}
		cmd = append(cmd, "--chdir", workspace)
	}

	for _, k := range sortedKeys(env) {
		cmd = append(cmd, "--setenv", k, env[k])
	}
	home := "/tmp/home"
	if spec.FilesystemScope == "host" {
		home = workspace
	}
	cmd = append(cmd,
		"--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin",
		"--setenv", "HOME", home,
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "LANG", "C.UTF-8",
		"--",
	)
	cmd = append(cmd, buildResourceLimitedArgv(argv, spec.Limits)...)
	return cmd, nil
}

// Run executes a command synchronously through bubblewrap.
func (s *BubblewrapSandbox) Run(argv []string, spec Spec, env map[string]string, timeout time.Duration, outputLimit int, discardStdout bool) (RunResult, error) {
	return runSandbox(s, argv, spec, env, timeout, outputLimit, discardStdout)
}

// SpawnShell starts `sh -c command` through bubblewrap.
func (s *BubblewrapSandbox) SpawnShell(command string, spec Spec, env map[string]string) (*exec.Cmd, error) {
	return spawnShellSandbox(s, command, spec, env)
}

func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			return r
		}
		return abs
	}
	return p
}

func sortedByDepth(paths map[string]bool) []string {
	out := make([]string, 0, len(paths))
	for p := range paths {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		di := strings.Count(out[i], string(os.PathSeparator))
		dj := strings.Count(out[j], string(os.PathSeparator))
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

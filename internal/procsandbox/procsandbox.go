// Package procsandbox provides a restricted-process launcher for Local
// computer use. Ported from astrbot/core/computer/process_sandbox/ (4.29.0):
// bubblewrap on Linux, Seatbelt (sandbox-exec) on macOS, plus Unix resource
// limits. Unlike the Python original (which wraps commands with a Python
// interpreter applying resource.setrlimit), this port applies limits through a
// POSIX `sh` ulimit wrapper.
package procsandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Limits are resource ceilings applied to a sandboxed process tree.
type Limits struct {
	CPUSeconds    int
	FileSizeBytes int64
	MemoryBytes   int64
	OpenFiles     int
	Processes     int
}

// DefaultLimits mirror py SandboxLimits defaults.
func DefaultLimits() Limits {
	return Limits{
		CPUSeconds:    300,
		FileSizeBytes: 100 * 1024 * 1024,
		MemoryBytes:   1024 * 1024 * 1024,
		OpenFiles:     256,
		Processes:     256,
	}
}

func (l Limits) validate() error {
	if l.CPUSeconds <= 0 || l.FileSizeBytes <= 0 || l.MemoryBytes <= 0 || l.OpenFiles <= 0 || l.Processes <= 0 {
		return fmt.Errorf("sandbox limits must be positive integers")
	}
	return nil
}

// Spec describes permissions, workspace and limits for a sandboxed process.
type Spec struct {
	Workspace         string
	WorkspaceWritable bool
	AllowNetwork      bool
	FilesystemScope   string // "workspace" | "host"
	Limits            Limits
	ReadableRoots     []string
	WritableRoots     []string
}

// RunResult is a synchronous sandbox execution result.
type RunResult struct {
	ReturnCode    int
	Stdout        []byte
	Stderr        []byte
	StdoutLimited bool
	StderrLimited bool
}

// TimeoutError indicates a sandbox command exceeded its timeout.
type TimeoutError struct{ Message string }

func (e TimeoutError) Error() string { return e.Message }

// Sandbox launches restricted child processes.
type Sandbox interface {
	// BuildCommand returns the platform sandbox argv for argv+spec.
	BuildCommand(argv []string, spec Spec, env map[string]string) ([]string, error)
	// Run executes a command synchronously.
	Run(argv []string, spec Spec, env map[string]string, timeout time.Duration, outputLimit int, discardStdout bool) (RunResult, error)
	// SpawnShell starts a shell command in a new session (process group).
	SpawnShell(command string, spec Spec, env map[string]string) (*exec.Cmd, error)
}

func prepareCommand(argv []string, spec Spec, env map[string]string) ([]string, string, map[string]string, error) {
	if len(argv) == 0 {
		return nil, "", nil, fmt.Errorf("a sandbox command is required")
	}
	if spec.FilesystemScope != "workspace" && spec.FilesystemScope != "host" {
		return nil, "", nil, fmt.Errorf("invalid Local filesystem scope: %s", spec.FilesystemScope)
	}
	if spec.Limits == (Limits{}) {
		spec.Limits = DefaultLimits()
	}
	if err := spec.Limits.validate(); err != nil {
		return nil, "", nil, err
	}
	workspace, err := filepath.Abs(spec.Workspace)
	if err != nil {
		return nil, "", nil, err
	}
	if st, err := os.Stat(workspace); err != nil || !st.IsDir() {
		return nil, "", nil, fmt.Errorf("sandbox workspace does not exist: %s", workspace)
	}
	if spec.FilesystemScope == "workspace" {
		for _, root := range spec.WritableRoots {
			_ = os.MkdirAll(root, 0o755)
		}
	}
	normalized := map[string]string{}
	for k, v := range env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.Contains(v, "\x00") {
			return nil, "", nil, fmt.Errorf("invalid sandbox environment variable name: %q", k)
		}
		normalized[k] = v
	}
	return argv, workspace, normalized, nil
}

// CreateProcessSandbox selects the restricted-process launcher for this system.
func CreateProcessSandbox() (Sandbox, error) {
	switch runtime.GOOS {
	case "linux":
		return &BubblewrapSandbox{}, nil
	case "darwin":
		return &SeatbeltSandbox{}, nil
	default:
		return nil, fmt.Errorf("no Local process sandbox backend is available")
	}
}

// Status reports Local isolation availability (probed once per process).
type Status struct {
	Backend string `json:"backend"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

var cachedStatus *Status

// GetProcessSandboxStatus probes Local isolation once per process.
func GetProcessSandboxStatus() Status {
	if cachedStatus != nil {
		return *cachedStatus
	}
	status := Status{Status: "unsupported"}
	switch runtime.GOOS {
	case "linux":
		status.Backend = "bubblewrap"
		if _, err := exec.LookPath("bwrap"); err == nil {
			status.Status = "detected"
		} else {
			status.Status = "missing"
		}
	case "darwin":
		status.Backend = "seatbelt"
		if st, err := os.Stat("/usr/bin/sandbox-exec"); err == nil && !st.IsDir() {
			status.Status = "detected"
		} else {
			status.Status = "missing"
		}
	}
	if status.Status == "detected" {
		if sb, err := CreateProcessSandbox(); err != nil {
			status.Status = "unavailable"
			status.Error = err.Error()
		} else {
			tmp, err := os.MkdirTemp("", "sandbox-probe-")
			if err != nil {
				status.Status = "unavailable"
				status.Error = err.Error()
			} else {
				defer os.RemoveAll(tmp)
				res, err := sb.Run([]string{"/bin/sh", "-c", ":"}, Spec{Workspace: tmp, Limits: DefaultLimits()}, nil, 5*time.Second, 1024, false)
				if err != nil {
					status.Status = "unavailable"
					status.Error = truncate(err.Error(), 1024)
				} else if res.ReturnCode != 0 {
					status.Status = "unavailable"
					msg := strings.TrimSpace(string(res.Stderr))
					if msg == "" {
						msg = fmt.Sprintf("sandbox probe exited with code %d", res.ReturnCode)
					}
					status.Error = truncate(msg, 1024)
				}
			}
		}
	}
	cachedStatus = &status
	return status
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

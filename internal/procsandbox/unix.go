package procsandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// buildResourceLimitedArgv wraps argv with a POSIX `sh` ulimit bootstrap so the
// sandboxed command inherits the requested resource ceilings (aligned with py
// unix.build_resource_limited_argv, which uses resource.setrlimit).
func buildResourceLimitedArgv(argv []string, l Limits) []string {
	script := fmt.Sprintf(
		"ulimit -c 0 2>/dev/null; ulimit -t %d 2>/dev/null; ulimit -f %d 2>/dev/null; "+
			"ulimit -n %d 2>/dev/null; ulimit -u %d 2>/dev/null; ulimit -v %d 2>/dev/null; exec \"$@\"",
		l.CPUSeconds, l.FileSizeBytes/512, l.OpenFiles, l.Processes, l.MemoryBytes/1024,
	)
	return append([]string{"/bin/sh", "-c", script, "sh"}, argv...)
}

// runSandbox executes bs.BuildCommand(argv, spec, env) synchronously.
func runSandbox(bs Sandbox, argv []string, spec Spec, env map[string]string, timeout time.Duration, outputLimit int, discardStdout bool) (RunResult, error) {
	if outputLimit < 0 {
		return RunResult{}, fmt.Errorf("sandbox output limit must be greater than 0")
	}
	cmdArgv, err := bs.BuildCommand(argv, spec, env)
	if err != nil {
		return RunResult{}, err
	}
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, cmdArgv[0], cmdArgv[1:]...)
	cmd.Dir = spec.Workspace
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	var stdout, stderr bytes.Buffer
	if !discardStdout {
		cmd.Stdout = &stdout
	}
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return RunResult{}, TimeoutError{Message: fmt.Sprintf("Sandbox command timed out after %s.", timeout)}
	}
	res := RunResult{}
	if cmd.ProcessState != nil {
		res.ReturnCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			res.ReturnCode = ee.ExitCode()
		} else {
			return RunResult{}, runErr
		}
	}

	out := stdout.Bytes()
	errOut := stderr.Bytes()
	if outputLimit > 0 {
		if len(out) > outputLimit {
			out = out[:outputLimit]
			res.StdoutLimited = true
		}
		if len(errOut) > outputLimit {
			errOut = errOut[:outputLimit]
			res.StderrLimited = true
		}
	}
	if !discardStdout {
		res.Stdout = out
	}
	res.Stderr = errOut
	return res, nil
}

// spawnShellSandbox starts `sh -c command` inside the sandbox (stdin/stdout are
// set by the caller; stderr is merged into stdout).
func spawnShellSandbox(bs Sandbox, command string, spec Spec, env map[string]string) (*exec.Cmd, error) {
	cmdArgv, err := bs.BuildCommand([]string{"/bin/sh", "-c", command}, spec, env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(cmdArgv[0], cmdArgv[1:]...)
	cmd.Dir = spec.Workspace
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	return cmd, nil
}

// Package sandbox provides isolated execution boundaries for EVOLV runs.
// It decouples the agent's tool execution from bare host commands, allowing
// seamless switching between local execution and sandboxed microVM/container runners
// in multi-tenant cloud deployments.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Result is the outcome of a sandboxed execution command.
type Result struct {
	ExitCode int           `json:"exit_code"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	Duration time.Duration `json:"duration"`
	TimedOut bool          `json:"timed_out"`
}

// Limits defines compute, memory, and timeout ceilings for sandboxed execution.
type Limits struct {
	Timeout   time.Duration `json:"timeout"`
	MaxMemory int64         `json:"max_memory_bytes"` // e.g. 512MB
	MaxOutput int           `json:"max_output_bytes"` // e.g. 1MB
}

// DefaultLimits provides sensible defaults for agent command execution.
func DefaultLimits() Limits {
	return Limits{
		Timeout:   30 * time.Second,
		MaxMemory: 512 * 1024 * 1024,
		MaxOutput: 1024 * 1024,
	}
}

// Runner defines the execution engine (Local vs Cloud/Container).
type Runner interface {
	Name() string
	Run(ctx context.Context, cmd string, dir string, env []string, limits Limits) (*Result, error)
}

// LocalRunner executes commands directly on the host operating system.
type LocalRunner struct{}

// NewLocalRunner creates a local runner.
func NewLocalRunner() *LocalRunner {
	return &LocalRunner{}
}

// Name returns the runner identifier.
func (r *LocalRunner) Name() string { return "local" }

// Run executes a shell command on the host machine subject to timeouts and buffer limits.
func (r *LocalRunner) Run(ctx context.Context, command string, dir string, env []string, limits Limits) (*Result, error) {
	if command == "" {
		return nil, errors.New("empty command")
	}
	if limits.Timeout <= 0 {
		limits.Timeout = 30 * time.Second
	}
	if limits.MaxOutput <= 0 {
		limits.MaxOutput = 1024 * 1024
	}

	execCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	start := time.Now()
	var cmd *exec.Cmd

	// Platform shell resolution
	if os.PathSeparator == '\\' {
		cmd = exec.CommandContext(execCtx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(execCtx, "sh", "-c", command)
	}

	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	duration := time.Since(start)

	timedOut := errors.Is(execCtx.Err(), context.DeadlineExceeded)

	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()

	if len(stdoutStr) > limits.MaxOutput {
		stdoutStr = stdoutStr[:limits.MaxOutput] + "\n[output truncated: limit reached]"
	}
	if len(stderrStr) > limits.MaxOutput {
		stderrStr = stderrStr[:limits.MaxOutput] + "\n[output truncated: limit reached]"
	}

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if timedOut {
			exitCode = 124
		} else {
			exitCode = 1
		}
	}

	res := &Result{
		ExitCode: exitCode,
		Stdout:   stdoutStr,
		Stderr:   stderrStr,
		Duration: duration,
		TimedOut: timedOut,
	}

	return res, nil
}

// ContainerRunner simulates or coordinates execution inside an isolated container / microVM.
type ContainerRunner struct {
	Image   string `json:"image"`
	Network bool   `json:"network"`
}

// NewContainerRunner creates a containerized runner configuration.
func NewContainerRunner(image string, network bool) *ContainerRunner {
	if image == "" {
		image = "alpine:latest"
	}
	return &ContainerRunner{Image: image, Network: network}
}

// Name returns the runner identifier.
func (c *ContainerRunner) Name() string { return fmt.Sprintf("container(%s)", c.Image) }

// ErrContainerUnavailable indicates that no container engine is running.
var ErrContainerUnavailable = errors.New("container execution backend unavailable")

// Run executes the command inside a container sandbox.
func (c *ContainerRunner) Run(ctx context.Context, command string, dir string, env []string, limits Limits) (*Result, error) {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return nil, ErrContainerUnavailable
	}

	if limits.Timeout <= 0 {
		limits.Timeout = 30 * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	args := []string{"run", "--rm"}
	if !c.Network {
		args = append(args, "--network=none")
	}
	if dir != "" {
		args = append(args, "-v", fmt.Sprintf("%s:%s", dir, "/workspace"), "-w", "/workspace")
	}
	args = append(args, c.Image, "sh", "-c", command)

	cmd := exec.CommandContext(execCtx, dockerPath, args...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	start := time.Now()
	err = cmd.Run()
	duration := time.Since(start)
	timedOut := errors.Is(execCtx.Err(), context.DeadlineExceeded)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if timedOut {
			exitCode = 124
		} else {
			exitCode = 1
		}
	}

	return &Result{
		ExitCode: exitCode,
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
		Duration: duration,
		TimedOut: timedOut,
	}, nil
}

package sandbox

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalRunnerBasic(t *testing.T) {
	r := NewLocalRunner()
	if r.Name() != "local" {
		t.Fatalf("expected runner name 'local', got %s", r.Name())
	}

	limits := DefaultLimits()
	ctx := context.Background()

	// 1. Simple echo command
	res, err := r.Run(ctx, "echo evolv_sandbox_ok", "", nil, limits)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr: %s)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "evolv_sandbox_ok") {
		t.Fatalf("expected output to contain 'evolv_sandbox_ok', got: %s", res.Stdout)
	}

	// 2. Empty command error
	if _, err := r.Run(ctx, "", "", nil, limits); err == nil {
		t.Fatal("expected error on empty command")
	}

	// 3. Timeout enforcement
	shortLimits := Limits{Timeout: 50 * time.Millisecond}
	var sleepCmd string
	if strings.Contains(strings.ToLower(r.Name()), "win") || runtime.GOOS == "windows" {
		sleepCmd = "ping -n 2 127.0.0.1 >nul"
	} else {
		sleepCmd = "sleep 1"
	}
	resTimeout, _ := r.Run(ctx, sleepCmd, "", nil, shortLimits)
	if resTimeout != nil && !resTimeout.TimedOut {
		t.Fatalf("expected command to time out, duration: %v", resTimeout.Duration)
	}
}

func TestContainerRunnerRequiresEngine(t *testing.T) {
	cr := NewContainerRunner("alpine:latest", false)
	_, err := cr.Run(context.Background(), "echo test", "", nil, DefaultLimits())
	if err != nil && !errors.Is(err, ErrContainerUnavailable) {
		t.Fatalf("unexpected error: %v", err)
	}
}

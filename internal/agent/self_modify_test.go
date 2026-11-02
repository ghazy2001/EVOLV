package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// setupMockSourceRepo creates a minimal valid Go module mimicking github.com/dxn111/EVOLV.
func setupMockSourceRepo(t *testing.T, mainBody string, testBody string) string {
	t.Helper()
	srcDir := t.TempDir()

	// Write go.mod
	goMod := "module github.com/dxn111/EVOLV\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create cmd/evolv
	cmdDir := filepath.Join(srcDir, "cmd", "evolv")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if mainBody == "" {
		mainBody = `package main

import (
	"flag"
	"fmt"
)

func main() {
	ver := flag.Bool("version", false, "")
	flag.Parse()
	if *ver {
		fmt.Println("evolv 0.3.0")
		return
	}
	fmt.Println("evolv 0.3.0")
}
`
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte(mainBody), 0o644); err != nil {
		t.Fatal(err)
	}

	if testBody != "" {
		if err := os.WriteFile(filepath.Join(cmdDir, "main_test.go"), []byte(testBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return srcDir
}

// TestSelfModify_NoSourceRoot verifies rejection when no Go source root is found.
func TestSelfModify_NoSourceRoot(t *testing.T) {
	a := testAgent(t)
	res := a.SelfModify(selfModifyOptions{
		SourceRoot: filepath.Join(t.TempDir(), "nonexistent"),
	})

	if res.OK {
		t.Fatal("expected failure when source root does not exist, got ok=true")
	}
	if res.Stage != "detect" {
		t.Fatalf("expected stage 'detect', got %q", res.Stage)
	}
}

// TestSelfModify_InvalidGoMod verifies rejection if source root is not github.com/dxn111/EVOLV.
func TestSelfModify_InvalidGoMod(t *testing.T) {
	a := testAgent(t)
	fakeDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(fakeDir, "go.mod"), []byte("module github.com/other/repo\n\ngo 1.22\n"), 0o644)

	res := a.SelfModify(selfModifyOptions{
		SourceRoot: fakeDir,
	})

	if res.OK {
		t.Fatal("expected failure for foreign go.mod, got ok=true")
	}
	if res.Stage != "detect" {
		t.Fatalf("expected stage 'detect', got %q", res.Stage)
	}
}

// TestSelfModify_PathEscape verifies that file mutations cannot escape source root.
func TestSelfModify_PathEscape(t *testing.T) {
	a := testAgent(t)
	srcDir := setupMockSourceRepo(t, "", "")

	res := a.SelfModify(selfModifyOptions{
		SourceRoot: srcDir,
		Files: []fileMutation{
			{Path: "../../escape.txt", Content: "hacked"},
		},
	})

	if res.OK {
		t.Fatal("expected failure on path traversal escape, got ok=true")
	}
	if res.Stage != "mutate" {
		t.Fatalf("expected stage 'mutate', got %q", res.Stage)
	}
}

// TestSelfModify_CompileFailure verifies that compilation errors are safely caught
// and broadcast without corrupting anything.
func TestSelfModify_CompileFailure(t *testing.T) {
	a := testAgent(t)
	sub, _ := a.bus.Subscribe()
	defer a.bus.Unsubscribe(sub)

	// Syntax error in main.go
	srcDir := setupMockSourceRepo(t, "package main\n\nfunc main() { syntax error here }", "")

	res := a.SelfModify(selfModifyOptions{
		SourceRoot: srcDir,
		RunTests:   false,
	})

	if res.OK {
		t.Fatal("expected compilation failure, got ok=true")
	}
	if res.Stage != "compile" {
		t.Fatalf("expected stage 'compile', got %q", res.Stage)
	}
	if !strings.Contains(res.Error, "go build failed") {
		t.Fatalf("expected 'go build failed' in error, got %q", res.Error)
	}

	// Verify failed bus event was emitted
	select {
	case ev := <-sub.Events():
		if ev.Type == "kernel:modify:failed" {
			var payload map[string]string
			if err := json.Unmarshal(ev.Payload, &payload); err == nil && payload["stage"] == "compile" {
				return // Success
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for kernel:modify:failed event")
	}
}

// TestSelfModify_TestFailure verifies that a failing test prevents candidate swapping.
func TestSelfModify_TestFailure(t *testing.T) {
	a := testAgent(t)
	sub, _ := a.bus.Subscribe()
	defer a.bus.Unsubscribe(sub)

	failingTest := `package main

import "testing"

func TestIntentionalFailure(t *testing.T) {
	t.Fatal("intentional test break")
}
`
	srcDir := setupMockSourceRepo(t, "", failingTest)

	res := a.SelfModify(selfModifyOptions{
		SourceRoot: srcDir,
		RunTests:   true,
		TestPkg:    "./cmd/evolv",
	})

	if res.OK {
		t.Fatal("expected test failure, got ok=true")
	}
	if res.Stage != "test" {
		t.Fatalf("expected stage 'test', got %q", res.Stage)
	}
	if !res.Compiled {
		t.Fatal("expected compiled=true prior to test phase")
	}
}

// TestSelfModify_SuccessfulStageOnly verifies that candidate compilation, testing,
// and smoke testing succeed when stage_only=true.
func TestSelfModify_SuccessfulStageOnly(t *testing.T) {
	a := testAgent(t)
	sub, _ := a.bus.Subscribe()
	defer a.bus.Unsubscribe(sub)

	passTest := `package main

import "testing"

func TestPass(t *testing.T) {}
`
	srcDir := setupMockSourceRepo(t, "", passTest)

	res := a.SelfModify(selfModifyOptions{
		SourceRoot: srcDir,
		RunTests:   true,
		TestPkg:    "./cmd/evolv",
		StageOnly:  true,
		Message:    "stage candidate v0.3.1",
	})

	if !res.OK {
		t.Fatalf("expected success, got error: %s (stage: %s)", res.Error, res.Stage)
	}
	if !res.Compiled {
		t.Fatal("expected compiled=true")
	}
	if !res.Tested {
		t.Fatal("expected tested=true")
	}
	if res.Swapped {
		t.Fatal("expected swapped=false when stage_only=true")
	}
	if _, err := os.Stat(res.Candidate); err != nil {
		t.Fatalf("candidate binary does not exist at %s: %v", res.Candidate, err)
	}

	// Verify bus emitted kernel:evolved, skipping unrelated events until it arrives
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type != "kernel:evolved" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatalf("malformed kernel:evolved payload: %v", err)
			}
			if payload["tag"] != res.Tag {
				t.Fatalf("unexpected tag in kernel:evolved: got %v, want %v", payload["tag"], res.Tag)
			}
			if payload["swapped"] != false {
				t.Fatalf("unexpected swapped in kernel:evolved: got %v, want false", payload["swapped"])
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for kernel:evolved event")
		}
	}
}

// TestSelfModify_RollbackMutationsOnFailure verifies that inline mutations are reverted
// when the candidate fails verification, restoring edited files and removing created ones.
func TestSelfModify_RollbackMutationsOnFailure(t *testing.T) {
	a := testAgent(t)
	srcDir := setupMockSourceRepo(t, "", "")
	mainPath := filepath.Join(srcDir, "cmd", "evolv", "main.go")
	original, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join("cmd", "evolv", "extra.go")

	res := a.SelfModify(selfModifyOptions{
		SourceRoot: srcDir,
		Files: []fileMutation{
			{Path: filepath.Join("cmd", "evolv", "main.go"), Content: "package main\n\nfunc main() { syntax error here }"},
			{Path: newFile, Content: "package main\n"},
		},
	})

	if res.OK || res.Stage != "compile" {
		t.Fatalf("expected compile failure, got ok=%v stage=%q", res.OK, res.Stage)
	}
	restored, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("main.go was not restored after failure, got: %s", restored)
	}
	if _, err := os.Stat(filepath.Join(srcDir, newFile)); !os.IsNotExist(err) {
		t.Fatalf("created file should have been removed after failure, stat err: %v", err)
	}
}

// TestSelfModify_KeepMutationsOnStageOnlySuccess verifies mutations are retained on success.
func TestSelfModify_KeepMutationsOnStageOnlySuccess(t *testing.T) {
	a := testAgent(t)
	srcDir := setupMockSourceRepo(t, "", "")
	newFile := filepath.Join("cmd", "evolv", "extra.go")

	res := a.SelfModify(selfModifyOptions{
		SourceRoot: srcDir,
		StageOnly:  true,
		Files:      []fileMutation{{Path: newFile, Content: "package main\n"}},
	})

	if !res.OK {
		t.Fatalf("expected success, got error: %s (stage: %s)", res.Error, res.Stage)
	}
	if _, err := os.Stat(filepath.Join(srcDir, newFile)); err != nil {
		t.Fatalf("mutation should be retained on success: %v", err)
	}
}

// TestSwapExecutable_FirstInstall verifies a missing launcher is a valid first install
// and that no staging leftovers remain.
func TestSwapExecutable_FirstInstall(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "candidate")
	if err := os.WriteFile(src, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "launcher")

	if err := swapExecutable(src, dst); err != nil {
		t.Fatalf("first install failed: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "new binary" {
		t.Fatalf("unexpected launcher content %q (err: %v)", got, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".new-") || strings.HasSuffix(e.Name(), ".old") {
			t.Fatalf("unexpected leftover after swap: %s", e.Name())
		}
	}
}

// TestSwapExecutable_MissingSourceKeepsLauncher verifies the launcher is untouched
// when the candidate cannot be staged.
func TestSwapExecutable_MissingSourceKeepsLauncher(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "launcher")
	if err := os.WriteFile(dst, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := swapExecutable(filepath.Join(dir, "missing"), dst); err == nil {
		t.Fatal("expected error when candidate source is missing")
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "old binary" {
		t.Fatalf("launcher should be untouched, got %q (err: %v)", got, err)
	}
}

// TestSelfModify_AtomicSwapOnGreen verifies candidate compilation and atomic swap
// into a target launcher path with automatic rollback preservation.
func TestSelfModify_AtomicSwapOnGreen(t *testing.T) {
	a := testAgent(t)

	srcDir := setupMockSourceRepo(t, "", "")

	// Create an existing mock launcher binary
	launcherDir := t.TempDir()
	binName := "evolv"
	if runtime.GOOS == "windows" {
		binName = "evolv.exe"
	}
	launcherPath := filepath.Join(launcherDir, binName)
	if err := os.WriteFile(launcherPath, []byte("old binary content"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := a.SelfModify(selfModifyOptions{
		SourceRoot:     srcDir,
		RunTests:       false,
		StageOnly:      false,
		TargetLauncher: launcherPath,
		Message:        "upgrade to new version",
	})

	if !res.OK {
		t.Fatalf("expected success, got error: %s (stage: %s)", res.Error, res.Stage)
	}
	if !res.Swapped {
		t.Fatal("expected swapped=true")
	}

	// Verify target launcher has been replaced with the executable
	fi, err := os.Stat(launcherPath)
	if err != nil {
		t.Fatalf("launcher missing after swap: %v", err)
	}
	if fi.Size() < 1000 {
		t.Fatalf("launcher size too small (%d bytes), swap failed", fi.Size())
	}

	// Execute swapped launcher to ensure it runs cleanly
	cmd := exec.Command(launcherPath, "-version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("swapped launcher failed to execute: %v (%s)", err, string(out))
	}
	if !strings.Contains(string(out), "evolv 0.3.0") {
		t.Fatalf("expected 'evolv 0.3.0' from swapped launcher, got: %s", string(out))
	}
}

// TestSelfModify_ExecuteActionIntegration verifies executing self_modify through agent.Execute.
func TestSelfModify_ExecuteActionIntegration(t *testing.T) {
	a := testAgent(t)
	srcDir := setupMockSourceRepo(t, "", "")

	act := action{
		Action:     "self_modify",
		SourceRoot: srcDir,
		StageOnly:  true,
		Message:    "integration test candidate",
	}

	raw := a.Execute(act)
	raw = strings.TrimPrefix(raw, "kernel:")

	var res map[string]any
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("malformed execute json: %v", err)
	}

	if res["ok"] != true {
		t.Fatalf("expected ok=true, got %v (error: %v)", res["ok"], res["error"])
	}
	if res["compiled"] != true {
		t.Fatalf("expected compiled=true, got %v", res["compiled"])
	}
	if res["swapped"] != false {
		t.Fatalf("expected swapped=false with stage_only, got %v", res["swapped"])
	}
}

func TestSelfModify_RollbackOnFailure(t *testing.T) {
	a := testAgent(t)
	srcDir := setupMockSourceRepo(t, "", "")

	// 1. Existing file mutated to broken syntax should be rolled back to original
	targetFile := filepath.Join(srcDir, "cmd", "evolv", "main.go")
	origContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatal(err)
	}

	res := a.SelfModify(selfModifyOptions{
		SourceRoot: srcDir,
		Files: []fileMutation{
			{Path: "cmd/evolv/main.go", Content: "package main\n\nsyntax error!"},
		},
	})
	if res.OK {
		t.Fatal("expected failure due to syntax error")
	}

	restoredContent, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(restoredContent) != string(origContent) {
		t.Fatalf("rollback failed: expected %q, got %q", string(origContent), string(restoredContent))
	}

	// 2. Newly created file should be removed on failure
	newFile := filepath.Join(srcDir, "cmd", "evolv", "extra.go")
	res = a.SelfModify(selfModifyOptions{
		SourceRoot: srcDir,
		Files: []fileMutation{
			{Path: "cmd/evolv/extra.go", Content: "package main\n\nsyntax error 2!"},
		},
	})
	if res.OK {
		t.Fatal("expected failure due to syntax error")
	}
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatalf("newly created file was not cleaned up on failure: %v", err)
	}
}

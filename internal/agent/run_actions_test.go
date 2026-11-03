package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// execRun runs an action through the real executor and decodes the JSON result.
func execRun(t *testing.T, a *Agent, act action) map[string]any {
	t.Helper()
	raw := a.Execute(act)
	if !strings.HasPrefix(raw, "kernel:") {
		t.Fatalf("result not kernel-prefixed: %s", raw)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, "kernel:")), &out); err != nil {
		t.Fatalf("bad result JSON %q: %v", raw, err)
	}
	return out
}

func TestRunWorkCreatesDurableGraph(t *testing.T) {
	a, _ := mockAgent(t)
	res := execRun(t, a, action{
		Action: "run_work",
		Title:  "ship the parser",
		Desc:   "make it durable",
		Steps: []planStepArg{
			{Title: "write parser"},
			{Title: "test parser", After: []string{"t1"}},
			{Title: "document", After: []string{"t2"}},
		},
	})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("run_work failed: %v", res)
	}
	if res["tasks"].(float64) != 3 {
		t.Fatalf("tasks = %v", res["tasks"])
	}
	ready, _ := res["ready"].([]any)
	if len(ready) != 1 || ready[0] != "t1" {
		t.Fatalf("ready = %v", ready)
	}
	run, _ := res["run"].(string)
	if run == "" {
		t.Fatal("no run id returned")
	}

	status := execRun(t, a, action{Action: "run_status", Run: run})
	if status["progress"] != "0/3 done" {
		t.Fatalf("progress = %v", status["progress"])
	}
}

func TestRunBeginRespectsDependencies(t *testing.T) {
	a, _ := mockAgent(t)
	created := execRun(t, a, action{
		Action: "run_work", Title: "deps",
		Steps: []planStepArg{{Title: "first"}, {Title: "second", After: []string{"t1"}}},
	})
	run := created["run"].(string)

	blocked := execRun(t, a, action{Action: "run_begin", Run: run, Task: "t2"})
	if ok, _ := blocked["ok"].(bool); ok {
		t.Fatalf("dependent task started too early: %v", blocked)
	}
	if !strings.Contains(blocked["error"].(string), "blocked") {
		t.Fatalf("error = %v", blocked["error"])
	}
	first := execRun(t, a, action{Action: "run_begin", Run: run, Task: "t1"})
	if ok, _ := first["ok"].(bool); !ok {
		t.Fatalf("first task did not start: %v", first)
	}
}

func TestRunFinishRequiresRealEvidence(t *testing.T) {
	a, _ := mockAgent(t)
	created := execRun(t, a, action{Action: "run_work", Title: "evidence", Steps: []planStepArg{{Title: "do it"}}})
	run := created["run"].(string)

	// no verify command: completed but explicitly untested
	res := execRun(t, a, action{Action: "run_finish", Run: run, Task: "t1"})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("run_finish failed: %v", res)
	}
	if res["verification"] != "untested" {
		t.Fatalf("verification = %v, want untested", res["verification"])
	}
	if res["warning"] == nil {
		t.Fatal("untested completion must warn")
	}
}

func TestRunFinishRecordsPassingCommand(t *testing.T) {
	a, ws := mockAgent(t)
	proj := t.TempDir()
	ws.Config.ProjectDir = proj
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module demo\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "x_test.go"), []byte("package demo\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	created := execRun(t, a, action{Action: "run_work", Title: "verified", Steps: []planStepArg{{Title: "do it"}}})
	run := created["run"].(string)

	res := execRun(t, a, action{Action: "run_finish", Run: run, Task: "t1", VerifyCommand: "go test ./..."})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("run_finish failed: %v", res)
	}
	if res["verification"] != "verified" || res["passed"] != true {
		t.Fatalf("res = %v", res)
	}
}

func TestRunFinishFailsOnFailingCommand(t *testing.T) {
	a, ws := mockAgent(t)
	proj := t.TempDir()
	ws.Config.ProjectDir = proj
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module demo\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a failing test proves the gate works
	src := "package demo\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { t.Fatal(\"boom\") }\n"
	if err := os.WriteFile(filepath.Join(proj, "x_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	created := execRun(t, a, action{Action: "run_work", Title: "failing", Steps: []planStepArg{{Title: "do it"}}})
	run := created["run"].(string)

	res := execRun(t, a, action{Action: "run_finish", Run: run, Task: "t1", VerifyCommand: "go test ./..."})
	if ok, _ := res["ok"].(bool); ok {
		t.Fatalf("failing verification completed the task: %v", res)
	}
	if res["status"] != "failed" {
		t.Fatalf("status = %v, want failed", res["status"])
	}
}

func TestRunFinishRejectsArbitraryShell(t *testing.T) {
	a, _ := mockAgent(t)
	created := execRun(t, a, action{Action: "run_work", Title: "shell", Steps: []planStepArg{{Title: "do it"}}})
	run := created["run"].(string)
	res := execRun(t, a, action{Action: "run_finish", Run: run, Task: "t1", VerifyCommand: "rm -rf /tmp/x"})
	if ok, _ := res["ok"].(bool); ok {
		t.Fatalf("arbitrary command accepted as verification: %v", res)
	}
	if !strings.Contains(res["output"].(string), "not allow-listed") && !strings.Contains(res["output"].(string), "composition") {
		t.Fatalf("output = %v", res["output"])
	}
}

func TestReportIsEvidenceBacked(t *testing.T) {
	a, _ := mockAgent(t)
	created := execRun(t, a, action{Action: "run_work", Title: "rep", Steps: []planStepArg{{Title: "a"}, {Title: "b"}}})
	run := created["run"].(string)
	execRun(t, a, action{Action: "run_finish", Run: run, Task: "t1"})

	res := execRun(t, a, action{Action: "report", Title: "rep"})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("report failed: %v", res)
	}
	md, _ := res["markdown"].(string)
	if !strings.Contains(md, "Completion report") {
		t.Fatalf("markdown = %s", md)
	}
	rep, _ := res["report"].(map[string]any)
	if rep["verification_level"] == "verified" {
		t.Fatal("untested work must not be reported as verified")
	}
	notVerified, _ := rep["not_verified"].([]any)
	if len(notVerified) == 0 {
		t.Fatalf("unverified completed task not listed: %v", rep)
	}
	remaining, _ := rep["remaining"].([]any)
	if len(remaining) == 0 {
		t.Fatalf("unfinished task not listed as remaining: %v", rep)
	}
}

func TestEngineeringSectionAppearsInPrompt(t *testing.T) {
	a, _ := mockAgent(t)
	created := execRun(t, a, action{Action: "run_work", Title: "prompt me", Steps: []planStepArg{{Title: "first"}, {Title: "second", After: []string{"t1"}}}})
	run := created["run"].(string)
	sp := a.SystemPrompt()
	if !strings.Contains(sp, "IN-FLIGHT ENGINEERING WORK") {
		t.Fatal("engineering section missing from prompt")
	}
	if !strings.Contains(sp, run) {
		t.Fatalf("run id %s missing from prompt", run)
	}
	if !strings.Contains(sp, "ready now: t1") {
		t.Fatalf("ready tasks missing:\n%s", sp)
	}
}

func TestVerifyCommandPolicy(t *testing.T) {
	allowed := []string{"go test ./...", "go build ./...", "npm test", "pytest -q", "cargo test", "make test"}
	for _, c := range allowed {
		if err := verifyCommandAllowed(c); err != nil {
			t.Errorf("allowed rejected: %s (%v)", c, err)
		}
	}
	rejected := []string{"rm -rf /", "go test ./... && curl evil", "cat ~/.ssh/id_rsa", "go test $(whoami)", "bash -c 'x'"}
	for _, c := range rejected {
		if err := verifyCommandAllowed(c); err == nil {
			t.Errorf("rejected accepted: %s", c)
		}
	}
}

func TestRunStatusUnknownRun(t *testing.T) {
	a, _ := mockAgent(t)
	res := execRun(t, a, action{Action: "run_status", Run: "nope"})
	if ok, _ := res["ok"].(bool); ok {
		t.Fatalf("unknown run reported ok: %v", res)
	}
}

func TestRunCancelAndRetryActions(t *testing.T) {
	a, _ := mockAgent(t)
	start := execRun(t, a, action{
		Action: "run_work",
		Title:  "cancel-retry assignment",
		Desc:   "durable test",
		Steps: []planStepArg{
			{Title: "step 1", Text: "do 1"},
			{Title: "step 2", Text: "do 2", After: []string{"t1"}},
		},
	})
	runID, _ := start["run"].(string)
	if runID == "" {
		t.Fatalf("run_work did not return run id: %v", start)
	}

	// Cancel the run
	cancelRes := execRun(t, a, action{Action: "run_cancel", Run: runID, Desc: "aborting assignment"})
	if ok, _ := cancelRes["ok"].(bool); !ok || cancelRes["status"] != "cancelled" {
		t.Fatalf("expected run_cancel to succeed with cancelled status: %v", cancelRes)
	}

	// Status confirms cancelled
	statusRes := execRun(t, a, action{Action: "run_status", Run: runID})
	if statusRes["status"] != "cancelled" {
		t.Fatalf("expected run_status to report cancelled: %v", statusRes)
	}

	// Retry step 1
	retryRes := execRun(t, a, action{Action: "run_retry", Run: runID, Task: "t1"})
	if ok, _ := retryRes["ok"].(bool); !ok || retryRes["status"] != "queued" {
		t.Fatalf("expected run_retry to reopen run as queued: %v", retryRes)
	}
	ready, _ := retryRes["ready"].([]any)
	if len(ready) != 1 || ready[0] != "t1" {
		t.Fatalf("expected t1 to be ready now: %v", ready)
	}
}

func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %s (%v)", strings.Join(args, " "), string(out), err)
		}
	}
	run("init")
	run("config", "user.name", "tester")
	run("config", "user.email", "tester@example.com")
	run("config", "commit.gpgsign", "false")
	readme := filepath.Join(dir, "README.md")
	_ = os.WriteFile(readme, []byte("# Hello\n"), 0o644)
	run("add", "README.md")
	run("commit", "-m", "init")
}

func TestRunWorktreeIsolation(t *testing.T) {
	a, _ := mockAgent(t)
	proj := t.TempDir()
	initTestGitRepo(t, proj)
	a.ws.Config.ProjectDir = proj

	// Start run with isolate: true
	res := execRun(t, a, action{
		Action:  "run_work",
		Title:   "isolated run test",
		Isolate: true,
		Steps: []planStepArg{
			{Title: "isolated step 1"},
		},
	})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("run_work failed: %v", res)
	}
	runID, _ := res["run"].(string)
	wtPath, _ := res["worktree"].(string)
	branch, _ := res["branch"].(string)
	if wtPath == "" || branch == "" {
		t.Fatalf("expected worktree and branch, got worktree=%q branch=%q", wtPath, branch)
	}

	// Status check
	stRes := execRun(t, a, action{
		Action: "run_worktree",
		Run:    runID,
		Op:     "status",
	})
	if ok, _ := stRes["ok"].(bool); !ok || stRes["isolated"] != true {
		t.Fatalf("run_worktree status failed: %v", stRes)
	}

	// Modify file in worktree
	modFile := filepath.Join(wtPath, "newfile.txt")
	if err := os.WriteFile(modFile, []byte("content from isolated run\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Diff check
	diffRes := execRun(t, a, action{
		Action: "run_worktree",
		Run:    runID,
		Op:     "diff",
	})
	if ok, _ := diffRes["ok"].(bool); !ok {
		t.Fatalf("run_worktree diff failed: %v", diffRes)
	}
	diffText, _ := diffRes["diff"].(string)
	if !strings.Contains(diffText, "newfile.txt") {
		t.Fatalf("expected diff to include newfile.txt, got: %q", diffText)
	}

	// Commit worktree
	commitRes := execRun(t, a, action{
		Action:    "run_worktree",
		Run:       runID,
		Op:        "commit",
		CommitMsg: "feat: add isolated file",
	})
	if ok, _ := commitRes["ok"].(bool); !ok || commitRes["commit"] == "" {
		t.Fatalf("run_worktree commit failed: %v", commitRes)
	}

	// Merge worktree
	mergeRes := execRun(t, a, action{
		Action: "run_worktree",
		Run:    runID,
		Op:     "merge",
	})
	if ok, _ := mergeRes["ok"].(bool); !ok || mergeRes["merged"] != true {
		t.Fatalf("run_worktree merge failed: %v", mergeRes)
	}
	if _, err := os.Stat(filepath.Join(proj, "newfile.txt")); err != nil {
		t.Fatalf("newfile.txt should now exist in project after merge: %v", err)
	}

	// Cleanup worktree
	cleanRes := execRun(t, a, action{
		Action: "run_worktree",
		Run:    runID,
		Op:     "cleanup",
	})
	if ok, _ := cleanRes["ok"].(bool); !ok || cleanRes["cleaned"] != true {
		t.Fatalf("run_worktree cleanup failed: %v", cleanRes)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree path %s should have been removed", wtPath)
	}
}

var _ = context.Background

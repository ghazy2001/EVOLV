package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dxn111/EVOLV/internal/execution"
	"github.com/dxn111/EVOLV/internal/workspace"
)

func TestRunsAPIEndToEnd(t *testing.T) {
	s, _ := newWbServer(t)

	create := `{"title":"ship a feature","objective":"make it real","tasks":[
		{"id":"model","title":"data model","status":"queued"},
		{"id":"api","title":"endpoint","depends_on":["model"]},
		{"id":"tests","title":"regression tests","depends_on":["api"]}
	]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/create", strings.NewReader(create)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create -> %d: %s", rec.Code, rec.Body.String())
	}
	var run struct {
		ID    string `json:"id"`
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.ID == "" || len(run.Tasks) != 3 {
		t.Fatalf("run = %+v", run)
	}

	// advance model through a legal path, with evidence
	for _, step := range []string{"running", "verifying"} {
		body, _ := json.Marshal(map[string]any{"run": run.ID, "task": "model", "status": step})
		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/transition", strings.NewReader(string(body))))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s -> %d: %s", step, rec.Code, rec.Body.String())
		}
	}
	body, _ := json.Marshal(map[string]any{
		"run": run.ID, "task": "model", "status": "completed",
		"verification": map[string]any{"kind": "test", "command": "go test ./...", "passed": true, "exit_code": 0},
	})
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/transition", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("complete -> %d: %s", rec.Code, rec.Body.String())
	}

	// an illegal transition is a conflict, not a silent success
	body, _ = json.Marshal(map[string]any{"run": run.ID, "task": "model", "status": "running"})
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/transition", strings.NewReader(string(body))))
	if rec.Code != http.StatusConflict {
		t.Fatalf("illegal transition -> %d", rec.Code)
	}

	// list + detail expose progress and ready tasks
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), run.ID) {
		t.Fatalf("list -> %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/detail?id="+run.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail -> %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Progress struct {
			Completed int `json:"completed"`
			Total     int `json:"total"`
		} `json:"progress"`
		Ready []struct {
			ID string `json:"id"`
		} `json:"ready"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Progress.Completed != 1 || detail.Progress.Total != 3 {
		t.Fatalf("progress = %+v", detail.Progress)
	}
	if len(detail.Ready) != 1 || detail.Ready[0].ID != "api" {
		t.Fatalf("ready = %+v", detail.Ready)
	}

	// journal events are readable
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/events?id="+run.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "task.moved") {
		t.Fatalf("events -> %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunsAPIValidation(t *testing.T) {
	s, _ := newWbServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/create", strings.NewReader(`{"title":""}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty create -> %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/detail", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id -> %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/detail?id=nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id -> %d", rec.Code)
	}
}

func TestRunsInsightsAfterCompletion(t *testing.T) {
	s, _ := newWbServer(t)
	create := `{"title":"insight run","tasks":[{"id":"a","title":"do it"}]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/create", strings.NewReader(create)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create -> %d", rec.Code)
	}
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"running", "verifying"} {
		body, _ := json.Marshal(map[string]any{"run": run.ID, "task": "a", "status": step})
		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/transition", strings.NewReader(string(body))))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s -> %d", step, rec.Code)
		}
	}
	body, _ := json.Marshal(map[string]any{
		"run": run.ID, "task": "a", "status": "completed",
		"verification": map[string]any{"kind": "test", "command": "go test ./...", "passed": true, "exit_code": 0},
	})
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/transition", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("complete -> %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/insights", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("insights -> %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Insights struct {
			Tasks        int `json:"tasks"`
			VerifiedRate int `json:"verified_rate"`
		} `json:"insights"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Insights.Tasks == 0 {
		t.Fatalf("no outcomes recorded: %s", rec.Body.String())
	}
	if out.Insights.VerifiedRate != 100 {
		t.Fatalf("verified rate = %d", out.Insights.VerifiedRate)
	}
}

func TestRunCancelAndRetryEndpoints(t *testing.T) {
	s, _ := newWbServer(t)
	create := `{"title":"cancel retry run","tasks":[{"id":"t1","title":"task 1"}]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/create", strings.NewReader(create)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create -> %d", rec.Code)
	}
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}

	// Cancel endpoint
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/cancel", strings.NewReader(`{"id":"`+run.ID+`","reason":"stop now"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel -> %d: %s", rec.Code, rec.Body.String())
	}
	var cancelResp struct {
		Ok  bool           `json:"ok"`
		Run *execution.Run `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cancelResp); err != nil || !cancelResp.Ok || cancelResp.Run.Status != execution.StatusCancelled {
		t.Fatalf("cancel response invalid: %s", rec.Body.String())
	}

	// Retry endpoint
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/retry", strings.NewReader(`{"id":"`+run.ID+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("retry -> %d: %s", rec.Code, rec.Body.String())
	}
	var retryResp struct {
		Ok  bool           `json:"ok"`
		Run *execution.Run `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &retryResp); err != nil || !retryResp.Ok || retryResp.Run.Status != execution.StatusQueued {
		t.Fatalf("retry response invalid: %s", rec.Body.String())
	}
}

func TestRunsJsServed(t *testing.T) {
	s, _ := newWbServer(t)
	uiDir := filepath.Join(s.ws.Root, workspace.UIRel())
	if err := os.MkdirAll(uiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "assets", "ui", "runs.js"))
	if err != nil {
		t.Fatalf("failed to read source assets/ui/runs.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "runs.js"), raw, 0o644); err != nil {
		t.Fatalf("failed to write runs.js into workspace UI: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("runs.js -> %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "run-modal") || !strings.Contains(body, "renderRunDetail") {
		t.Fatalf("runs.js does not contain expected inspector elements")
	}
}

func TestRunDetailAndEventsIntegration(t *testing.T) {
	s, _ := newWbServer(t)
	create := `{"title":"inspector run","objective":"verify drawer detail","tasks":[
		{"id":"t1","title":"step 1","objective":"initial task","status":"queued"}
	]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/create", strings.NewReader(create)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create -> %d: %s", rec.Code, rec.Body.String())
	}
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}

	// Verify detail response has all required fields for UI
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/detail?id="+run.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail -> %d: %s", rec.Code, rec.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["run"] == nil || detail["progress"] == nil {
		t.Fatalf("detail missing run or progress: %v", detail)
	}

	// Verify events response has events array
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/events?id="+run.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("events -> %d: %s", rec.Code, rec.Body.String())
	}
	var eventsResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &eventsResp); err != nil {
		t.Fatal(err)
	}
	events, ok := eventsResp["events"].([]any)
	if !ok || len(events) == 0 {
		t.Fatalf("expected non-empty events array, got: %v", eventsResp)
	}
}

func TestRunsWorktreeEndpoints(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	s, proj := newWbServer(t)

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = proj
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
			t.Fatalf("git %s: %s (%v)", strings.Join(args, " "), string(out), err)
		}
	}
	runGit("init")
	runGit("config", "user.name", "tester")
	runGit("config", "user.email", "tester@example.com")
	runGit("config", "commit.gpgsign", "false")
	_ = os.WriteFile(filepath.Join(proj, "README.md"), []byte("# Hello\n"), 0o644)
	runGit("add", "README.md")
	runGit("commit", "-m", "initial")

	create := `{"title":"isolated feature","tasks":[{"id":"t1","title":"do it"}],"isolate":true}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/create", strings.NewReader(create)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create -> %d: %s", rec.Code, rec.Body.String())
	}
	var run struct {
		ID       string `json:"id"`
		Worktree *struct {
			Path   string `json:"path"`
			Branch string `json:"branch"`
			Active bool   `json:"active"`
		} `json:"worktree"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Worktree == nil || !run.Worktree.Active || run.Worktree.Path == "" {
		t.Fatalf("expected active worktree on run, got: %+v", run.Worktree)
	}

	// 2. GET /api/runs/worktree?id=...
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/worktree?id="+run.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("worktree get -> %d: %s", rec.Code, rec.Body.String())
	}
	var getOut struct {
		Isolated bool `json:"isolated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &getOut)
	if !getOut.Isolated {
		t.Fatal("expected isolated: true")
	}

	// 3. Write a file in the worktree
	newFile := filepath.Join(run.Worktree.Path, "worktree_test.txt")
	if err := os.WriteFile(newFile, []byte("created in worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. POST /api/runs/worktree action: diff
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/worktree", strings.NewReader(`{"run":"`+run.ID+`","action":"diff"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("diff -> %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "worktree_test.txt") {
		t.Fatalf("diff missing new file: %s", rec.Body.String())
	}

	// 5. POST /api/runs/worktree action: commit
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/worktree", strings.NewReader(`{"run":"`+run.ID+`","action":"commit","message":"test commit"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("commit -> %d: %s", rec.Code, rec.Body.String())
	}

	// 6. POST /api/runs/worktree action: merge
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/worktree", strings.NewReader(`{"run":"`+run.ID+`","action":"merge"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("merge -> %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(proj, "worktree_test.txt")); err != nil {
		t.Fatalf("file should exist in proj after merge: %v", err)
	}

	// 7. POST /api/runs/worktree action: cleanup
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/worktree", strings.NewReader(`{"run":"`+run.ID+`","action":"cleanup"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup -> %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(run.Worktree.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree path %s should be deleted", run.Worktree.Path)
	}
}

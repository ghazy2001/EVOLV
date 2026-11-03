package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dxn111/EVOLV/internal/execution"
)

// runStore opens the durable run store for an action.
func (a *Agent) runStore() (*execution.Store, error) {
	if err := os.MkdirAll(a.ws.Dir(runsRel), 0o755); err != nil {
		return nil, err
	}
	return execution.Open(a.ws.Dir(runsRel))
}

// runStart creates a durable run from a plan so long assignments survive a
// restart, a crash, or simply a very long conversation.
func (a *Agent) runStart(act action) map[string]any {
	res := map[string]any{"action": "run_work"}
	title := strings.TrimSpace(firstNonEmptyText(act.Title, act.Desc, act.Summary))
	if title == "" {
		return runErr(res, "run_work needs a title")
	}
	var tasks []execution.Task
	for i, step := range act.Steps {
		t := execution.Task{
			ID:        fmt.Sprintf("t%d", i+1),
			Title:     strings.TrimSpace(step.Title),
			Objective: strings.TrimSpace(step.Text),
			Status:    execution.StatusQueued,
			Phase:     strings.TrimSpace(act.Framework),
			DependsOn: normaliseStepDeps(step.After),
			FilesPlan: normaliseStepDeps(step.Files),
		}
		if t.Title == "" {
			t.Title = fmt.Sprintf("step %d", i+1)
		}
		tasks = append(tasks, t)
	}
	if len(tasks) == 0 {
		return runErr(res, "run_work needs steps:[{title}]")
	}
	store, err := a.runStore()
	if err != nil {
		return runErr(res, err.Error())
	}
	defer store.CloseAll()
	run, err := store.CreateRun(execution.NewRunID(), title, strings.TrimSpace(act.Desc), tasks)
	if err != nil {
		return runErr(res, err.Error())
	}

	isolate := act.Isolate
	if raw, ok := act.Args["isolate"].(bool); ok && raw {
		isolate = true
	}
	if raw, ok := act.Args["worktree"].(bool); ok && raw {
		isolate = true
	}

	if isolate {
		repoDir := a.ws.Config.ProjectDir
		if repoDir == "" {
			repoDir = a.ws.Root
		}
		if execution.IsGitRepo(repoDir) {
			wt, err := execution.CreateWorktree(repoDir, run.ID, execution.WorktreeOptions{})
			if err != nil {
				res["worktree_error"] = err.Error()
			} else {
				_ = store.AttachWorktree(run.ID, *wt)
				res["worktree"] = wt.Path
				res["branch"] = wt.Branch
			}
		} else {
			res["worktree_warning"] = "not a git repository; proceeding without worktree isolation"
		}
	}

	res["ok"] = true
	res["run"] = run.ID
	res["tasks"] = len(run.Tasks)
	res["ready"] = idsOf(run.Graph().Ready())
	res["note"] = "durable run created — advance it with run_finish as each task really completes"
	return res
}

// runBegin marks a task as running, after checking its dependencies.
func (a *Agent) runBegin(act action) map[string]any {
	res := map[string]any{"action": "run_begin", "run": act.Run, "task": act.Task}
	store, run, task, err := a.lookupRunTask(act)
	if err != nil {
		return runErr(res, err.Error())
	}
	defer store.CloseAll()
	graph := run.Graph()
	completed := map[string]bool{}
	for _, t := range run.Tasks {
		completed[t.ID] = t.Status == execution.StatusCompleted
	}
	for _, dep := range task.DependsOn {
		if !completed[dep] {
			return runErr(res, fmt.Sprintf("task %s is blocked: dependency %s is not completed", task.ID, dep))
		}
	}
	if _, err := store.Transition(run.ID, task.ID, execution.StatusRunning); err != nil {
		return runErr(res, err.Error())
	}
	res["ok"] = true
	res["status"] = string(execution.StatusRunning)
	res["dependents"] = graph.Dependents(task.ID)
	return res
}

// runFinish records real evidence and only then completes the task.
//
// Rules (deliberately strict — this is the whole point of the feature):
//   - a verify_command is executed here; its exit code is the evidence
//   - no verify_command means the task is completed as "untested", and the
//     result says so instead of pretending
//   - a failing command leaves the task failed and reports the output
func (a *Agent) runFinish(act action) map[string]any {
	res := map[string]any{"action": "run_finish", "run": act.Run, "task": act.Task}
	store, run, task, err := a.lookupRunTask(act)
	if err != nil {
		return runErr(res, err.Error())
	}
	defer store.CloseAll()

	if task.Status == execution.StatusQueued || task.Status == execution.StatusPaused {
		if _, err := store.Transition(run.ID, task.ID, execution.StatusRunning); err != nil {
			return runErr(res, err.Error())
		}
	}
	if _, err := store.Transition(run.ID, task.ID, execution.StatusVerifying); err != nil {
		// already verifying (retry after an error) is fine
		if !strings.Contains(err.Error(), "verifying") {
			return runErr(res, err.Error())
		}
	}

	verify := strings.TrimSpace(act.VerifyCommand)
	if verify != "" {
		v := a.runVerification(run.ID, task.ID, act.VerifyCommand)
		if err := store.RecordVerification(run.ID, task.ID, v); err != nil {
			return runErr(res, err.Error())
		}
		res["exit_code"] = v.ExitCode
		res["passed"] = v.Passed
		res["output"] = clip(v.Output, 800)
		if !v.Passed {
			_, _ = store.Transition(run.ID, task.ID, execution.StatusFailed)
			_ = store.RecordAttempt(run.ID, task.ID, "verification failed: "+v.Command)
			a.recordPlaybook(run.ID, task.ID, task.Title, verify, task.Attempts+1, "failed", v.DurationMS)
			res["ok"] = false
			res["error"] = "verification failed — the task is marked failed, not done"
			res["status"] = string(execution.StatusFailed)
			return res
		}
	}

	if _, err := store.Transition(run.ID, task.ID, execution.StatusCompleted); err != nil {
		return runErr(res, err.Error())
	}
	res["ok"] = true
	res["status"] = string(execution.StatusCompleted)
	if verify == "" {
		res["verification"] = "untested"
		res["warning"] = "no verify_command was run: the task is recorded as implemented-but-unverified"
		a.recordPlaybook(run.ID, task.ID, task.Title, "", task.Attempts, "unverified", 0)
	} else {
		res["verification"] = "verified"
		a.recordPlaybook(run.ID, task.ID, task.Title, verify, task.Attempts, "verified", 0)
	}

	if run.Worktree != nil && run.Worktree.Active {
		commitMsg := strings.TrimSpace(firstNonEmptyText(act.CommitMsg, act.Message))
		if commitMsg != "" {
			if commitHash, err := execution.CommitWorktree(run.Worktree, commitMsg); err == nil {
				run.Worktree.Commit = commitHash
				_ = store.UpdateWorktree(run.ID, *run.Worktree)
				res["commit"] = commitHash
			}
		}
		merge := act.Merge
		if raw, ok := act.Args["merge"].(bool); ok && raw {
			merge = true
		}
		if merge {
			repoDir := a.ws.Config.ProjectDir
			if repoDir == "" {
				repoDir = a.ws.Root
			}
			if err := execution.MergeWorktree(repoDir, run.Worktree); err == nil {
				_ = store.UpdateWorktree(run.ID, *run.Worktree)
				res["merged"] = true
			} else {
				res["merge_error"] = err.Error()
			}
		}
	}

	if updated, ok := store.Get(run.ID); ok {
		progress := updated.Graph().Progress()
		res["progress"] = fmt.Sprintf("%d/%d done", progress.Completed, progress.Total)
		res["ready"] = idsOf(updated.Graph().Ready())
	}
	return res
}

// runFail marks a task failed or blocked with a reason, keeping the graph
// honest about what can never run.
func (a *Agent) runFail(act action) map[string]any {
	res := map[string]any{"action": "run_fail", "run": act.Run, "task": act.Task}
	store, run, task, err := a.lookupRunTask(act)
	if err != nil {
		return runErr(res, err.Error())
	}
	defer store.CloseAll()
	reason := strings.TrimSpace(firstNonEmptyText(act.Reason, act.Message, act.Summary, "failed"))
	target := execution.StatusFailed
	if strings.EqualFold(strings.TrimSpace(act.Status), "cancelled") {
		target = execution.StatusCancelled
	}
	if task.Status != target {
		if _, err := store.Transition(run.ID, task.ID, target); err != nil {
			return runErr(res, err.Error())
		}
	}
	_ = store.RecordAttempt(run.ID, task.ID, reason)
	a.recordPlaybook(run.ID, task.ID, task.Title, "", task.Attempts+1, "failed", 0)
	if updated, ok := store.Get(run.ID); ok {
		graph := updated.Graph()
		res["blocked"] = graph.Blocked()
		progress := graph.Progress()
		res["progress"] = fmt.Sprintf("%d/%d done, %d failed", progress.Completed, progress.Total, progress.Failed)
	}
	res["ok"] = true
	res["status"] = string(target)
	return res
}

// runStatus reports a run's live graph, ready work and evidence.
func (a *Agent) runStatus(act action) map[string]any {
	res := map[string]any{"action": "run_status"}
	store, err := a.runStore()
	if err != nil {
		return runErr(res, err.Error())
	}
	defer store.CloseAll()
	runID := strings.TrimSpace(act.Run)
	if runID == "" {
		runs := store.List()
		list := make([]map[string]any, 0, len(runs))
		for _, r := range runs {
			p := r.Graph().Progress()
			list = append(list, map[string]any{
				"run": r.ID, "title": r.Title, "status": string(r.Status),
				"done": p.Completed, "total": p.Total, "failed": p.Failed,
			})
		}
		res["ok"] = true
		res["runs"] = list
		return res
	}
	run, ok := store.Get(runID)
	if !ok {
		return runErr(res, "run not found: "+runID)
	}
	graph := run.Graph()
	tasks := make([]map[string]any, 0, len(run.Tasks))
	for _, t := range run.Tasks {
		entry := map[string]any{
			"task": t.ID, "title": t.Title, "status": string(t.Status),
			"attempts": t.Attempts, "verified": t.Verified(),
		}
		if len(t.DependsOn) > 0 {
			entry["depends_on"] = t.DependsOn
		}
		if t.LastError != "" {
			entry["last_error"] = clip(t.LastError, 300)
		}
		if len(t.Verify) > 0 {
			last := t.Verify[len(t.Verify)-1]
			entry["last_check"] = fmt.Sprintf("%s → exit %d", last.Command, last.ExitCode)
		}
		tasks = append(tasks, entry)
	}
	progress := graph.Progress()
	res["ok"] = true
	res["run"] = run.ID
	res["status"] = string(run.Status)
	res["progress"] = fmt.Sprintf("%d/%d done", progress.Completed, progress.Total)
	res["ready"] = idsOf(graph.Ready())
	res["blocked"] = graph.Blocked()
	res["tasks"] = tasks
	if run.Worktree != nil {
		res["worktree"] = run.Worktree
	}
	return res
}

// runCancel cancels an in-flight run.
func (a *Agent) runCancel(act action) map[string]any {
	res := map[string]any{"action": "run_cancel", "run": act.Run}
	if act.Run == "" {
		return runErr(res, "run_cancel needs run")
	}
	store, err := a.runStore()
	if err != nil {
		return runErr(res, err.Error())
	}
	defer store.CloseAll()
	run, err := store.Cancel(act.Run, act.Desc)
	if err != nil {
		return runErr(res, err.Error())
	}
	res["ok"] = true
	res["status"] = string(run.Status)
	return res
}

// runRetry requeues failed or cancelled tasks in a run.
func (a *Agent) runRetry(act action) map[string]any {
	res := map[string]any{"action": "run_retry", "run": act.Run}
	if act.Run == "" {
		return runErr(res, "run_retry needs run")
	}
	store, err := a.runStore()
	if err != nil {
		return runErr(res, err.Error())
	}
	defer store.CloseAll()
	run, err := store.Retry(act.Run, act.Task)
	if err != nil {
		return runErr(res, err.Error())
	}
	res["ok"] = true
	res["status"] = string(run.Status)
	res["ready"] = idsOf(run.Graph().Ready())
	return res
}

// runWorktree manages or inspects an isolated git worktree for a run.
func (a *Agent) runWorktree(act action) map[string]any {
	res := map[string]any{"action": "run_worktree", "run": act.Run}
	runID := strings.TrimSpace(act.Run)
	if runID == "" {
		return runErr(res, "run_worktree needs a run id")
	}
	store, err := a.runStore()
	if err != nil {
		return runErr(res, err.Error())
	}
	defer store.CloseAll()
	run, ok := store.Get(runID)
	if !ok {
		return runErr(res, "run not found: "+runID)
	}

	op := strings.ToLower(strings.TrimSpace(firstNonEmptyText(act.Op, act.Status)))
	if raw, ok := act.Args["op"].(string); ok && op == "" {
		op = strings.ToLower(strings.TrimSpace(raw))
	}
	if op == "" {
		op = "status"
	}

	repoDir := a.ws.Config.ProjectDir
	if repoDir == "" {
		repoDir = a.ws.Root
	}

	switch op {
	case "create", "setup":
		if run.Worktree != nil && run.Worktree.Active {
			res["ok"] = true
			res["worktree"] = run.Worktree
			return res
		}
		if !execution.IsGitRepo(repoDir) {
			return runErr(res, "target directory is not a git repository")
		}
		wt, err := execution.CreateWorktree(repoDir, run.ID, execution.WorktreeOptions{})
		if err != nil {
			return runErr(res, err.Error())
		}
		if err := store.AttachWorktree(run.ID, *wt); err != nil {
			return runErr(res, err.Error())
		}
		res["ok"] = true
		res["worktree"] = wt
		return res

	case "status":
		if run.Worktree == nil {
			res["ok"] = true
			res["isolated"] = false
			return res
		}
		res["ok"] = true
		res["isolated"] = true
		res["worktree"] = run.Worktree
		return res

	case "diff":
		if run.Worktree == nil || !run.Worktree.Active {
			return runErr(res, "no active worktree for run "+runID)
		}
		diff, err := execution.DiffWorktree(run.Worktree)
		if err != nil {
			return runErr(res, err.Error())
		}
		res["ok"] = true
		res["diff"] = diff
		return res

	case "commit":
		if run.Worktree == nil || !run.Worktree.Active {
			return runErr(res, "no active worktree for run "+runID)
		}
		msg := strings.TrimSpace(firstNonEmptyText(act.CommitMsg, act.Message, act.Desc, "chore(evolv): commit worktree changes"))
		hash, err := execution.CommitWorktree(run.Worktree, msg)
		if err != nil {
			return runErr(res, err.Error())
		}
		run.Worktree.Commit = hash
		_ = store.UpdateWorktree(run.ID, *run.Worktree)
		res["ok"] = true
		res["commit"] = hash
		return res

	case "merge":
		if run.Worktree == nil || !run.Worktree.Active {
			return runErr(res, "no active worktree for run "+runID)
		}
		if err := execution.MergeWorktree(repoDir, run.Worktree); err != nil {
			return runErr(res, err.Error())
		}
		_ = store.UpdateWorktree(run.ID, *run.Worktree)
		res["ok"] = true
		res["merged"] = true
		return res

	case "remove", "cleanup":
		if run.Worktree == nil {
			res["ok"] = true
			return res
		}
		deleteBranch := act.Force
		if raw, ok := act.Args["delete_branch"].(bool); ok {
			deleteBranch = raw
		}
		if err := execution.RemoveWorktree(repoDir, run.Worktree, deleteBranch); err != nil {
			return runErr(res, err.Error())
		}
		_ = store.DetachWorktree(run.ID)
		res["ok"] = true
		res["cleaned"] = true
		return res

	default:
		return runErr(res, "unknown worktree op: "+op+" (allowed: status, create, diff, commit, merge, cleanup)")
	}
}

// runVerification executes a verification command and captures the evidence.
// It is the same discipline the swarm execution mode uses: only a real exit
// code counts, and only allow-listed commands run.
func (a *Agent) runVerification(runID, taskID, command string) execution.Verification {
	started := time.Now()
	v := execution.Verification{Kind: "test", Command: command, RanAt: started.UTC().Format(time.RFC3339Nano), ExitCode: -1}
	if err := verifyCommandAllowed(command); err != nil {
		v.Output = err.Error()
		v.DurationMS = time.Since(started).Milliseconds()
		return v
	}
	dir := a.ws.Config.ProjectDir
	if dir == "" {
		dir = a.ws.Root
	}
	if store, err := a.runStore(); err == nil {
		if run, ok := store.Get(runID); ok && run.Worktree != nil && run.Worktree.Active && run.Worktree.Path != "" {
			dir = run.Worktree.Path
		}
		store.CloseAll()
	}
	cctx, cancel := context.WithTimeout(context.Background(), maxVerificationSeconds*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CI=1", "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	v.ExitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			v.ExitCode = ee.ExitCode()
		} else {
			v.ExitCode = 1
		}
	}
	if v.ExitCode == 0 {
		v.Passed = true
	}
	v.Output = truncateOutput(string(out), 4000)
	v.DurationMS = time.Since(started).Milliseconds()
	a.bus.Publish("run:evidence", map[string]any{
		"run": runID, "task": taskID, "command": command,
		"exit_code": v.ExitCode, "passed": v.Passed,
	})
	return v
}

// allowedVerifyPrefixes is the allow-list for verification commands. Anything
// else is refused: the agent may not use "verification" as a shell.
var allowedVerifyPrefixes = []string{
	"go test", "go build", "go vet",
	"npm test", "npm run test", "npm run build", "npm run lint",
	"pytest", "python -m pytest", "python3 -m pytest",
	"node --test", "cargo test", "cargo build", "cargo clippy",
	"make test", "make build", "make check", "make lint",
}

// maxVerificationSeconds bounds a single verification run.
const maxVerificationSeconds = 300

// verifyCommandAllowed reports whether a verification command is allow-listed
// and free of shell composition that could chain arbitrary work.
func verifyCommandAllowed(command string) error {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return fmt.Errorf("empty verification command")
	}
	if strings.ContainsAny(cmd, ";&|`$<>\n") {
		return fmt.Errorf("verification command may not contain shell composition: %q", cmd)
	}
	for _, prefix := range allowedVerifyPrefixes {
		if strings.HasPrefix(cmd, prefix) {
			return nil
		}
	}
	return fmt.Errorf("verification command %q is not allow-listed (use go test, go build, npm test, pytest, cargo test, make test …)", cmd)
}

func (a *Agent) lookupRunTask(act action) (*execution.Store, *execution.Run, execution.Task, error) {
	runID := strings.TrimSpace(act.Run)
	taskID := strings.TrimSpace(act.Task)
	if runID == "" || taskID == "" {
		return nil, nil, execution.Task{}, fmt.Errorf("%s needs run and task ids (run_status lists them)", act.Action)
	}
	store, err := a.runStore()
	if err != nil {
		return nil, nil, execution.Task{}, err
	}
	run, ok := store.Get(runID)
	if !ok {
		store.CloseAll()
		return nil, nil, execution.Task{}, fmt.Errorf("run not found: %s", runID)
	}
	for _, t := range run.Tasks {
		if t.ID == taskID {
			return store, run, t, nil
		}
	}
	store.CloseAll()
	return nil, nil, execution.Task{}, fmt.Errorf("task %s not found in run %s", taskID, runID)
}

func runErr(res map[string]any, msg string) map[string]any {
	res["ok"] = false
	res["error"] = msg
	return res
}

func firstNonEmptyText(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func idsOf(tasks []execution.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	return out
}

// normaliseStepDeps trims, de-duplicates and drops empty entries.
func normaliseStepDeps(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

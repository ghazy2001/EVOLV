package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/dxn111/EVOLV/internal/execution"
)

// runsDir is where durable execution runs live inside the genome.
const runsDir = "workspace/execution/runs"

// runsStore lazily opens the execution store. It is opened on first use so a
// broken run directory never blocks boot.
func (s *Server) runsStore() (*execution.Store, error) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if s.execStore != nil {
		return s.execStore, nil
	}
	store, err := execution.Open(s.ws.Dir(runsDir))
	if err != nil {
		return nil, err
	}
	s.execStore = store
	return store, nil
}

// handleRuns lists durable execution runs.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	store, err := s.runsStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	runs := store.List()
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		progress := run.Graph().Progress()
		entry := map[string]any{
			"id": run.ID, "title": run.Title, "objective": run.Objective,
			"status": run.Status, "created_at": run.CreatedAt, "updated_at": run.UpdatedAt,
			"progress": progress,
			"ready":    len(run.Graph().Ready()),
		}
		if run.Worktree != nil {
			entry["worktree"] = run.Worktree
		}
		out = append(out, entry)
	}
	writeJSONOK(w, map[string]any{"runs": out})
}

// handleRunDetail returns one run with its graph, blocked tasks and evidence.
func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "id is required")
		return
	}
	store, err := s.runsStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	run, ok := store.Get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "run not found")
		return
	}
	graph := run.Graph()
	writeJSONOK(w, map[string]any{
		"run":      run,
		"progress": graph.Progress(),
		"blocked":  graph.Blocked(),
		"problems": graph.Validate(),
		"ready":    graph.Ready(),
	})
}

// handleRunEvents returns the journal history for one run.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "id is required")
		return
	}
	store, err := s.runsStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	events, err := store.Events(id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSONOK(w, map[string]any{"id": id, "events": events})
}

// handleRunInsights reports what the outcome log has learned.
func (s *Server) handleRunInsights(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	log, err := execution.OpenOutcomes(s.ws.Dir(runsDir))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	outcomes := log.Read()
	writeJSONOK(w, map[string]any{
		"insights": execution.Summarise(outcomes),
		"recent":   lastOutcomes(outcomes, 20),
	})
}

// lastOutcomes returns the newest n outcomes.
func lastOutcomes(list []execution.Outcome, n int) []execution.Outcome {
	if len(list) <= n {
		return list
	}
	return list[len(list)-n:]
}

// recordOutcomes persists terminal task outcomes so orchestration can learn
// from what actually happened. Errors are not fatal: learning is best-effort.
func (s *Server) recordOutcomes() {
	store, err := s.runsStore()
	if err != nil {
		return
	}
	log, err := execution.OpenOutcomes(s.ws.Dir(runsDir))
	if err != nil {
		return
	}
	for _, run := range store.List() {
		if run.Status.Terminal() {
			log.RecordRunOutcomes(run)
		}
	}
}

// handleRunCreate registers a run from a plan (used by the agent's plan tool
// and by clients that want durable tracking for a multi-step assignment).
func (s *Server) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Title     string           `json:"title"`
		Objective string           `json:"objective"`
		Tasks     []execution.Task `json:"tasks"`
		Isolate   bool             `json:"isolate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Title) == "" || len(in.Tasks) == 0 {
		writeJSONError(w, http.StatusBadRequest, "title and tasks are required")
		return
	}
	store, err := s.runsStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	run, err := store.CreateRun(execution.NewRunID(), in.Title, in.Objective, in.Tasks)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Isolate {
		repoDir := s.ws.Config.ProjectDir
		if repoDir == "" {
			repoDir = s.ws.Root
		}
		if execution.IsGitRepo(repoDir) {
			if wt, err := execution.CreateWorktree(repoDir, run.ID, execution.WorktreeOptions{}); err == nil {
				_ = store.AttachWorktree(run.ID, *wt)
				run.Worktree = wt
			}
		}
	}
	s.recordOutcomes()
	writeJSONOK(w, run)
}

// handleRunTransition moves a task between lifecycle states.
func (s *Server) handleRunTransition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Run        string                  `json:"run"`
		Task       string                  `json:"task"`
		Status     execution.Status        `json:"status"`
		Reason     string                  `json:"reason"`
		Verify     *execution.Verification `json:"verification"`
		Checkpoint string                  `json:"checkpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Run == "" || in.Task == "" || in.Status == "" {
		writeJSONError(w, http.StatusBadRequest, "run, task and status are required")
		return
	}
	store, err := s.runsStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if in.Verify != nil {
		if err := store.RecordVerification(in.Run, in.Task, *in.Verify); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
	}
	if in.Checkpoint != "" {
		_ = store.Checkpoint(in.Run, in.Task, in.Checkpoint)
	}
	task, err := store.Transition(in.Run, in.Task, in.Status)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		if strings.Contains(err.Error(), "cannot move") {
			code = http.StatusConflict
		}
		writeJSONError(w, code, err.Error())
		return
	}
	if in.Status == execution.StatusFailed && in.Reason != "" {
		_ = store.RecordAttempt(in.Run, in.Task, in.Reason)
	}
	type runState struct {
		Run *execution.Run `json:"run"`
	}
	s.recordOutcomes()
	updated, _ := store.Get(in.Run)
	writeJSONOK(w, map[string]any{"task": task, "run": updated})
}

// handleRunCancel cancels an active run and all unfinished tasks.
func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		ID     string `json:"id"`
		Run    string `json:"run"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	id := in.ID
	if id == "" {
		id = in.Run
	}
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "id or run is required")
		return
	}
	store, err := s.runsStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	run, err := store.Cancel(id, in.Reason)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		writeJSONError(w, code, err.Error())
		return
	}
	s.recordOutcomes()
	writeJSONOK(w, map[string]any{"ok": true, "run": run})
}

// handleRunRetry resets failed or cancelled tasks back to queued.
func (s *Server) handleRunRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		ID   string `json:"id"`
		Run  string `json:"run"`
		Task string `json:"task"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	id := in.ID
	if id == "" {
		id = in.Run
	}
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "id or run is required")
		return
	}
	store, err := s.runsStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	run, err := store.Retry(id, in.Task)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		writeJSONError(w, code, err.Error())
		return
	}
	s.recordOutcomes()
	writeJSONOK(w, map[string]any{"ok": true, "run": run})
}

// handleRunWorktree manages or inspects the isolated git worktree for a run.
func (s *Server) handleRunWorktree(w http.ResponseWriter, r *http.Request) {
	store, err := s.runsStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	repoDir := s.ws.Config.ProjectDir
	if repoDir == "" {
		repoDir = s.ws.Root
	}

	if r.Method == http.MethodGet {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSONError(w, http.StatusBadRequest, "id is required")
			return
		}
		run, ok := store.Get(id)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "run not found")
			return
		}
		if run.Worktree == nil {
			writeJSONOK(w, map[string]any{"isolated": false})
			return
		}
		diff, _ := execution.DiffWorktree(run.Worktree)
		writeJSONOK(w, map[string]any{
			"isolated": true,
			"worktree": run.Worktree,
			"diff":     diff,
		})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		return
	}

	var in struct {
		Run          string `json:"run"`
		Action       string `json:"action"` // create | commit | merge | cleanup | diff
		Message      string `json:"message"`
		DeleteBranch bool   `json:"delete_branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Run == "" {
		writeJSONError(w, http.StatusBadRequest, "run is required")
		return
	}
	run, ok := store.Get(in.Run)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "run not found")
		return
	}

	action := strings.ToLower(strings.TrimSpace(in.Action))
	switch action {
	case "create", "setup":
		if run.Worktree != nil && run.Worktree.Active {
			writeJSONOK(w, map[string]any{"ok": true, "worktree": run.Worktree})
			return
		}
		if !execution.IsGitRepo(repoDir) {
			writeJSONError(w, http.StatusBadRequest, "target directory is not a git repository")
			return
		}
		wt, err := execution.CreateWorktree(repoDir, run.ID, execution.WorktreeOptions{})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = store.AttachWorktree(run.ID, *wt)
		writeJSONOK(w, map[string]any{"ok": true, "worktree": wt})

	case "diff":
		if run.Worktree == nil || !run.Worktree.Active {
			writeJSONError(w, http.StatusBadRequest, "no active worktree for run")
			return
		}
		diff, err := execution.DiffWorktree(run.Worktree)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, map[string]any{"ok": true, "diff": diff})

	case "commit":
		if run.Worktree == nil || !run.Worktree.Active {
			writeJSONError(w, http.StatusBadRequest, "no active worktree for run")
			return
		}
		msg := in.Message
		if msg == "" {
			msg = "chore(evolv): commit worktree changes"
		}
		hash, err := execution.CommitWorktree(run.Worktree, msg)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		run.Worktree.Commit = hash
		_ = store.UpdateWorktree(run.ID, *run.Worktree)
		writeJSONOK(w, map[string]any{"ok": true, "commit": hash})

	case "merge":
		if run.Worktree == nil || !run.Worktree.Active {
			writeJSONError(w, http.StatusBadRequest, "no active worktree for run")
			return
		}
		if err := execution.MergeWorktree(repoDir, run.Worktree); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = store.UpdateWorktree(run.ID, *run.Worktree)
		writeJSONOK(w, map[string]any{"ok": true, "merged": true})

	case "cleanup", "remove":
		if run.Worktree == nil {
			writeJSONOK(w, map[string]any{"ok": true})
			return
		}
		if err := execution.RemoveWorktree(repoDir, run.Worktree, in.DeleteBranch); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = store.DetachWorktree(run.ID)
		writeJSONOK(w, map[string]any{"ok": true, "cleaned": true})

	default:
		writeJSONError(w, http.StatusBadRequest, "unknown action: "+in.Action+" (allowed: create, diff, commit, merge, cleanup)")
	}
}

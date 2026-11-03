package execution

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Run is one engineering assignment: a title, a task graph and a lifecycle.
type Run struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Objective string    `json:"objective,omitempty"`
	Status    Status    `json:"status"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
	ClosedAt  string    `json:"closed_at,omitempty"`
	Tasks     []Task    `json:"tasks"`
	Seq       int64     `json:"seq"`
	Worktree  *Worktree `json:"worktree,omitempty"`
}

// Graph returns the run's task graph.
func (r *Run) Graph() Graph { return Graph{Tasks: r.Tasks} }

// Event kinds.
const (
	KindRunCreated      = "run.created"
	KindTaskAdded       = "task.added"
	KindTaskMoved       = "task.moved"
	KindTaskAttempt     = "task.attempt"
	KindTaskError       = "task.error"
	KindVerify          = "verify.added"
	KindCheckpoint      = "checkpoint"
	KindRunClosed       = "run.closed"
	KindRunReopened     = "run.reopened"
	KindWorktreeCreated = "worktree.created"
	KindWorktreeUpdated = "worktree.updated"
	KindWorktreeRemoved = "worktree.removed"
)

// Store owns runs on disk: one directory per run with a journal and a
// snapshot. All mutations hold the store lock, then journal, then snapshot.
type Store struct {
	mu    sync.Mutex
	root  string
	runs  map[string]*Run
	journ map[string]*Journal
}

// Open loads (and recovers) every run under root.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	s := &Store{root: root, runs: map[string]*Run{}, journ: map[string]*Journal{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := s.load(e.Name()); err != nil {
			// A broken run must not block the rest of the workspace.
			continue
		}
	}
	return s, nil
}

func (s *Store) dir(id string) string { return filepath.Join(s.root, id) }

func (s *Store) snapshotPath(id string) string { return filepath.Join(s.dir(id), "run.json") }

func (s *Store) journalPath(id string) string { return filepath.Join(s.dir(id), "journal.ndjson") }

// load reads one run: snapshot first, then journal replay on top.
func (s *Store) load(id string) error {
	run := &Run{ID: id, Status: StatusQueued, Tasks: []Task{}}
	if raw, err := os.ReadFile(s.snapshotPath(id)); err == nil {
		if json.Unmarshal(raw, run) != nil {
			return fmt.Errorf("run %s: unreadable snapshot", id)
		}
		run.ID = id
	}
	events, err := Replay(s.journalPath(id))
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Seq <= run.Seq {
			continue // already folded into the snapshot
		}
		applyEvent(run, ev)
		run.Seq = ev.Seq
	}
	run.Tasks = normalizeRunTasks(run.Tasks)
	// Recovery: a task that was mid-flight when the process died must not
	// pretend to be running. The in-memory view is corrected and journalled so
	// the correction survives the next restart too.
	interrupted := []Task{}
	for _, t := range run.Tasks {
		if t.Status.Active() || t.Status == StatusWaiting {
			interrupted = append(interrupted, t)
		}
	}
	if run.Status.Active() {
		run.Status = StatusQueued
	}
	s.runs[id] = run
	j, _, err := OpenJournal(s.journalPath(id))
	if err != nil {
		return err
	}
	s.journ[id] = j
	for _, t := range interrupted {
		if _, err := s.Transition(id, t.ID, StatusQueued); err == nil {
			_ = s.RecordAttempt(id, t.ID, "interrupted: the process stopped while this task was running")
		}
	}
	return nil
}

// applyEvent folds one journal event into a run.
func applyEvent(run *Run, ev Event) {
	switch ev.Kind {
	case KindRunCreated:
		var data struct {
			Title     string `json:"title"`
			Objective string `json:"objective"`
			Status    Status `json:"status"`
		}
		if json.Unmarshal(ev.Data, &data) == nil {
			run.Title, run.Objective = data.Title, data.Objective
			if data.Status.Valid() {
				run.Status = data.Status
			}
		}
	case KindTaskAdded:
		var t Task
		if json.Unmarshal(ev.Data, &t) == nil && t.ID != "" {
			replaced := false
			for i := range run.Tasks {
				if run.Tasks[i].ID == t.ID {
					run.Tasks[i] = t
					replaced = true
				}
			}
			if !replaced {
				run.Tasks = append(run.Tasks, t)
			}
		}
	case KindTaskMoved:
		var data struct {
			From Status `json:"from"`
			To   Status `json:"to"`
		}
		if json.Unmarshal(ev.Data, &data) == nil {
			if i := taskIndex(run.Tasks, ev.Task); i >= 0 {
				run.Tasks[i].Status = data.To
				run.Tasks[i].UpdatedAt = ev.At
				if data.To == StatusCompleted {
					run.Tasks[i].DoneAt = ev.At
				}
			}
		}
	case KindTaskAttempt:
		if i := taskIndex(run.Tasks, ev.Task); i >= 0 {
			run.Tasks[i].Attempts++
		}
	case KindTaskError:
		var data struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(ev.Data, &data) == nil {
			if i := taskIndex(run.Tasks, ev.Task); i >= 0 {
				run.Tasks[i].LastError = data.Error
			}
		}
	case KindVerify:
		var v Verification
		if json.Unmarshal(ev.Data, &v) == nil {
			if i := taskIndex(run.Tasks, ev.Task); i >= 0 {
				run.Tasks[i].Verify = append(run.Tasks[i].Verify, v)
			}
		}
	case KindCheckpoint:
		var data struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(ev.Data, &data) == nil {
			if i := taskIndex(run.Tasks, ev.Task); i >= 0 {
				run.Tasks[i].Checkpoint = data.ID
			}
		}
	case KindRunClosed:
		var data struct {
			Status Status `json:"status"`
		}
		if json.Unmarshal(ev.Data, &data) == nil && data.Status.Valid() {
			run.Status = data.Status
			run.ClosedAt = ev.At
		}
	case KindRunReopened:
		var data struct {
			Status Status `json:"status"`
		}
		if json.Unmarshal(ev.Data, &data) == nil && data.Status.Valid() {
			run.Status = data.Status
		} else {
			run.Status = StatusQueued
		}
		run.ClosedAt = ""
	case KindWorktreeCreated, KindWorktreeUpdated:
		var wt Worktree
		if json.Unmarshal(ev.Data, &wt) == nil {
			run.Worktree = &wt
		}
	case KindWorktreeRemoved:
		if run.Worktree != nil {
			run.Worktree.Active = false
			run.Worktree.Cleaned = true
		}
	}
	run.UpdatedAt = ev.At
}

func taskIndex(tasks []Task, id string) int {
	for i := range tasks {
		if tasks[i].ID == id {
			return i
		}
	}
	return -1
}

func normalizeRunTasks(tasks []Task) []Task {
	seen := map[string]bool{}
	out := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		if t.ID == "" || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		if !t.Status.Valid() {
			t.Status = StatusQueued
		}
		t.DependsOn = normaliseIDs(t.ID, t.DependsOn)
		if t.CreatedAt == "" {
			t.CreatedAt = timestamp()
		}
		if t.UpdatedAt == "" {
			t.UpdatedAt = t.CreatedAt
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// CreateRun opens a new run with its task graph.
func (s *Store) CreateRun(id, title, objective string, tasks []Task) (*Run, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("run id is required")
	}
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil, fmt.Errorf("invalid run id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[id]; exists {
		return nil, fmt.Errorf("run %q already exists", id)
	}
	if err := os.MkdirAll(s.dir(id), 0o755); err != nil {
		return nil, err
	}
	j, _, err := OpenJournal(s.journalPath(id))
	if err != nil {
		return nil, err
	}
	run := &Run{ID: id, Title: title, Objective: objective, Status: StatusRunning, CreatedAt: timestamp(), UpdatedAt: timestamp()}

	ev, err := j.Append(KindRunCreated, "", map[string]any{"title": title, "objective": objective, "status": StatusRunning})
	if err != nil {
		return nil, err
	}
	run.Seq = ev.Seq
	run.Tasks = normalizeRunTasks(tasks)
	// Validate before journalling so an invalid plan never becomes state.
	if problems := run.Graph().Validate(); len(problems) > 0 {
		_ = j.Close()
		delete(s.journ, id)
		_ = os.RemoveAll(s.dir(id))
		return nil, fmt.Errorf("plan invalid: %s", strings.Join(problems, "; "))
	}
	for i := range run.Tasks {
		ev, err := j.Append(KindTaskAdded, run.Tasks[i].ID, run.Tasks[i])
		if err != nil {
			return nil, err
		}
		run.Seq = ev.Seq
	}
	s.runs[id] = run
	s.journ[id] = j
	if err := s.snapshotLocked(run); err != nil {
		return nil, err
	}
	return cloneRun(run), nil
}

// AddTasks appends tasks to a live run (the model may extend a plan).
func (s *Store) AddTasks(runID string, tasks []Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}
	if run.Status.Terminal() {
		return fmt.Errorf("run %q is already closed", runID)
	}
	for _, t := range tasks {
		if t.ID == "" {
			return fmt.Errorf("task id is required")
		}
		t = mergeTaskDefaults(t)
		ev, err := s.journ[runID].Append(KindTaskAdded, t.ID, t)
		if err != nil {
			return err
		}
		run.Seq = ev.Seq
		applyEvent(run, ev)
	}
	run.Tasks = normalizeRunTasks(run.Tasks)
	return s.snapshotLocked(run)
}

func mergeTaskDefaults(t Task) Task {
	if t.Status == "" {
		t.Status = StatusQueued
	}
	if t.CreatedAt == "" {
		t.CreatedAt = timestamp()
	}
	t.UpdatedAt = timestamp()
	t.DependsOn = normaliseIDs(t.ID, t.DependsOn)
	return t
}

// Transition moves a task to a new status after validating the edge.
func (s *Store) Transition(runID, taskID string, to Status) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return Task{}, fmt.Errorf("run %q not found", runID)
	}
	i := taskIndex(run.Tasks, taskID)
	if i < 0 {
		return Task{}, fmt.Errorf("task %q not found", taskID)
	}
	from := run.Tasks[i].Status
	if !to.Valid() {
		return Task{}, fmt.Errorf("unknown status %q", to)
	}
	if !CanTransition(from, to) {
		return Task{}, &TransitionError{TaskID: taskID, From: from, To: to}
	}
	ev, err := s.journ[runID].Append(KindTaskMoved, taskID, map[string]any{"from": from, "to": to})
	if err != nil {
		return Task{}, err
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
	if to == StatusCompleted {
		s.maybeCloseLocked(run)
	}
	if err := s.snapshotLocked(run); err != nil {
		return Task{}, err
	}
	return run.Tasks[i], nil
}

// RecordAttempt bumps a task's attempt counter and stores the failure reason.
func (s *Store) RecordAttempt(runID, taskID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}
	if taskIndex(run.Tasks, taskID) < 0 {
		return fmt.Errorf("task %q not found", taskID)
	}
	ev, err := s.journ[runID].Append(KindTaskAttempt, taskID, map[string]any{"reason": reason})
	if err != nil {
		return err
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
	if reason != "" {
		ev, err = s.journ[runID].Append(KindTaskError, taskID, map[string]any{"error": reason})
		if err != nil {
			return err
		}
		run.Seq = ev.Seq
		applyEvent(run, ev)
	}
	return s.snapshotLocked(run)
}

// RecordVerification stores a real check result for a task.
func (s *Store) RecordVerification(runID, taskID string, v Verification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}
	if taskIndex(run.Tasks, taskID) < 0 {
		return fmt.Errorf("task %q not found", taskID)
	}
	if v.RanAt == "" {
		v.RanAt = timestamp()
	}
	ev, err := s.journ[runID].Append(KindVerify, taskID, v)
	if err != nil {
		return err
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
	return s.snapshotLocked(run)
}

// Checkpoint records an external snapshot id against a task.
func (s *Store) Checkpoint(runID, taskID, snapshotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}
	ev, err := s.journ[runID].Append(KindCheckpoint, taskID, map[string]any{"id": snapshotID})
	if err != nil {
		return err
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
	return s.snapshotLocked(run)
}

// Close marks a run terminal.
func (s *Store) Close(runID string, status Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}
	ev, err := s.journ[runID].Append(KindRunClosed, "", map[string]any{"status": status})
	if err != nil {
		return err
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
	if j, ok := s.journ[runID]; ok {
		if j.NeedsRoll() {
			_ = j.Roll()
			nj, _, err := OpenJournal(s.journalPath(runID))
			if err == nil {
				s.journ[runID] = nj
			}
		}
	}
	return s.snapshotLocked(run)
}

// maybeCloseLocked closes a run when every task reached a terminal state.
func (s *Store) maybeCloseLocked(run *Run) {
	if run.Status.Terminal() {
		return
	}
	status := StatusCompleted
	for _, t := range run.Tasks {
		switch t.Status {
		case StatusCompleted:
		case StatusFailed, StatusCancelled:
			status = StatusFailed
		default:
			return
		}
	}
	ev, err := s.journ[run.ID].Append(KindRunClosed, "", map[string]any{"status": status})
	if err != nil {
		return
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
}

// Cancel transitions all unfinished tasks to StatusCancelled and closes the run.
func (s *Store) Cancel(runID, reason string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("run %q not found", runID)
	}
	if run.Status.Terminal() {
		return nil, fmt.Errorf("run %q is already %s", runID, run.Status)
	}
	for i := range run.Tasks {
		if !run.Tasks[i].Status.Terminal() {
			from := run.Tasks[i].Status
			ev, err := s.journ[runID].Append(KindTaskMoved, run.Tasks[i].ID, map[string]any{
				"from": from,
				"to":   StatusCancelled,
			})
			if err != nil {
				return nil, err
			}
			run.Seq = ev.Seq
			applyEvent(run, ev)
		}
	}
	ev, err := s.journ[runID].Append(KindRunClosed, "", map[string]any{"status": StatusCancelled})
	if err != nil {
		return nil, err
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
	if err := s.snapshotLocked(run); err != nil {
		return nil, err
	}
	return cloneRun(run), nil
}

// Retry resets failed or cancelled tasks back to StatusQueued and reopens the run if closed.
func (s *Store) Retry(runID, taskID string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("run %q not found", runID)
	}
	retried := 0
	for i := range run.Tasks {
		if taskID != "" && run.Tasks[i].ID != taskID {
			continue
		}
		if run.Tasks[i].Status == StatusFailed || run.Tasks[i].Status == StatusCancelled {
			from := run.Tasks[i].Status
			ev, err := s.journ[runID].Append(KindTaskMoved, run.Tasks[i].ID, map[string]any{
				"from": from,
				"to":   StatusQueued,
			})
			if err != nil {
				return nil, err
			}
			run.Seq = ev.Seq
			applyEvent(run, ev)
			retried++
		}
	}
	if retried == 0 {
		if taskID != "" {
			return nil, fmt.Errorf("task %q is not failed or cancelled", taskID)
		}
		return nil, fmt.Errorf("no failed or cancelled tasks to retry in run %q", runID)
	}
	if run.Status.Terminal() {
		ev, err := s.journ[runID].Append(KindRunReopened, "", map[string]any{"status": StatusQueued})
		if err != nil {
			return nil, err
		}
		run.Seq = ev.Seq
		applyEvent(run, ev)
	}
	if err := s.snapshotLocked(run); err != nil {
		return nil, err
	}
	return cloneRun(run), nil
}

// Get returns a copy of a run.
func (s *Store) Get(runID string) (*Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return nil, false
	}
	return cloneRun(run), true
}

// List returns all runs, newest first.
func (s *Store) List() []*Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Run, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, cloneRun(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Events returns the journal history for one run.
func (s *Store) Events(runID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[runID]; !ok {
		return nil, fmt.Errorf("run %q not found", runID)
	}
	return Replay(s.journalPath(runID))
}

// CloseAll closes every journal.
func (s *Store) CloseAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for _, j := range s.journ {
		if err := j.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// snapshotLocked writes run.json atomically.
func (s *Store) snapshotLocked(run *Run) error {
	run.UpdatedAt = timestamp()
	raw, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	path := s.snapshotPath(run.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AttachWorktree records an allocated isolated worktree for the run.
func (s *Store) AttachWorktree(runID string, wt Worktree) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}
	ev, err := s.journ[runID].Append(KindWorktreeCreated, "", wt)
	if err != nil {
		return err
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
	return s.snapshotLocked(run)
}

// UpdateWorktree updates the worktree state (e.g. after commit or merge).
func (s *Store) UpdateWorktree(runID string, wt Worktree) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}
	ev, err := s.journ[runID].Append(KindWorktreeUpdated, "", wt)
	if err != nil {
		return err
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
	return s.snapshotLocked(run)
}

// DetachWorktree marks the worktree as inactive and cleaned up.
func (s *Store) DetachWorktree(runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}
	ev, err := s.journ[runID].Append(KindWorktreeRemoved, "", nil)
	if err != nil {
		return err
	}
	run.Seq = ev.Seq
	applyEvent(run, ev)
	return s.snapshotLocked(run)
}

func cloneRun(r *Run) *Run {
	cp := *r
	if r.Worktree != nil {
		wtCopy := *r.Worktree
		cp.Worktree = &wtCopy
	}
	cp.Tasks = make([]Task, len(r.Tasks))
	for i, t := range r.Tasks {
		t.DependsOn = append([]string(nil), t.DependsOn...)
		t.FilesPlan = append([]string(nil), t.FilesPlan...)
		t.FilesTouch = append([]string(nil), t.FilesTouch...)
		t.Verify = append([]Verification(nil), t.Verify...)
		cp.Tasks[i] = t
	}
	return &cp
}

// NewRunID mints a sortable run id.
func NewRunID() string { return "run-" + time.Now().UTC().Format("20060102-150405") }

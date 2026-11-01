package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dxn111/EVOLV/internal/workspace"
)

func TestCompleteTaskSuccess(t *testing.T) {
	a := testAgent(t)
	sub, _ := a.bus.Subscribe()

	// 1. Declare a multi-step plan in the active task via set_plan
	execOK(t, a, action{
		Action: "set_plan",
		Title:  "Refactor database queries",
		Steps: []planStepArg{
			{Title: "Audit slow queries", Status: "done"},
			{Title: "Add index to user_id", Status: "doing"},
			{Title: "Verify execution plans", Status: "todo"},
		},
	})

	// 2. Call complete_task action
	res := execOK(t, a, action{
		Action:      "complete_task",
		Summary:     "Optimized query indexes and verified benchmarks",
		Status:      "success",
		Files:       []string{"schema.sql", "queries.go"},
		TestsPassed: 18,
		TestsFailed: 0,
		Confidence:  "high",
		Notes:       "All p99 queries now under 5ms",
	})

	if res["ok"] != true {
		t.Fatalf("complete_task failed: %v", res)
	}
	if res["summary"] != "Optimized query indexes and verified benchmarks" {
		t.Fatalf("unexpected summary: %v", res["summary"])
	}
	if res["status"] != "success" {
		t.Fatalf("unexpected status: %v", res["status"])
	}
	if res["confidence"] != "high" {
		t.Fatalf("unexpected confidence: %v", res["confidence"])
	}

	// 3. Verify task:completed event broadcast over the bus
	var taskEventFound bool
	var planEventFound bool

	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == "task:completed" {
				taskEventFound = true
				var payload map[string]any
				if err := json.Unmarshal(ev.Payload, &payload); err != nil {
					t.Fatalf("failed to unmarshal task:completed payload: %v", err)
				}
				if payload["summary"] != "Optimized query indexes and verified benchmarks" {
					t.Fatalf("event summary mismatch: %v", payload["summary"])
				}
				if payload["status"] != "success" {
					t.Fatalf("event status mismatch: %v", payload["status"])
				}
				if payload["confidence"] != "high" {
					t.Fatalf("event confidence mismatch: %v", payload["confidence"])
				}
			}
			if ev.Type == "plan" {
				planEventFound = true
			}
		default:
			goto eventsDone
		}
	}
eventsDone:

	if !taskEventFound {
		t.Fatal("expected task:completed bus event to be published")
	}
	if !planEventFound {
		t.Fatal("expected plan bus event to be published when steps were completed")
	}

	// 4. Verify all remaining plan steps were marked done
	updatedPlan := a.ws.ReadPlan()
	if updatedPlan == nil {
		t.Fatal("plan missing after completion")
	}
	for i, s := range updatedPlan.Steps {
		if s.Status != "done" {
			t.Fatalf("step %d (%s) should be done, got %s", i, s.Title, s.Status)
		}
	}

	// 5. Verify recorded in evolution ledger
	evols := a.ws.ReadEvolution(1)
	if len(evols) != 1 {
		t.Fatalf("expected 1 evolution entry, got %d", len(evols))
	}
	if evols[0]["kind"] != "task_completion" {
		t.Fatalf("expected kind task_completion, got: %v", evols[0]["kind"])
	}
}

func TestCompleteTaskRejectsEmptySummary(t *testing.T) {
	a := testAgent(t)

	res := execOK(t, a, action{
		Action: "complete_task",
	})

	if res["ok"] != false {
		t.Fatal("complete_task without summary should fail")
	}
	if !strings.Contains(res["error"].(string), "complete_task needs summary") {
		t.Fatalf("unexpected error message: %v", res["error"])
	}
}

func TestCompleteTaskDefaults(t *testing.T) {
	a := testAgent(t)

	res := execOK(t, a, action{
		Action:  "complete_task",
		Summary: "Simple fix",
	})

	if res["ok"] != true {
		t.Fatalf("complete_task failed: %v", res)
	}
	if res["status"] != "success" {
		t.Fatalf("expected default status success, got %v", res["status"])
	}
	if res["confidence"] != "high" {
		t.Fatalf("expected default confidence high, got %v", res["confidence"])
	}
	files, ok := res["files"].([]any)
	if !ok || len(files) != 0 {
		t.Fatalf("expected empty files slice, got: %v", res["files"])
	}
}

func TestCompleteTaskPartialDoesNotCompletePlan(t *testing.T) {
	a := testAgent(t)

	plan := workspace.Plan{
		Title: "Complex Migration",
		Steps: []workspace.PlanStep{
			{Title: "Phase 1", Status: "done"},
			{Title: "Phase 2", Status: "doing"},
		},
	}
	_ = a.ws.WritePlan(plan)

	res := execOK(t, a, action{
		Action:     "complete_task",
		Summary:    "Phase 1 done, Phase 2 hit a blocker",
		Status:     "partial",
		Confidence: "medium",
	})

	if res["ok"] != true {
		t.Fatalf("complete_task failed: %v", res)
	}

	// Verify Phase 2 is still "doing"
	curPlan := a.ws.ReadPlan()
	if curPlan.Steps[1].Status != "doing" {
		t.Fatalf("Phase 2 should still be doing on partial completion, got: %s", curPlan.Steps[1].Status)
	}
}

func TestCompleteTaskRejectsUnsupportedStatus(t *testing.T) {
	a := testAgent(t)

	res := execOK(t, a, action{
		Action:  "complete_task",
		Summary: "Finished work",
		Status:  "sucess",
	})

	if res["ok"] != false {
		t.Fatalf("expected invalid status to be rejected, got: %v", res)
	}
	if !strings.Contains(res["error"].(string), "invalid complete_task status") {
		t.Fatalf("unexpected error message: %v", res["error"])
	}
}

func TestCompleteTaskDoesNotCompleteStaleUnrelatedPlan(t *testing.T) {
	a := testAgent(t)

	// Persist a leftover plan from an earlier task without touching set_plan in this turn
	stalePlan := workspace.Plan{
		Title: "Previous unfinished mission",
		Steps: []workspace.PlanStep{
			{Title: "Step 1", Status: "done"},
			{Title: "Step 2", Status: "doing"},
		},
	}
	if err := a.ws.WritePlan(stalePlan); err != nil {
		t.Fatal(err)
	}

	res := execOK(t, a, action{
		Action:  "complete_task",
		Summary: "Answered quick unrelated question",
		Status:  "success",
	})
	if res["ok"] != true {
		t.Fatalf("complete_task failed: %v", res)
	}

	// Stale plan from previous task must remain untouched
	after := a.ws.ReadPlan()
	if after.Steps[1].Status != "doing" {
		t.Fatalf("stale plan step should remain doing, got %s", after.Steps[1].Status)
	}

	// Explicit plan_title matching the persisted plan completes it
	res2 := execOK(t, a, action{
		Action:    "complete_task",
		Summary:   "Finished previous mission explicitly",
		Status:    "success",
		PlanTitle: "Previous unfinished mission",
	})
	if res2["ok"] != true {
		t.Fatalf("complete_task with matching plan_title failed: %v", res2)
	}
	after2 := a.ws.ReadPlan()
	if after2.Steps[1].Status != "done" {
		t.Fatalf("matched plan step should be marked done, got %s", after2.Steps[1].Status)
	}
}

func TestActionFilesDualUnmarshaling(t *testing.T) {
	var completeAct action
	if err := json.Unmarshal([]byte(`{"action":"complete_task","summary":"done","files":["a.go","b.go"]}`), &completeAct); err != nil {
		t.Fatalf("failed to unmarshal complete_task files: %v", err)
	}
	if len(completeAct.Files) != 2 || completeAct.Files[0] != "a.go" {
		t.Fatalf("unexpected complete_task files: %+v", completeAct.Files)
	}

	var modifyAct action
	if err := json.Unmarshal([]byte(`{"action":"self_modify","files":[{"path":"x.go","content":"package x"}]}`), &modifyAct); err != nil {
		t.Fatalf("failed to unmarshal self_modify files: %v", err)
	}
	if len(modifyAct.FileMutations) != 1 || modifyAct.FileMutations[0].Path != "x.go" {
		t.Fatalf("unexpected self_modify file mutations: %+v", modifyAct.FileMutations)
	}
}

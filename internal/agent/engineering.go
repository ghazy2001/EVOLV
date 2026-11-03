package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dxn111/EVOLV/internal/execution"
)

// runsRel is where durable execution runs live inside the genome.
const runsRel = "workspace/execution/runs"

// activeRunSummary is the compact, prompt-sized view of a durable run.
const (
	maxRunsInPrompt   = 3
	maxTasksInRun     = 12
	maxBlockersInList = 5
)

// engineeringSection renders the live state of in-flight engineering work so
// the model always knows what it promised, what is done, and what is blocked —
// without the user having to re-explain the assignment every turn.
//
// It reads the durable store directly (cheap: the store is in memory once
// opened). A broken or absent store simply yields no section.
func (a *Agent) engineeringSection() string {
	root := a.ws.Dir(runsRel)
	if _, err := os.Stat(root); err != nil {
		return ""
	}
	store, err := execution.Open(root)
	if err != nil {
		return ""
	}
	defer store.CloseAll()

	runs := store.List()
	shown := 0
	var b strings.Builder
	for _, run := range runs {
		if shown >= maxRunsInPrompt {
			break
		}
		graph := run.Graph()
		progress := graph.Progress()
		// Finished runs with nothing left to say stay out of the prompt unless
		// they still hold unfinished work.
		if progress.Total > 0 && progress.Completed == progress.Total && run.Status.Terminal() {
			continue
		}
		if shown == 0 {
			b.WriteString("\n\n## IN-FLIGHT ENGINEERING WORK — keep this current\n")
			b.WriteString("Durable runs outlive the conversation. Advance them with run_work (start, isolate:true for git worktree), ")
			b.WriteString("run_finish (record evidence + mark a task done), run_fail (block or fail), ")
			b.WriteString("run_cancel, run_retry, run_worktree (diff, commit, merge, cleanup), and run_status (inspect). Never claim a task is done without a passing verification.\n")
		}
		shown++
		status := string(run.Status)
		fmt.Fprintf(&b, "\n### run %s — %s [%s]\n", run.ID, run.Title, status)
		if run.Worktree != nil && run.Worktree.Active {
			fmt.Fprintf(&b, "worktree: %s [branch: %s]\n", run.Worktree.Path, run.Worktree.Branch)
		}
		if run.Objective != "" {
			b.WriteString("objective: " + clip(run.Objective, 220) + "\n")
		}
		fmt.Fprintf(&b, "progress: %d/%d done", progress.Completed, progress.Total)
		if progress.Failed > 0 {
			fmt.Fprintf(&b, " · %d failed", progress.Failed)
		}
		if progress.Running > 0 {
			fmt.Fprintf(&b, " · %d running", progress.Running)
		}
		b.WriteString("\n")

		ready := graph.Ready()
		if len(ready) > 0 {
			ids := make([]string, 0, len(ready))
			for _, t := range ready {
				ids = append(ids, t.ID)
			}
			b.WriteString("ready now: " + strings.Join(ids, ", ") + "\n")
		}
		blocked := graph.Blocked()
		if len(blocked) > 0 {
			if len(blocked) > maxBlockersInList {
				blocked = blocked[:maxBlockersInList]
			}
			b.WriteString("blocked by a failed dependency: " + strings.Join(blocked, ", ") + "\n")
		}
		shownTasks := 0
		for _, t := range run.Tasks {
			if shownTasks >= maxTasksInRun {
				b.WriteString("  …\n")
				break
			}
			if t.Status == execution.StatusCompleted {
				continue
			}
			shownTasks++
			line := fmt.Sprintf("  - %s [%s] %s", t.ID, t.Status, clip(t.Title, 90))
			if len(t.DependsOn) > 0 {
				line += " (after " + strings.Join(t.DependsOn, ", ") + ")"
			}
			if t.Attempts > 0 {
				line += fmt.Sprintf(" · attempts %d", t.Attempts)
			}
			if t.LastError != "" {
				line += " · last error: " + clip(t.LastError, 120)
			}
			if t.Verified() {
				line += " · verified: yes"
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// RecentVerifications returns the newest passing checks across runs, used by
// the completion report so the model quotes real evidence instead of vibes.
func (a *Agent) RecentVerifications(limit int) []execution.Verification {
	store, err := execution.Open(a.ws.Dir(runsRel))
	if err != nil {
		return nil
	}
	defer store.CloseAll()
	type entry struct {
		at     string
		task   string
		verify execution.Verification
	}
	var all []entry
	for _, run := range store.List() {
		for _, t := range run.Tasks {
			for _, v := range t.Verify {
				all = append(all, entry{at: v.RanAt, task: run.ID + "/" + t.ID, verify: v})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at > all[j].at })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	out := make([]execution.Verification, 0, len(all))
	for _, e := range all {
		v := e.verify
		v.Kind = strings.TrimSpace(v.Kind)
		if v.Command != "" {
			v.Output = "[" + e.task + "] " + clip(v.Output, 200)
		}
		out = append(out, v)
	}
	return out
}

// completionReport builds the structured summary the agent is asked to end
// long work with. It merges what the model says with what the tools actually
// recorded, and never invents a passing result.
type completionReport struct {
	Task         string                   `json:"task"`
	Objectives   []string                 `json:"objectives"`
	Changed      []string                 `json:"changed_files"`
	Tests        []execution.Verification `json:"tests_run"`
	Verification string                   `json:"verification_level"`
	Unverified   []string                 `json:"not_verified"`
	Remaining    []string                 `json:"remaining"`
	Blockers     []string                 `json:"blockers"`
	Confidence   string                   `json:"confidence"`
}

// buildCompletionReport collects evidence-backed facts from the durable store.
func (a *Agent) buildCompletionReport(task string) completionReport {
	rep := completionReport{Task: task, Verification: "untested", Confidence: "low"}
	store, err := execution.Open(a.ws.Dir(runsRel))
	if err == nil {
		defer store.CloseAll()
		passed, failed := 0, 0
		for _, run := range store.List() {
			for _, t := range run.Tasks {
				for _, v := range t.Verify {
					if v.Passed {
						passed++
					} else {
						failed++
					}
					rep.Tests = append(rep.Tests, execution.Verification{
						Kind: v.Kind, Command: v.Command, ExitCode: v.ExitCode,
						Passed: v.Passed, Output: clip(v.Output, 240), RanAt: v.RanAt,
					})
				}
				switch t.Status {
				case execution.StatusCompleted:
					if !t.Verified() {
						rep.Unverified = append(rep.Unverified, t.ID+": "+t.Title+" (no passing check)")
					}
				case execution.StatusFailed:
					rep.Blockers = append(rep.Blockers, t.ID+": "+firstNonEmpty(t.LastError, "failed"))
				default:
					if !t.Status.Terminal() {
						rep.Remaining = append(rep.Remaining, t.ID+": "+t.Title+" ["+string(t.Status)+"]")
					}
				}
			}
		}
		switch {
		case failed > 0:
			rep.Verification, rep.Confidence = "failed", "low"
		case passed > 0 && len(rep.Unverified) == 0 && len(rep.Remaining) == 0:
			rep.Verification, rep.Confidence = "verified", "high"
		case passed > 0:
			rep.Verification, rep.Confidence = "partial", "medium"
		}
	}
	if len(rep.Tests) > 8 {
		rep.Tests = rep.Tests[len(rep.Tests)-8:]
	}
	return rep
}

// renderCompletionReport formats the report as a readable markdown block.
func renderCompletionReport(rep completionReport) string {
	var b strings.Builder
	b.WriteString("### Completion report\n\n")
	b.WriteString("**Task:** " + firstNonEmpty(rep.Task, "(unspecified)") + "\n\n")
	b.WriteString("**Verification level:** " + rep.Verification + " (confidence: " + rep.Confidence + ")\n")
	switch rep.Verification {
	case "verified":
		b.WriteString("\nEvery completed task carries at least one passing command.\n")
	case "partial":
		b.WriteString("\nSome work has passing checks; the rest is implemented but unverified.\n")
	case "failed":
		b.WriteString("\nAt least one check failed — this work is not done.\n")
	default:
		b.WriteString("\nNo command has been run and passed. Treat this as a proposal, not a finished change.\n")
	}
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		b.WriteString("\n**" + title + "**\n")
		for _, item := range items {
			b.WriteString("- " + item + "\n")
		}
	}
	section("Checks run", func() []string {
		out := make([]string, 0, len(rep.Tests))
		for _, t := range rep.Tests {
			line := fmt.Sprintf("`%s` → exit %d (%s)", t.Command, t.ExitCode, passWord(t.Passed))
			out = append(out, line)
		}
		return out
	}())
	if len(rep.Tests) == 0 {
		b.WriteString("\n**Checks run:** none recorded\n")
	}
	section("Implemented but not verified", rep.Unverified)
	section("Blockers", rep.Blockers)
	section("Remaining", rep.Remaining)
	return b.String()
}

func passWord(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// runsDirPresent reports whether the durable store exists on disk.
func (a *Agent) runsDirPresent() bool {
	_, err := os.Stat(filepath.Join(a.ws.Dir(runsRel)))
	return err == nil
}

// clip shortens s to n runes, adding an ellipsis, without splitting runes.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

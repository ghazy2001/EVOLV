package execution

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed, skipping git worktree test")
	}

	dir := t.TempDir()
	runGit := func(args ...string) {
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

	runGit("init")
	runGit("config", "user.name", "tester")
	runGit("config", "user.email", "tester@example.com")
	runGit("config", "commit.gpgsign", "false")

	// Create initial file & commit
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# Test Repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "initial commit")

	return dir
}

func TestWorktreeLifecycle(t *testing.T) {
	repoDir := setupTestGitRepo(t)

	if !IsGitRepo(repoDir) {
		t.Fatal("expected IsGitRepo to return true for initialized repo")
	}

	runID := "test-wt-001"
	wt, err := CreateWorktree(repoDir, runID, WorktreeOptions{})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	if !wt.Active {
		t.Fatalf("expected worktree to be active")
	}
	if !strings.Contains(wt.Branch, runID) {
		t.Fatalf("unexpected branch name: %s", wt.Branch)
	}
	if _, err := os.Stat(wt.Path); os.IsNotExist(err) {
		t.Fatalf("worktree path does not exist: %s", wt.Path)
	}

	// Verify the file from the base repo exists in the worktree
	wtReadme := filepath.Join(wt.Path, "README.md")
	if _, err := os.Stat(wtReadme); err != nil {
		t.Fatalf("README.md should exist in worktree: %v", err)
	}

	// Make an isolated change in the worktree
	featureFile := filepath.Join(wt.Path, "feature.txt")
	if err := os.WriteFile(featureFile, []byte("isolated feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ensure the base repo does NOT see this file (isolation verified!)
	baseFeature := filepath.Join(repoDir, "feature.txt")
	if _, err := os.Stat(baseFeature); !os.IsNotExist(err) {
		t.Fatal("feature.txt leaked into main repository before merge!")
	}

	// Check Diff
	diff, err := DiffWorktree(wt)
	if err != nil {
		t.Fatalf("DiffWorktree error: %v", err)
	}
	if !strings.Contains(diff, "feature.txt") {
		t.Fatalf("diff should mention feature.txt, got: %q", diff)
	}

	// Commit worktree
	commitHash, err := CommitWorktree(wt, "feat: implement isolated feature")
	if err != nil {
		t.Fatalf("CommitWorktree error: %v", err)
	}
	if commitHash == "" || wt.Commit != commitHash {
		t.Fatalf("expected commit hash, got wt.Commit=%q", wt.Commit)
	}

	// Merge into main repo
	if err := MergeWorktree(repoDir, wt); err != nil {
		t.Fatalf("MergeWorktree error: %v", err)
	}
	if !wt.Merged {
		t.Fatal("expected wt.Merged to be true")
	}

	// Verify main repo now has feature.txt
	if _, err := os.Stat(baseFeature); err != nil {
		t.Fatalf("feature.txt should now exist in main repository after merge: %v", err)
	}

	// Remove worktree
	if err := RemoveWorktree(repoDir, wt, true); err != nil {
		t.Fatalf("RemoveWorktree error: %v", err)
	}
	if wt.Active {
		t.Fatal("expected wt.Active to be false after removal")
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree path %s should have been deleted", wt.Path)
	}
}

func TestStoreWorktreeJournalAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.CloseAll()

	runID := "run-wt-journal"
	run, err := store.CreateRun(runID, "Worktree test", "verify worktree persistence", []Task{
		NewTask("t1", "test step"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Worktree != nil {
		t.Fatal("initial run should not have worktree")
	}

	wt := Worktree{
		Path:    filepath.Join(dir, "worktree"),
		Branch:  "evolv/run-wt-journal",
		BaseRef: "main",
		Commit:  "abcdef123456",
		Active:  true,
	}

	if err := store.AttachWorktree(runID, wt); err != nil {
		t.Fatalf("AttachWorktree error: %v", err)
	}

	// Check store in-memory
	got, ok := store.Get(runID)
	if !ok || got.Worktree == nil || got.Worktree.Branch != wt.Branch {
		t.Fatalf("worktree not reflected in store.Get: %+v", got.Worktree)
	}

	// Update worktree (e.g. after commit)
	wt.Commit = "999888777"
	if err := store.UpdateWorktree(runID, wt); err != nil {
		t.Fatalf("UpdateWorktree error: %v", err)
	}

	got, _ = store.Get(runID)
	if got.Worktree.Commit != "999888777" {
		t.Fatalf("expected updated commit, got %s", got.Worktree.Commit)
	}

	// Reopen store from disk and assert replay
	store.CloseAll()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.CloseAll()

	reloaded, ok := reopened.Get(runID)
	if !ok || reloaded.Worktree == nil {
		t.Fatalf("worktree lost after reopen: %+v", reloaded)
	}
	if reloaded.Worktree.Commit != "999888777" || !reloaded.Worktree.Active {
		t.Fatalf("reloaded worktree state mismatch: %+v", reloaded.Worktree)
	}

	// Detach worktree
	if err := reopened.DetachWorktree(runID); err != nil {
		t.Fatalf("DetachWorktree error: %v", err)
	}
	detached, _ := reopened.Get(runID)
	if detached.Worktree.Active || !detached.Worktree.Cleaned {
		t.Fatalf("expected detached/cleaned worktree, got: %+v", detached.Worktree)
	}
}

package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Worktree records an isolated git worktree attached to an engineering run.
type Worktree struct {
	Path      string `json:"path"`              // absolute path to the worktree directory
	Branch    string `json:"branch"`            // git branch name created for this worktree
	BaseRef   string `json:"base_ref"`          // base commit or branch when created
	Commit    string `json:"commit,omitempty"`  // latest commit hash in the worktree
	Active    bool   `json:"active"`            // true while the worktree is actively attached
	Merged    bool   `json:"merged,omitempty"`  // true if merged into the target branch
	Cleaned   bool   `json:"cleaned,omitempty"` // true if removed from disk
	CreatedAt string `json:"created_at,omitempty"`
}

// WorktreeOptions configures worktree creation.
type WorktreeOptions struct {
	BranchPrefix string // default "evolv/run-"
	BaseRef      string // default "HEAD"
	WorktreeDir  string // optional custom directory; defaults to <repoRoot>/.evolv/worktrees/run-<runID>
}

// gitTimeout is the maximum time allowed for any single git command.
const gitTimeout = 30 * time.Second

// gitExec runs a git command in the specified directory.
func gitExec(ctx context.Context, dir string, args ...string) (string, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), gitTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"NO_COLOR=1",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		if trimmed != "" {
			return "", fmt.Errorf("git %s: %s (%w)", strings.Join(args, " "), trimmed, err)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return trimmed, nil
}

// IsGitRepo reports whether dir is inside a git working tree.
func IsGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	out, err := gitExec(nil, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// RepoRoot returns the top-level directory of the git repository enclosing dir.
func RepoRoot(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("directory is empty")
	}
	out, err := gitExec(nil, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	return filepath.Clean(out), nil
}

// CurrentRef returns the current branch name, or the commit SHA if in detached HEAD.
func CurrentRef(dir string) (string, error) {
	branch, err := gitExec(nil, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil && branch != "" && branch != "HEAD" {
		return branch, nil
	}
	return gitExec(nil, dir, "rev-parse", "HEAD")
}

// CreateWorktree allocates a new isolated git worktree for the given runID.
func CreateWorktree(repoDir, runID string, opts WorktreeOptions) (*Worktree, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, errors.New("runID is required")
	}
	root, err := RepoRoot(repoDir)
	if err != nil {
		return nil, err
	}

	branchPrefix := opts.BranchPrefix
	if branchPrefix == "" {
		branchPrefix = "evolv/run-"
	}
	branch := branchPrefix + runID

	baseRef := opts.BaseRef
	if baseRef == "" {
		baseRef = "HEAD"
	}
	baseCurrent, _ := CurrentRef(root)
	if baseCurrent != "" {
		baseRef = baseCurrent
	}

	worktreePath := opts.WorktreeDir
	if worktreePath == "" {
		worktreePath = filepath.Join(root, ".evolv", "worktrees", "run-"+runID)
	}
	worktreePath = filepath.Clean(worktreePath)

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return nil, fmt.Errorf("creating worktree parent dir: %w", err)
	}

	// If the worktree path already exists on disk, prune/remove it cleanly first.
	if _, err := os.Stat(worktreePath); err == nil {
		_, _ = gitExec(nil, root, "worktree", "remove", "--force", worktreePath)
		_ = os.RemoveAll(worktreePath)
		_, _ = gitExec(nil, root, "worktree", "prune")
	}

	// Add worktree on a dedicated branch (-B creates or resets the branch at baseRef).
	_, err = gitExec(nil, root, "worktree", "add", "-B", branch, worktreePath, baseRef)
	if err != nil {
		return nil, fmt.Errorf("creating git worktree: %w", err)
	}

	headCommit, _ := gitExec(nil, worktreePath, "rev-parse", "HEAD")

	return &Worktree{
		Path:      worktreePath,
		Branch:    branch,
		BaseRef:   baseRef,
		Commit:    headCommit,
		Active:    true,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// CommitWorktree stages and commits all changes in the worktree.
// If there are no changes, it returns the current commit hash without creating an empty commit.
func CommitWorktree(wt *Worktree, message string) (string, error) {
	if wt == nil || wt.Path == "" {
		return "", errors.New("worktree is nil or has empty path")
	}
	if !wt.Active {
		return "", errors.New("worktree is not active")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "chore(evolv): update worktree for run"
	}

	// Stage all changes (new, modified, deleted)
	if _, err := gitExec(nil, wt.Path, "add", "-A"); err != nil {
		return "", fmt.Errorf("staging worktree changes: %w", err)
	}

	// Check if working tree is clean
	status, err := gitExec(nil, wt.Path, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("checking worktree status: %w", err)
	}
	if status == "" {
		// Clean, return current HEAD commit
		return gitExec(nil, wt.Path, "rev-parse", "HEAD")
	}

	// Commit
	if _, err := gitExec(nil, wt.Path, "commit", "-m", message); err != nil {
		return "", fmt.Errorf("committing worktree: %w", err)
	}

	newCommit, err := gitExec(nil, wt.Path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	wt.Commit = newCommit
	return newCommit, nil
}

// DiffWorktree reports unstaged and staged changes in the worktree, or diff against base.
func DiffWorktree(wt *Worktree) (string, error) {
	if wt == nil || wt.Path == "" {
		return "", errors.New("worktree is nil or has empty path")
	}
	// Mark untracked files as intent-to-add so newly created files appear as diff additions
	_, _ = gitExec(nil, wt.Path, "add", "-N", ".")
	// Diff against HEAD in the worktree
	diff, err := gitExec(nil, wt.Path, "diff", "HEAD")
	if err == nil && diff != "" {
		return diff, nil
	}
	// If clean against HEAD, check if any untracked or staged changes exist
	status, _ := gitExec(nil, wt.Path, "status", "--short")
	if status != "" {
		return status, nil
	}
	// Check diff against BaseRef if available
	if wt.BaseRef != "" && wt.BaseRef != "HEAD" {
		baseDiff, bErr := gitExec(nil, wt.Path, "diff", wt.BaseRef+"...HEAD")
		if bErr == nil && baseDiff != "" {
			return baseDiff, nil
		}
	}
	return "", nil
}

// MergeWorktree merges the worktree branch into the base repository.
func MergeWorktree(repoDir string, wt *Worktree) error {
	if wt == nil || wt.Branch == "" {
		return errors.New("worktree is nil or missing branch")
	}
	root, err := RepoRoot(repoDir)
	if err != nil {
		return err
	}
	// Commit any uncommitted changes in the worktree first
	_, _ = CommitWorktree(wt, "chore(evolv): final worktree snapshot before merge")

	// Merge the worktree branch into the current branch of the main repo
	_, err = gitExec(nil, root, "merge", "--no-edit", wt.Branch)
	if err != nil {
		return fmt.Errorf("merging worktree branch %s: %w", wt.Branch, err)
	}
	wt.Merged = true
	return nil
}

// RemoveWorktree deletes the worktree from disk and prunes git tracking.
func RemoveWorktree(repoDir string, wt *Worktree, deleteBranch bool) error {
	if wt == nil || wt.Path == "" {
		return nil
	}
	root, err := RepoRoot(repoDir)
	if err != nil {
		root = repoDir
	}

	_, _ = gitExec(nil, root, "worktree", "remove", "--force", wt.Path)
	_, _ = gitExec(nil, root, "worktree", "prune")

	// Windows or locked files safety: ensure directory is cleaned
	_ = os.RemoveAll(wt.Path)

	if deleteBranch && wt.Branch != "" {
		_, _ = gitExec(nil, root, "branch", "-D", wt.Branch)
	}

	wt.Active = false
	wt.Cleaned = true
	return nil
}

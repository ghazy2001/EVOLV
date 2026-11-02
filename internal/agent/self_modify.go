package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dxn111/EVOLV/internal/workspace"
)

// fileMutation represents an inline source file change to apply before compiling.
type fileMutation struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// fileMutationList accepts both protocol shapes models actually emit:
//
//	"files": [{"path":"a.go","content":"..."}]
//	"files": ["a.go","b.go"]              (complete_task uses paths)
//
// Without the second shape, a perfectly reasonable complete_task payload fails
// at JSON decode time and the whole batch is dropped.
type fileMutationList []fileMutation

func (l *fileMutationList) UnmarshalJSON(raw []byte) error {
	var objs []fileMutation
	if err := json.Unmarshal(raw, &objs); err == nil {
		*l = objs
		return nil
	}
	var paths []string
	if err := json.Unmarshal(raw, &paths); err != nil {
		return fmt.Errorf("files must be [{path, content}] or a list of paths")
	}
	out := make(fileMutationList, 0, len(paths))
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, fileMutation{Path: p})
		}
	}
	*l = out
	return nil
}

// selfModifyOptions defines parameters for kernel self-modification.
type selfModifyOptions struct {
	Files          []fileMutation `json:"files,omitempty"`
	Message        string         `json:"message,omitempty"`
	RunTests       bool           `json:"run_tests"`
	TestPkg        string         `json:"test_pkg,omitempty"`
	StageOnly      bool           `json:"stage_only,omitempty"`
	SourceRoot     string         `json:"source_root,omitempty"`
	TargetLauncher string         `json:"target_launcher,omitempty"`
}

// SelfModifyResult summarizes the outcome of a kernel modification, compilation, and swap.
type SelfModifyResult struct {
	OK         bool   `json:"ok"`
	Stage      string `json:"stage,omitempty"` // detect | mutate | compile | test | smoke | swap
	Error      string `json:"error,omitempty"`
	Tag        string `json:"tag,omitempty"`
	SourceRoot string `json:"source_root,omitempty"`
	Candidate  string `json:"candidate,omitempty"`
	Compiled   bool   `json:"compiled"`
	Tested     bool   `json:"tested"`
	Swapped    bool   `json:"swapped"`
	Output     string `json:"output,omitempty"`
	Note       string `json:"note,omitempty"`
}

// SelfModify compiles a candidate build from the Go source tree, runs automated tests,
// verifies binary smoke health, and atomically swaps the launcher on green.
func (a *Agent) SelfModify(opt selfModifyOptions) SelfModifyResult {
	// 1. Detect and validate Go source root
	srcRoot := opt.SourceRoot
	if srcRoot == "" {
		srcRoot = workspace.DetectSourceRoot()
	}
	if srcRoot == "" {
		return SelfModifyResult{
			OK:    false,
			Stage: "detect",
			Error: "Go source root not found (cannot self-modify without local git/source repository)",
		}
	}
	absSrc, err := filepath.Abs(srcRoot)
	if err != nil {
		return SelfModifyResult{OK: false, Stage: "detect", Error: err.Error()}
	}
	srcRoot = absSrc

	modBytes, err := os.ReadFile(filepath.Join(srcRoot, "go.mod"))
	if err != nil || !strings.Contains(string(modBytes), "github.com/dxn111/EVOLV") {
		return SelfModifyResult{
			OK:    false,
			Stage: "detect",
			Error: fmt.Sprintf("invalid source root %q: missing EVOLV go.mod", srcRoot),
		}
	}

	// 2. Apply inline file mutations if provided. Every target is snapshotted before it is
	// written so the source tree is restored unless the whole operation succeeds.
	var snapshots []fileSnapshot
	mutationsKept := false
	defer func() {
		if !mutationsKept {
			restoreSnapshots(snapshots)
		}
	}()

	for _, f := range opt.Files {
		cleanPath := filepath.Clean(f.Path)
		var target string
		if filepath.IsAbs(cleanPath) {
			target = cleanPath
		} else {
			target = filepath.Join(srcRoot, cleanPath)
		}
		target = filepath.Clean(target)
		rel, err := filepath.Rel(srcRoot, target)
		if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
			return SelfModifyResult{
				OK:    false,
				Stage: "mutate",
				Error: fmt.Sprintf("file %q escapes source root %q", f.Path, srcRoot),
			}
		}
		snap, err := takeSnapshot(target)
		if err != nil {
			return SelfModifyResult{OK: false, Stage: "mutate", Error: err.Error()}
		}
		snap.writtenData = []byte(f.Content)
		snapshots = append(snapshots, snap)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return SelfModifyResult{OK: false, Stage: "mutate", Error: err.Error()}
		}
		if err := os.WriteFile(target, []byte(f.Content), 0o644); err != nil {
			return SelfModifyResult{OK: false, Stage: "mutate", Error: err.Error()}
		}
	}

	// 3. Staging directory setup
	tag := time.Now().UTC().Format("20060102-150405")
	candidatesDir := filepath.Join(a.ws.Root, "workspace", "candidates", tag)
	if err := os.MkdirAll(candidatesDir, 0o755); err != nil {
		return SelfModifyResult{OK: false, Stage: "stage", Error: err.Error()}
	}

	binName := "evolv"
	if runtime.GOOS == "windows" {
		binName = "evolv.exe"
	}
	candidateBin := filepath.Join(candidatesDir, binName)

	goTool := findGoBinary()

	// 4. Automated Compilation
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer buildCancel()

	buildCmd := exec.CommandContext(buildCtx, goTool, "build", "-o", candidateBin, "./cmd/evolv")
	buildCmd.Dir = srcRoot
	buildOut, buildErr := buildCmd.CombinedOutput()
	if buildErr != nil {
		outStr := string(buildOut)
		if len(outStr) > 4000 {
			outStr = outStr[:4000] + "\n...(truncated)"
		}
		a.bus.Publish("kernel:modify:failed", map[string]string{
			"stage": "compile",
			"error": outStr,
			"tag":   tag,
		})
		return SelfModifyResult{
			OK:         false,
			Stage:      "compile",
			Error:      fmt.Sprintf("go build failed: %s", outStr),
			Tag:        tag,
			SourceRoot: srcRoot,
			Output:     outStr,
		}
	}

	// 5. Automated Tests (if requested)
	tested := false
	if opt.RunTests {
		testPkg := opt.TestPkg
		if testPkg == "" {
			testPkg = "./cmd/evolv"
		}
		testCtx, testCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer testCancel()

		testCmd := exec.CommandContext(testCtx, goTool, "test", testPkg)
		testCmd.Dir = srcRoot
		testOut, testErr := testCmd.CombinedOutput()
		if testErr != nil {
			outStr := string(testOut)
			if len(outStr) > 4000 {
				outStr = outStr[:4000] + "\n...(truncated)"
			}
			a.bus.Publish("kernel:modify:failed", map[string]string{
				"stage": "test",
				"error": outStr,
				"tag":   tag,
			})
			return SelfModifyResult{
				OK:         false,
				Stage:      "test",
				Error:      fmt.Sprintf("go test %s failed: %s", testPkg, outStr),
				Tag:        tag,
				SourceRoot: srcRoot,
				Candidate:  candidateBin,
				Compiled:   true,
				Output:     outStr,
			}
		}
		tested = true
	}

	// 6. Binary Smoke Health Verification
	smokeCtx, smokeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer smokeCancel()

	if err := smokeTestCandidate(smokeCtx, candidateBin); err != nil {
		a.bus.Publish("kernel:modify:failed", map[string]string{
			"stage": "smoke",
			"error": err.Error(),
			"tag":   tag,
		})
		return SelfModifyResult{
			OK:         false,
			Stage:      "smoke",
			Error:      err.Error(),
			Tag:        tag,
			SourceRoot: srcRoot,
			Candidate:  candidateBin,
			Compiled:   true,
			Tested:     tested,
		}
	}

	// 7. Atomic Swap-on-Green
	swapped := false
	targetLauncher := opt.TargetLauncher
	defaultedLauncher := false
	if targetLauncher == "" {
		targetLauncher = workspace.LauncherPath()
		defaultedLauncher = true
	}

	note := fmt.Sprintf("kernel candidate compiled and verified at %s", candidateBin)

	if !opt.StageOnly && targetLauncher != "" && (!defaultedLauncher || !isTemporaryExecutable(targetLauncher)) {
		if err := swapExecutable(candidateBin, targetLauncher); err != nil {
			a.bus.Publish("kernel:modify:failed", map[string]string{
				"stage": "swap",
				"error": err.Error(),
				"tag":   tag,
			})
			return SelfModifyResult{
				OK:         false,
				Stage:      "swap",
				Error:      fmt.Sprintf("swap failed: %v", err),
				Tag:        tag,
				SourceRoot: srcRoot,
				Candidate:  candidateBin,
				Compiled:   true,
				Tested:     tested,
			}
		}
		swapped = true
		note = fmt.Sprintf("kernel evolved and swapped into %s (candidate archived in %s); restart organism to run new body", targetLauncher, candidatesDir)
	} else if opt.StageOnly {
		note = fmt.Sprintf("kernel candidate compiled and staged at %s (stage_only=true, active binary untouched)", candidateBin)
	} else if defaultedLauncher && isTemporaryExecutable(targetLauncher) {
		note = fmt.Sprintf("kernel candidate compiled and verified at %s (running in temporary runner %s, in-place swap bypassed)", candidateBin, targetLauncher)
	}

	// 8. Commit Mutation and Broadcast Event
	msg := opt.Message
	if msg == "" {
		msg = "kernel evolved to candidate " + tag
	}
	a.commit("self_modify", tag, candidateBin, msg)
	a.bus.Publish("kernel:evolved", map[string]any{
		"tag":         tag,
		"candidate":   candidateBin,
		"source_root": srcRoot,
		"swapped":     swapped,
		"message":     msg,
	})

	mutationsKept = true
	return SelfModifyResult{
		OK:         true,
		Tag:        tag,
		SourceRoot: srcRoot,
		Candidate:  candidateBin,
		Compiled:   true,
		Tested:     tested,
		Swapped:    swapped,
		Note:       note,
	}
}

// findGoBinary locates the go compiler binary via PATH, GOROOT, or standard system paths.
func findGoBinary() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	if goroot := os.Getenv("GOROOT"); goroot != "" {
		candidate := filepath.Join(goroot, "bin", "go")
		if runtime.GOOS == "windows" {
			candidate += ".exe"
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if runtime.GOOS == "windows" {
		defaultWin := `C:\Program Files\Go\bin\go.exe`
		if _, err := os.Stat(defaultWin); err == nil {
			return defaultWin
		}
	}
	return "go"
}

// smokeTestCandidate verifies the candidate binary boots and exits cleanly.
func smokeTestCandidate(ctx context.Context, binPath string) error {
	// Try -version flag
	cmd := exec.CommandContext(ctx, binPath, "-version")
	out, err := cmd.CombinedOutput()
	if err == nil && strings.Contains(strings.ToLower(string(out)), "evolv") {
		return nil
	}

	// Try version subcommand
	cmd = exec.CommandContext(ctx, binPath, "version")
	out, err = cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	// Try --help flag
	cmd = exec.CommandContext(ctx, binPath, "--help")
	out, err = cmd.CombinedOutput()
	if err == nil || (cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0) {
		return nil
	}

	return fmt.Errorf("candidate binary %s failed smoke test: %s", filepath.Base(binPath), string(out))
}

// isTemporaryExecutable checks if an executable path is a temporary test or go-run artifact.
func isTemporaryExecutable(path string) bool {
	if path == "" {
		return true
	}
	clean := filepath.Clean(path)
	tempDir := filepath.Clean(os.TempDir())
	if strings.HasPrefix(clean, tempDir) {
		return true
	}
	lower := strings.ToLower(clean)
	if strings.Contains(lower, "go-build") || strings.Contains(lower, `\temp\`) || strings.Contains(lower, "/tmp/") {
		return true
	}
	return false
}

// swapExecutable performs an atomic replacement of dst with src, rolling back on failure.
func swapExecutable(src, dst string) error {
	dst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}

	staged, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".new-*")
	if err != nil {
		return fmt.Errorf("failed to stage candidate binary: %w", err)
	}
	stagedPath := staged.Name()
	_ = staged.Close()
	defer os.Remove(stagedPath) // no-op once the staged file has been renamed into place

	if err := copyExecutable(src, stagedPath); err != nil {
		return fmt.Errorf("failed to copy candidate binary: %w", err)
	}
	if err := os.Chmod(stagedPath, 0o755); err != nil {
		return fmt.Errorf("failed to mark candidate binary executable: %w", err)
	}

	old := dst + ".old"
	_ = os.Remove(old)

	hadExisting := true
	if err := os.Rename(dst, old); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to backup existing binary: %w", err)
		}
		hadExisting = false
	}

	if err := os.Rename(stagedPath, dst); err != nil {
		if hadExisting {
			_ = os.Rename(old, dst) // rollback
		}
		return fmt.Errorf("failed to install candidate binary: %w", err)
	}

	if hadExisting {
		_ = os.Remove(old)
	}
	return nil
}

// fileSnapshot records the prior state of a mutation target so it can be restored.
type fileSnapshot struct {
	path        string
	existed     bool
	data        []byte
	writtenData []byte
	mode        os.FileMode
}

// takeSnapshot captures the current contents and permissions of path, or its absence.
func takeSnapshot(path string) (fileSnapshot, error) {
	snap := fileSnapshot{path: path}
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return snap, nil
		}
		return snap, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return snap, fmt.Errorf("cannot mutate symlink target: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return snap, err
	}
	snap.existed = true
	snap.data = data
	snap.mode = fi.Mode().Perm()
	return snap, nil
}

// restoreSnapshots undoes recorded mutations in reverse order.
func restoreSnapshots(snapshots []fileSnapshot) {
	for i := len(snapshots) - 1; i >= 0; i-- {
		s := snapshots[i]
		if !s.existed {
			// Only remove if the file still matches the content we wrote (no newer external edits)
			if cur, err := os.ReadFile(s.path); err == nil && (len(s.writtenData) == 0 || bytes.Equal(cur, s.writtenData)) {
				_ = os.Remove(s.path)
			}
			continue
		}
		// Only restore if current file still matches what this mutation wrote (preserve user edits made during build/test)
		if cur, err := os.ReadFile(s.path); err == nil && (len(s.writtenData) == 0 || bytes.Equal(cur, s.writtenData)) {
			_ = os.WriteFile(s.path, s.data, s.mode)
		}
	}
}

// copyExecutable copies file contents from src to dst with 0755 permissions.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

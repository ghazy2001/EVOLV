package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TestFailure describes a single failing test or build error.
type TestFailure struct {
	Name    string `json:"name"`
	Package string `json:"package,omitempty"`
	Output  string `json:"output,omitempty"`
}

// TestResult represents the structured results of running a test suite.
type TestResult struct {
	Framework  string        `json:"framework"`
	Target     string        `json:"target"`
	Passed     int           `json:"passed"`
	Failed     int           `json:"failed"`
	Skipped    int           `json:"skipped"`
	Total      int           `json:"total"`
	DurationMS int64         `json:"duration_ms"`
	Failures   []TestFailure `json:"failures,omitempty"`
	Summary    string        `json:"summary"`
	Raw        string        `json:"raw,omitempty"`
}

// detectTestFramework probes dir and parents for test suite indicators.
func detectTestFramework(dir string) (string, error) {
	d := dir
	for i := 0; i < 4; i++ {
		if fileExists(filepath.Join(d, "go.mod")) {
			return "go", nil
		}
		if fileExists(filepath.Join(d, "package.json")) {
			return "node", nil
		}
		for _, py := range []string{"pytest.ini", "pyproject.toml", "setup.py", "requirements.txt", "tox.ini"} {
			if fileExists(filepath.Join(d, py)) {
				return "python", nil
			}
		}
		if fileExists(filepath.Join(d, "Cargo.toml")) {
			return "cargo", nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", errors.New("could not detect test framework (supported: go, node, python, cargo)")
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// runTestsSuite executes tests for the specified directory and framework.
func runTestsSuite(ctx context.Context, framework, targetDir string) (*TestResult, error) {
	switch framework {
	case "go":
		return runGoTests(ctx, targetDir)
	case "node":
		return runNodeTests(ctx, targetDir)
	case "python":
		return runPythonTests(ctx, targetDir)
	case "cargo":
		return runCargoTests(ctx, targetDir)
	default:
		return nil, fmt.Errorf("unsupported test framework: %s", framework)
	}
}

// ── Go (go test -json) ───────────────────────────────────────────────────────

type goTestEvent struct {
	Time        time.Time `json:"Time"`
	Action      string    `json:"Action"`
	Package     string    `json:"Package"`
	Test        string    `json:"Test"`
	Elapsed     float64   `json:"Elapsed"`
	Output      string    `json:"Output"`
	ImportPath  string    `json:"ImportPath"`
	FailedBuild string    `json:"FailedBuild"`
}

func runGoTests(ctx context.Context, targetDir string) (*TestResult, error) {
	cmd := exec.CommandContext(ctx, "go", "test", "-json", "./...")
	cmd.Dir = targetDir
	out, err := cmd.CombinedOutput()
	// An exit code error from go test is expected when tests fail; parse output first.
	res := parseGoTestJSON(out)
	res.Framework = "go"
	res.Target = targetDir
	if err != nil && res.Failed == 0 {
		res.Failed++
		res.Total++
		res.Failures = append(res.Failures, TestFailure{
			Name:   "go test",
			Output: truncateOutput(string(out), 4000),
		})
		res.Summary = fmt.Sprintf("go test exited abnormally: %v", err)
	}
	return &res, nil
}

func parseGoTestJSON(raw []byte) TestResult {
	var (
		passed       int
		failed       int
		skipped      int
		failures     []TestFailure
		outputs      = map[string][]string{}
		buildOutputs = map[string][]string{}
		pkgFails     = map[string]string{}
		start        = time.Now()
		hasTime      bool
		minTime      time.Time
		maxTime      time.Time
	)

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var ev goTestEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		if !ev.Time.IsZero() {
			if !hasTime || ev.Time.Before(minTime) {
				minTime = ev.Time
				hasTime = true
			}
			if ev.Time.After(maxTime) {
				maxTime = ev.Time
			}
		}

		key := ev.Package
		if ev.Test != "" {
			key = ev.Package + "/" + ev.Test
		}

		if ev.Action == "output" && ev.Output != "" {
			outputs[key] = append(outputs[key], ev.Output)
		}
		if ev.Action == "build-output" && ev.Output != "" {
			buildOutputs[ev.ImportPath] = append(buildOutputs[ev.ImportPath], ev.Output)
		}

		switch ev.Action {
		case "pass":
			if ev.Test != "" {
				passed++
			}
		case "skip":
			if ev.Test != "" {
				skipped++
			}
		case "fail":
			if ev.Test != "" {
				failed++
				failOut := strings.Join(outputs[key], "")
				failures = append(failures, TestFailure{
					Name:    ev.Test,
					Package: ev.Package,
					Output:  cleanTestOutput(failOut),
				})
			} else {
				// Package-level failure (e.g. compilation error or setup panic)
				pkgOut := strings.Join(outputs[key], "")
				if ev.FailedBuild != "" {
					pkgOut += strings.Join(buildOutputs[ev.FailedBuild], "")
				}
				pkgFails[ev.Package] = pkgOut
			}
		}
	}

	// Catch package-level build errors where no individual test ran
	for pkg, out := range pkgFails {
		hasTestFail := false
		for _, f := range failures {
			if f.Package == pkg {
				hasTestFail = true
				break
			}
		}
		if !hasTestFail {
			failed++
			failures = append(failures, TestFailure{
				Name:    pkg,
				Package: pkg,
				Output:  cleanTestOutput(out),
			})
		}
	}

	total := passed + failed + skipped
	var durationMS int64
	if hasTime && !maxTime.IsZero() && !minTime.IsZero() {
		durationMS = maxTime.Sub(minTime).Milliseconds()
	} else {
		durationMS = time.Since(start).Milliseconds()
	}

	summary := fmt.Sprintf("%d passed, %d failed, %d skipped in %.2fs",
		passed, failed, skipped, float64(durationMS)/1000.0)
	if failed > 0 {
		summary = fmt.Sprintf("%d failed, %d passed in %.2fs", failed, passed, float64(durationMS)/1000.0)
	}

	return TestResult{
		Framework:  "go",
		Passed:     passed,
		Failed:     failed,
		Skipped:    skipped,
		Total:      total,
		DurationMS: durationMS,
		Failures:   failures,
		Summary:    summary,
		Raw:        truncateOutput(string(raw), 2000),
	}
}

// ── Node (npm test) ─────────────────────────────────────────────────────────

func runNodeTests(ctx context.Context, targetDir string) (*TestResult, error) {
	cmd := exec.CommandContext(ctx, "npm", "test")
	cmd.Dir = targetDir
	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start).Milliseconds()
	exitCode := 0
	if err != nil {
		exitCode = 1
	}
	res := parseNodeTestOutput(out, exitCode)
	res.Framework = "node"
	res.Target = targetDir
	res.DurationMS = dur
	return &res, nil
}

var (
	nodeTestsLineRe = regexp.MustCompile(`(?im)^\s*Tests:.*$`)
	nodePassRe      = regexp.MustCompile(`(?i)(?:tests?:\s+)?(\d+)\s+passed`)
	nodeFailRe      = regexp.MustCompile(`(?i)(?:tests?:\s+)?(\d+)\s+failed`)
	nodeSkipRe      = regexp.MustCompile(`(?i)(?:tests?:\s+)?(\d+)\s+(?:skipped|todo)`)
)

func parseNodeTestOutput(raw []byte, exitCode int) TestResult {
	text := string(raw)
	summaryLine := nodeTestsLineRe.FindString(text)
	if summaryLine == "" {
		summaryLine = text
	}
	passed := extractFirstInt(nodePassRe, summaryLine)
	failed := extractFirstInt(nodeFailRe, summaryLine)
	skipped := extractFirstInt(nodeSkipRe, summaryLine)

	if failed == 0 && exitCode != 0 {
		failed = 1
	}
	total := passed + failed + skipped

	var failures []TestFailure
	if failed > 0 {
		failures = append(failures, TestFailure{
			Name:   "node-tests",
			Output: truncateOutput(text, 3000),
		})
	}

	summary := fmt.Sprintf("%d passed, %d failed in test suite", passed, failed)
	return TestResult{
		Framework: "node",
		Passed:    passed,
		Failed:    failed,
		Skipped:   skipped,
		Total:     total,
		Failures:  failures,
		Summary:   summary,
		Raw:       truncateOutput(text, 2000),
	}
}

// ── Python (pytest) ─────────────────────────────────────────────────────────

func runPythonTests(ctx context.Context, targetDir string) (*TestResult, error) {
	cmd := exec.CommandContext(ctx, "pytest", "-q", "--tb=short")
	cmd.Dir = targetDir
	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start).Milliseconds()
	exitCode := 0
	if err != nil {
		exitCode = 1
	}
	res := parsePytestOutput(out, exitCode)
	res.Framework = "python"
	res.Target = targetDir
	res.DurationMS = dur
	return &res, nil
}

var (
	pytestPassRe = regexp.MustCompile(`(\d+)\s+passed`)
	pytestFailRe = regexp.MustCompile(`(\d+)\s+(?:failed|error)`)
	pytestSkipRe = regexp.MustCompile(`(\d+)\s+skipped`)
)

func parsePytestOutput(raw []byte, exitCode int) TestResult {
	text := string(raw)
	passed := extractFirstInt(pytestPassRe, text)
	failed := extractFirstInt(pytestFailRe, text)
	skipped := extractFirstInt(pytestSkipRe, text)

	if failed == 0 && exitCode != 0 {
		failed = 1
	}
	total := passed + failed + skipped

	var failures []TestFailure
	if failed > 0 {
		failures = append(failures, TestFailure{
			Name:   "pytest",
			Output: truncateOutput(text, 3000),
		})
	}

	summary := fmt.Sprintf("%d passed, %d failed, %d skipped", passed, failed, skipped)
	return TestResult{
		Framework: "python",
		Passed:    passed,
		Failed:    failed,
		Skipped:   skipped,
		Total:     total,
		Failures:  failures,
		Summary:   summary,
		Raw:       truncateOutput(text, 2000),
	}
}

// ── Cargo (cargo test) ──────────────────────────────────────────────────────

var cargoResultRe = regexp.MustCompile(`test result: (?:ok|FAILED)\.\s+(\d+)\s+passed;\s+(\d+)\s+failed;\s+(\d+)\s+ignored`)

func runCargoTests(ctx context.Context, targetDir string) (*TestResult, error) {
	cmd := exec.CommandContext(ctx, "cargo", "test")
	cmd.Dir = targetDir
	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start).Milliseconds()
	exitCode := 0
	if err != nil {
		exitCode = 1
	}
	res := parseCargoTestOutput(out, exitCode)
	res.Framework = "cargo"
	res.Target = targetDir
	res.DurationMS = dur
	return &res, nil
}

func parseCargoTestOutput(raw []byte, exitCode int) TestResult {
	text := string(raw)
	var passed, failed, skipped int
	for _, m := range cargoResultRe.FindAllStringSubmatch(text, -1) {
		p, _ := strconv.Atoi(m[1])
		f, _ := strconv.Atoi(m[2])
		s, _ := strconv.Atoi(m[3])
		passed += p
		failed += f
		skipped += s
	}
	if failed == 0 && exitCode != 0 {
		failed = 1
	}

	total := passed + failed + skipped
	var failures []TestFailure
	if failed > 0 {
		failures = append(failures, TestFailure{
			Name:   "cargo-test",
			Output: truncateOutput(text, 3000),
		})
	}

	return TestResult{
		Framework: "cargo",
		Passed:    passed,
		Failed:    failed,
		Skipped:   skipped,
		Total:     total,
		Failures:  failures,
		Summary:   fmt.Sprintf("%d passed, %d failed", passed, failed),
		Raw:       truncateOutput(text, 2000),
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func extractFirstInt(re *regexp.Regexp, s string) int {
	matches := re.FindStringSubmatch(s)
	if len(matches) >= 2 {
		if n, err := strconv.Atoi(matches[1]); err == nil {
			return n
		}
	}
	return 0
}

func cleanTestOutput(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 4000 {
		s = s[:4000] + "\n…(truncated)"
	}
	return s
}

func truncateOutput(s string, max int) string {
	if len(s) > max {
		return s[:max] + "\n…(truncated)"
	}
	return s
}

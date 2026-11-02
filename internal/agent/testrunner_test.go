package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- framework detection tests ----------

func TestDetectTestFramework(t *testing.T) {
	tmp := t.TempDir()

	// 1. Unknown
	if _, err := detectTestFramework(tmp); err == nil {
		t.Fatal("expected error on empty dir without framework markers")
	}

	// 2. Go
	goDir := filepath.Join(tmp, "go-proj")
	os.MkdirAll(goDir, 0o755)
	os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module example.com/test\n"), 0o644)
	fw, err := detectTestFramework(goDir)
	if err != nil || fw != "go" {
		t.Fatalf("go detection failed: %s %v", fw, err)
	}

	// Subdirectory of Go module inherits Go
	subDir := filepath.Join(goDir, "pkg", "sub")
	os.MkdirAll(subDir, 0o755)
	fw, err = detectTestFramework(subDir)
	if err != nil || fw != "go" {
		t.Fatalf("go subDir detection failed: %s %v", fw, err)
	}

	// 3. Node
	nodeDir := filepath.Join(tmp, "node-proj")
	os.MkdirAll(nodeDir, 0o755)
	os.WriteFile(filepath.Join(nodeDir, "package.json"), []byte(`{"name":"test"}`), 0o644)
	fw, err = detectTestFramework(nodeDir)
	if err != nil || fw != "node" {
		t.Fatalf("node detection failed: %s %v", fw, err)
	}

	// 4. Python
	pyDir := filepath.Join(tmp, "py-proj")
	os.MkdirAll(pyDir, 0o755)
	os.WriteFile(filepath.Join(pyDir, "pytest.ini"), []byte("[pytest]\n"), 0o644)
	fw, err = detectTestFramework(pyDir)
	if err != nil || fw != "python" {
		t.Fatalf("python detection failed: %s %v", fw, err)
	}

	// 5. Cargo
	cargoDir := filepath.Join(tmp, "cargo-proj")
	os.MkdirAll(cargoDir, 0o755)
	os.WriteFile(filepath.Join(cargoDir, "Cargo.toml"), []byte("[package]\nname = \"foo\""), 0o644)
	fw, err = detectTestFramework(cargoDir)
	if err != nil || fw != "cargo" {
		t.Fatalf("cargo detection failed: %s %v", fw, err)
	}
}

// ---------- go test JSON parser tests ----------

func TestParseGoTestJSON(t *testing.T) {
	// Sample go test -json output with 2 passed, 1 skipped, 1 failed
	sample := `
{"Time":"2026-11-01T12:00:00Z","Action":"run","Package":"example.com/math","Test":"TestAdd"}
{"Time":"2026-11-01T12:00:00.01Z","Action":"output","Package":"example.com/math","Test":"TestAdd","Output":"=== RUN   TestAdd\n"}
{"Time":"2026-11-01T12:00:00.02Z","Action":"pass","Package":"example.com/math","Test":"TestAdd","Elapsed":0.01}
{"Time":"2026-11-01T12:00:00.03Z","Action":"run","Package":"example.com/math","Test":"TestSkip"}
{"Time":"2026-11-01T12:00:00.04Z","Action":"skip","Package":"example.com/math","Test":"TestSkip","Elapsed":0.00}
{"Time":"2026-11-01T12:00:00.05Z","Action":"run","Package":"example.com/math","Test":"TestSub"}
{"Time":"2026-11-01T12:00:00.06Z","Action":"output","Package":"example.com/math","Test":"TestSub","Output":"math_test.go:42: expected 5, got 3\n"}
{"Time":"2026-11-01T12:00:00.07Z","Action":"fail","Package":"example.com/math","Test":"TestSub","Elapsed":0.02}
{"Time":"2026-11-01T12:00:00.08Z","Action":"run","Package":"example.com/math","Test":"TestMul"}
{"Time":"2026-11-01T12:00:00.09Z","Action":"pass","Package":"example.com/math","Test":"TestMul","Elapsed":0.01}
{"Time":"2026-11-01T12:00:00.10Z","Action":"fail","Package":"example.com/math","Elapsed":0.10}
`
	res := parseGoTestJSON([]byte(sample))
	if res.Passed != 2 {
		t.Fatalf("expected 2 passed, got %d", res.Passed)
	}
	if res.Failed != 1 {
		t.Fatalf("expected 1 failed, got %d", res.Failed)
	}
	if res.Skipped != 1 {
		t.Fatalf("expected 1 skipped, got %d", res.Skipped)
	}
	if res.Total != 4 {
		t.Fatalf("expected 4 total, got %d", res.Total)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("expected 1 failure item, got %d", len(res.Failures))
	}
	if res.Failures[0].Name != "TestSub" {
		t.Fatalf("wrong failing test name: %s", res.Failures[0].Name)
	}
	if !strings.Contains(res.Failures[0].Output, "expected 5, got 3") {
		t.Fatalf("failure output missing diagnostic: %s", res.Failures[0].Output)
	}
}

func TestParseGoTestJSONBuildFailure(t *testing.T) {
	// Go 1.24+ package build failure emitting build-output + FailedBuild
	sample := `
{"Time":"2026-11-01T12:00:00Z","Action":"build-output","ImportPath":"example.com/broken [example.com/broken.test]","Output":"# example.com/broken [example.com/broken.test]\n"}
{"Time":"2026-11-01T12:00:00.01Z","Action":"build-output","ImportPath":"example.com/broken [example.com/broken.test]","Output":"main.go:5:2: syntax error: unexpected newline\n"}
{"Time":"2026-11-01T12:00:00.02Z","Action":"build-fail","ImportPath":"example.com/broken [example.com/broken.test]"}
{"Time":"2026-11-01T12:00:00.03Z","Action":"fail","Package":"example.com/broken","Elapsed":0.01,"FailedBuild":"example.com/broken [example.com/broken.test]"}
`
	res := parseGoTestJSON([]byte(sample))
	if res.Failed != 1 {
		t.Fatalf("expected 1 failed, got %d", res.Failed)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("expected 1 failure item, got %d", len(res.Failures))
	}
	if !strings.Contains(res.Failures[0].Output, "syntax error") {
		t.Fatalf("build failure diagnostic missing: %s", res.Failures[0].Output)
	}
}

// ---------- node, pytest, and cargo parsers ----------

func TestParseNodeTestOutput(t *testing.T) {
	sample := `
PASS src/sum.test.js
FAIL src/div.test.js
  ● division by zero

Test Suites: 1 failed, 1 passed, 2 total
Tests:       1 failed, 4 passed, 1 skipped, 6 total
Snapshots:   0 total
Time:        1.234 s
`
	res := parseNodeTestOutput([]byte(sample), 1)
	if res.Passed != 4 || res.Failed != 1 || res.Skipped != 1 || res.Total != 6 {
		t.Fatalf("bad node parsing: %+v", res)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("expected 1 failure item, got %d", len(res.Failures))
	}

	// Empty zero-count successful output does not synthesize a fake passing test
	emptyRes := parseNodeTestOutput([]byte("> my-app@1.0.0 test\n> echo ok\nok\n"), 0)
	if emptyRes.Passed != 0 || emptyRes.Total != 0 || emptyRes.Failed != 0 {
		t.Fatalf("expected zero counts for empty node run, got %+v", emptyRes)
	}
}

func TestParseCargoTestOutput(t *testing.T) {
	sample := `
running 3 tests
test result: ok. 3 passed; 0 failed; 1 ignored; 0 measured; 0 filtered out

running 2 tests
test result: FAILED. 1 passed; 1 failed; 0 ignored; 0 measured; 0 filtered out
`
	res := parseCargoTestOutput([]byte(sample), 101)
	if res.Passed != 4 || res.Failed != 1 || res.Skipped != 1 || res.Total != 6 {
		t.Fatalf("bad cargo multi-target parsing: %+v", res)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("expected 1 cargo failure entry, got %d", len(res.Failures))
	}

	// Non-zero exit after an ok lib target (e.g. later doc-test compile failure) still records failure
	partial := `test result: ok. 2 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out`
	resPartial := parseCargoTestOutput([]byte(partial), 101)
	if resPartial.Failed != 1 {
		t.Fatalf("expected non-zero cargo exit to record failure, got %+v", resPartial)
	}
}

func TestParsePytestOutput(t *testing.T) {
	sample := `
=================== 2 failed, 15 passed, 1 skipped in 0.45s ===================
`
	res := parsePytestOutput([]byte(sample), 1)
	if res.Passed != 15 || res.Failed != 2 || res.Skipped != 1 || res.Total != 18 {
		t.Fatalf("bad pytest parsing: %+v", res)
	}
}

// ---------- integration test: run_tests action ----------

func TestRunTestsActionIntegration(t *testing.T) {
	a := testAgent(t)
	proj := t.TempDir()

	// Setup a real Go module in proj
	goMod := "module example.com/testsuite\n\ngo 1.22\n"
	os.WriteFile(filepath.Join(proj, "go.mod"), []byte(goMod), 0o644)

	// Create test file with 1 pass and 1 intentional fail
	testSrc := `package testsuite

import "testing"

func TestPass(t *testing.T) {}

func TestFail(t *testing.T) {
	t.Fatal("intentional test failure for verification")
}
`
	os.WriteFile(filepath.Join(proj, "suite_test.go"), []byte(testSrc), 0o644)

	// 1. Run tests with failure
	res := execOK(t, a, action{Action: "run_tests", Path: proj})
	if res["ok"] != false {
		t.Fatalf("run_tests should report ok=false when tests fail: %v", res)
	}
	if res["framework"] != "go" {
		t.Fatalf("auto-detected framework should be go: %v", res["framework"])
	}
	if res["passed"].(float64) != 1 || res["failed"].(float64) != 1 {
		t.Fatalf("unexpected counts: %v", res)
	}
	failures, ok := res["failures"].([]any)
	if !ok || len(failures) != 1 {
		t.Fatalf("expected 1 failure in failures list: %v", res["failures"])
	}
	firstFail := failures[0].(map[string]any)
	if firstFail["name"] != "TestFail" {
		t.Fatalf("expected TestFail, got: %v", firstFail["name"])
	}
	if !strings.Contains(firstFail["output"].(string), "intentional test failure") {
		t.Fatalf("failure output missing diagnosis: %v", firstFail["output"])
	}

	// 2. Fix the test and re-run
	fixedSrc := `package testsuite

import "testing"

func TestPass(t *testing.T) {}

func TestFail(t *testing.T) {
	// fixed!
}
`
	os.WriteFile(filepath.Join(proj, "suite_test.go"), []byte(fixedSrc), 0o644)

	res = execOK(t, a, action{Action: "run_tests", Path: proj})
	if res["ok"] != true {
		t.Fatalf("run_tests should report ok=true after fix: %v", res)
	}
	if res["failed"].(float64) != 0 || res["passed"].(float64) != 2 {
		t.Fatalf("expected 2 passed, 0 failed: %v", res)
	}
}

// TestRunTestsInvalidDirectory: missing path reports clean error.
func TestRunTestsInvalidDirectory(t *testing.T) {
	a := testAgent(t)
	res := execOK(t, a, action{Action: "run_tests", Path: filepath.Join(t.TempDir(), "nonexistent")})
	if res["ok"] != false || !strings.Contains(res["error"].(string), "does not exist") {
		t.Fatalf("missing directory should be rejected: %v", res)
	}
}

// TestRunTestsUnsupportedFramework: unsupported framework reports clean error.
func TestRunTestsUnsupportedFramework(t *testing.T) {
	a := testAgent(t)
	dir := t.TempDir()
	res := execOK(t, a, action{Action: "run_tests", Path: dir, Framework: "ruby"})
	if res["ok"] != false || !strings.Contains(res["error"].(string), "unsupported") {
		t.Fatalf("unsupported framework should be rejected: %v", res)
	}
}

func TestRunTestsResolvesRelativePathAgainstWorkspace(t *testing.T) {
	a := testAgent(t)
	relDir := filepath.Join("projects", "submod")
	absDir := filepath.Join(a.ws.Root, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(absDir, "go.mod"), []byte("module example.com/submod\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(absDir, "sub_test.go"), []byte("package submod\nimport \"testing\"\nfunc TestSub(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := execOK(t, a, action{Action: "run_tests", Path: relDir})
	if res["ok"] != true || res["passed"].(float64) != 1 {
		t.Fatalf("expected relative path %q to resolve against workspace root and pass: %v", relDir, res)
	}
}

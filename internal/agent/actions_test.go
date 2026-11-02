package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dxn111/EVOLV/internal/bus"
	"github.com/dxn111/EVOLV/internal/workspace"
)

func testAgent(t *testing.T) *Agent {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ws, err := workspace.Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(ws, bus.New(), nil, "test")
}

func execOK(t *testing.T, a *Agent, act action) map[string]any {
	t.Helper()
	raw := a.Execute(act)
	raw = strings.TrimPrefix(raw, "kernel:")
	// parse last {...}
	i := strings.Index(raw, "{")
	if i < 0 {
		t.Fatalf("no json in %q", raw)
	}
	return nilOrPanic(raw[i:])
}

func nilOrPanic(s string) map[string]any {
	var m map[string]any
	if err := json.NewDecoder(strings.NewReader(s)).Decode(&m); err != nil {
		panic(err)
	}
	return m
}

func TestAbsolutePathsAnywhere(t *testing.T) {
	a := testAgent(t)
	outside := filepath.Join(os.TempDir(), "evolv-outside-test")
	os.MkdirAll(outside, 0o755)
	defer os.RemoveAll(outside)

	res := execOK(t, a, action{Action: "write_file", Path: filepath.Join(outside, "hello.txt"), Content: "hi"})
	if res["ok"] != true {
		t.Fatalf("write outside workspace failed: %v", res)
	}
	res = execOK(t, a, action{Action: "read_file", Path: filepath.Join(outside, "hello.txt")})
	if res["content"] != "hi" {
		t.Fatalf("read outside failed: %v", res)
	}
}

func TestEditFile(t *testing.T) {
	a := testAgent(t)
	p := filepath.Join(a.ws.Root, "t.txt")
	os.WriteFile(p, []byte("alpha beta"), 0o644)
	res := execOK(t, a, action{Action: "edit_file", Path: p, Find: "beta", Replace: "gamma", ExpectedHash: contentHash([]byte("alpha beta"))})
	if res["ok"] != true || res["replacements"].(float64) != 1 {
		t.Fatalf("edit_file: %v", res)
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != "alpha gamma" {
		t.Fatalf("content=%q", raw)
	}
}

func TestEditFileRejectsStaleHash(t *testing.T) {
	a := testAgent(t)
	p := filepath.Join(a.ws.Root, "stale.txt")
	os.WriteFile(p, []byte("newer"), 0o644)
	res := execOK(t, a, action{Action: "edit_file", Path: p, Find: "newer", Replace: "lost", ExpectedHash: contentHash([]byte("older"))})
	if res["ok"] != false || !strings.Contains(fmt.Sprint(res["error"]), "stale-write guard") {
		t.Fatalf("stale edit was not rejected: %v", res)
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != "newer" {
		t.Fatalf("stale edit changed content: %q", raw)
	}
}

func TestSetProjectAndShellCwd(t *testing.T) {
	a := testAgent(t)
	proj := t.TempDir()
	res := execOK(t, a, action{Action: "set_project", Path: proj})
	if res["ok"] != true || a.ws.Config.ProjectDir != proj {
		t.Fatalf("set_project: %v", res)
	}
	cmd := "pwd"
	if runtime.GOOS == "windows" {
		cmd = "cd"
	}
	res = execOK(t, a, action{Action: "shell", Command: cmd})
	out := strings.TrimRight(res["output"].(string), "\r\n")
	if !strings.EqualFold(out, proj) {
		t.Fatalf("shell cwd: got %q, want %q", out, proj)
	}
}

func TestTreeAndListDir(t *testing.T) {
	a := testAgent(t)
	os.MkdirAll(filepath.Join(a.ws.Root, "workspace/ui/components"), 0o755)
	os.WriteFile(filepath.Join(a.ws.Root, "workspace/ui/components/x.html"), []byte("x"), 0o644)
	res := execOK(t, a, action{Action: "tree", Path: "", Depth: 5})
	if res["ok"] != true {
		t.Fatalf("tree: %v", res)
	}
	files := res["files"].([]any)
	if len(files) == 0 {
		t.Fatal("tree empty")
	}
}

func TestSnapshotAndRollback(t *testing.T) {
	a := testAgent(t)
	skill := filepath.Join(a.ws.Root, "workspace/skills/dream.md")
	os.WriteFile(skill, []byte("v1"), 0o644)

	res := execOK(t, a, action{Action: "snapshot"})
	if res["ok"] != true || res["snapshot"] == "" {
		t.Fatalf("snapshot: %v", res)
	}
	id := res["snapshot"].(string)

	// mutate the genome after the snapshot
	os.WriteFile(skill, []byte("v2-broken"), 0o644)
	os.WriteFile(filepath.Join(a.ws.Root, "workspace/ui/index.html"), []byte("<h1>broken</h1>"), 0o644)

	res = execOK(t, a, action{Action: "rollback", Path: id})
	if res["ok"] != true {
		t.Fatalf("rollback: %v", res)
	}
	raw, _ := os.ReadFile(skill)
	if string(raw) != "v1" {
		t.Fatalf("skill not restored: %q", raw)
	}
	raw, _ = os.ReadFile(filepath.Join(a.ws.Root, "workspace/ui/index.html"))
	if string(raw) == "<h1>broken</h1>" {
		t.Fatal("ui not restored")
	}
	// restore with an unknown id must fail cleanly
	res = execOK(t, a, action{Action: "rollback", Path: "19990101-000000"})
	if res["ok"] != false {
		t.Fatalf("bogus rollback should fail: %v", res)
	}
}

func TestSnapshotRestoreWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ws, err := workspace.Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids := ws.ListSnapshots(); len(ids) != 0 {
		t.Fatalf("fresh workspace has snapshots: %v", ids)
	}
	id, err := ws.Snapshot()
	if err != nil || id == "" {
		t.Fatalf("snapshot: %v %v", id, err)
	}
	if ids := ws.ListSnapshots(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("list: %v", ids)
	}
	if err := ws.Restore(id); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := ws.Restore("nope"); err == nil {
		t.Fatal("bogus restore should error")
	}
}

func TestCommitRedactsCredentials(t *testing.T) {
	a := testAgent(t)
	sub, _ := a.bus.Subscribe()

	a.commit(
		"tool",
		"my-key-sk-1234567890abcdef12345678",
		"workspace/tools/ghu_1234567890abcdef1234567890abcdef12/leak.js",
		"setup with ghp_1234567890abcdef1234567890abcdef12 and Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.secret",
	)

	var ev bus.Event
	select {
	case ev = <-sub.Events():
	default:
		t.Fatal("expected evolution bus event")
	}

	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}

	busPath := payload["path"].(string)
	if strings.Contains(busPath, "ghu_1234567890abcdef1234567890abcdef12") {
		t.Fatalf("credential in path leaked to evolution bus event: %s", busPath)
	}
	if !strings.Contains(busPath, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in bus event path: %s", busPath)
	}

	desc := payload["description"].(string)
	if strings.Contains(desc, "ghp_1234567890abcdef1234567890abcdef12") {
		t.Fatalf("GitHub PAT leaked to bus: %s", desc)
	}
	if strings.Contains(desc, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.secret") {
		t.Fatalf("Bearer token leaked to bus: %s", desc)
	}
	if !strings.Contains(desc, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in bus event description: %s", desc)
	}

	history := a.ws.ReadEvolution(1)
	if len(history) != 1 {
		t.Fatalf("expected 1 evolution entry, got %d", len(history))
	}
	entryDesc := history[0]["description"].(string)
	if strings.Contains(entryDesc, "ghp_1234567890abcdef1234567890abcdef12") {
		t.Fatalf("GitHub PAT leaked to ledger: %s", entryDesc)
	}
	if !strings.Contains(entryDesc, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in ledger entry: %s", entryDesc)
	}
}

func TestFormatShellOutputRedactsBeforeTruncation(t *testing.T) {
	prefix := strings.Repeat("A", 7900) + "\n"
	keyBody := "MIIEowIBAAKCAQEA0Y1+" + strings.Repeat("Z", 300)
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + keyBody + "\n-----END RSA PRIVATE KEY-----\n"
	out := formatShellOutput([]byte(prefix + pem))
	if strings.Contains(out, "MIIEowIBAAKCAQEA0Y1+") {
		t.Fatalf("private key body leaked due to pre-redaction truncation: %s", out[7880:])
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in shell output")
	}
}

func TestEvolveComponentSecurityLint(t *testing.T) {
	a := testAgent(t)

	// Insecure component with external CDN script
	insecureAct := action{
		Action: "evolve_component",
		Name:   "insecure_widget",
		HTML:   `<div id="test"></div><script src="https://cdn.jsdelivr.net/vue.js"></script>`,
	}
	res := execOK(t, a, insecureAct)
	if res["ok"] == true {
		t.Fatal("expected insecure component evolution to be blocked")
	}
	if !strings.Contains(res["error"].(string), "security lint failed") {
		t.Fatalf("expected error message to mention security lint failure, got: %v", res["error"])
	}

	// Force override allows write with security report attached
	forcedAct := action{
		Action: "evolve_component",
		Name:   "insecure_widget",
		HTML:   `<div id="test"></div><script src="https://cdn.jsdelivr.net/vue.js"></script>`,
		Force:  true,
	}
	resForced := execOK(t, a, forcedAct)
	if resForced["ok"] != true {
		t.Fatalf("expected forced component write to succeed, got: %v", resForced)
	}
	if resForced["security"] == nil {
		t.Fatal("expected security report in forced result")
	}

	// Clean component succeeds without force
	cleanAct := action{
		Action: "evolve_component",
		Name:   "clean_widget",
		HTML:   `<div id="test"><h3>Clean</h3></div><script>fetch('/api/creations');</script>`,
	}
	resClean := execOK(t, a, cleanAct)
	if resClean["ok"] != true {
		t.Fatalf("expected clean component write to succeed, got: %v", resClean)
	}
}

func TestLintAction(t *testing.T) {
	a := testAgent(t)

	act := action{
		Action:     "lint_component",
		Name:       "eval_widget",
		TargetType: "component",
		HTML:       `<div></div><script>document.cookie = 'x=1';</script>`,
	}
	res := execOK(t, a, act)
	if res["ok"] != true {
		t.Fatalf("lint action failed: %v", res)
	}
	if res["valid"] == true {
		t.Fatal("expected cookie stealer to be invalid")
	}
	if !strings.Contains(res["summary"].(string), "security errors") {
		t.Fatalf("expected summary to mention security errors, got: %v", res["summary"])
	}
}

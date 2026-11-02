package candidate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dxn111/EVOLV/internal/workspace"
)

func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	source, home := t.TempDir(), t.TempDir()
	files := map[string]string{"go.mod": "module candidate.test\n\ngo 1.22\n", "main.go": "package main\nfunc main() {}\n"}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws := &workspace.W{Root: home, Config: &workspace.Config{}}
	if err := os.MkdirAll(ws.Dir(workspace.CandidatesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Manager{WS: ws, SourceRoot: source}, source
}

func TestStageAndPromoteCandidate(t *testing.T) {
	mgr, source := testManager(t)
	original, _ := os.ReadFile(filepath.Join(source, "main.go"))
	next := "package main\nfunc main() { println(\"candidate\") }\n"
	man, err := mgr.Stage(context.Background(), Proposal{Name: "safe change", Files: map[string]string{"main.go": next}, ExpectedHashes: map[string]string{"main.go": hash(original)}})
	if err != nil || man.Status != "ready" {
		t.Fatalf("stage: status=%v err=%v checks=%+v", man, err, man.Checks)
	}
	if live, _ := os.ReadFile(filepath.Join(source, "main.go")); string(live) != string(original) {
		t.Fatal("staging touched living source")
	}
	if len(mgr.List()) != 1 {
		t.Fatal("candidate was not archived")
	}
	promoted, err := mgr.Promote(man.ID)
	if err != nil || promoted.Status != "promoted" {
		t.Fatalf("promote: %v %v", promoted, err)
	}
	if live, _ := os.ReadFile(filepath.Join(source, "main.go")); string(live) != next {
		t.Fatalf("promotion missing: %s", live)
	}
}

func TestCandidateRejectsStaleStageAndPromotion(t *testing.T) {
	mgr, source := testManager(t)
	path := filepath.Join(source, "main.go")
	original, _ := os.ReadFile(path)
	if _, err := mgr.Stage(context.Background(), Proposal{Name: "stale", Files: map[string]string{"main.go": "package main\nfunc main(){}\n"}, ExpectedHashes: map[string]string{"main.go": "sha256:old"}}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale stage accepted: %v", err)
	}
	man, err := mgr.Stage(context.Background(), Proposal{Name: "race", Files: map[string]string{"main.go": "package main\nfunc main(){println(1)}\n"}, ExpectedHashes: map[string]string{"main.go": hash(original)}})
	if err != nil || man.Status != "ready" {
		t.Fatalf("stage: %v %v", man, err)
	}
	if err = os.WriteFile(path, []byte("package main\nfunc main(){println(2)}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = mgr.Promote(man.ID); err == nil || !strings.Contains(err.Error(), "changed since staging") {
		t.Fatalf("stale promotion accepted: %v", err)
	}
}

func TestPromotionRollsBackWhenLiveValidationFails(t *testing.T) {
	mgr, source := testManager(t)
	path := filepath.Join(source, "main.go")
	original, _ := os.ReadFile(path)
	next := "package main\nfunc main(){println(1)}\n"
	man, err := mgr.Stage(context.Background(), Proposal{Name: "rollback", Files: map[string]string{"main.go": next}, ExpectedHashes: map[string]string{"main.go": hash(original)}})
	if err != nil || man.Status != "ready" {
		t.Fatalf("stage: %v %v", man, err)
	}
	if err = os.WriteFile(filepath.Join(source, "broken.go"), []byte("this is not go"), 0o644); err != nil {
		t.Fatal(err)
	}
	rolled, err := mgr.Promote(man.ID)
	if err == nil || rolled == nil || rolled.Status != "promotion_failed" {
		t.Fatalf("failed promotion was not rolled back: %+v %v", rolled, err)
	}
	live, _ := os.ReadFile(path)
	if string(live) != string(original) {
		t.Fatalf("rollback did not restore main.go: %s", live)
	}
}

func TestCandidateRejectsPathEscape(t *testing.T) {
	mgr, _ := testManager(t)
	escapes := []string{
		"/abs/path.go",
		"\\abs\\path.go",
		"../escape.go",
		"..\\escape.go",
		".",
		"..",
	}
	for _, esc := range escapes {
		_, err := mgr.Stage(context.Background(), Proposal{
			Name:  "escape",
			Files: map[string]string{esc: "package main"},
		})
		if err == nil {
			t.Fatalf("expected path %q to be rejected as escaping source root", esc)
		}
	}
}

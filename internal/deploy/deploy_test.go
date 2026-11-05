package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dxn111/EVOLV/internal/workspace"
)

func TestDeployPackage(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)
	t.Setenv("USERPROFILE", td)

	ws, err := workspace.Load(nil)
	if err != nil {
		t.Fatalf("workspace.Load: %v", err)
	}

	mgr := NewManager(ws)
	ctx := context.Background()

	// 1. Deploy default component
	dep, err := mgr.DeployPackage(ctx, "calculator", "local_preview")
	if err != nil {
		t.Fatalf("DeployPackage failed: %v", err)
	}

	if dep.ID == "" {
		t.Fatal("expected non-empty deployment ID")
	}
	if dep.Component != "calculator" {
		t.Fatalf("expected component calculator, got %s", dep.Component)
	}
	if !strings.HasPrefix(dep.URL, "/deploy/") {
		t.Fatalf("expected /deploy/ URL, got %s", dep.URL)
	}

	// Verify disk files
	absIndex := filepath.Join(ws.Root, filepath.FromSlash(dep.DistPath), "index.html")
	raw, err := os.ReadFile(absIndex)
	if err != nil {
		t.Fatalf("failed to read bundle index.html: %v", err)
	}
	if !strings.Contains(string(raw), "calculator") {
		t.Fatalf("expected bundle HTML to contain 'calculator', got: %s", string(raw))
	}

	// 2. Retrieve via Get
	retrieved, err := mgr.Get(dep.ID)
	if err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	if retrieved.ID != dep.ID {
		t.Fatalf("expected ID %s, got %s", dep.ID, retrieved.ID)
	}

	// 3. List
	all := mgr.List()
	if len(all) != 1 {
		t.Fatalf("expected 1 deployment in list, got %d", len(all))
	}
}

func TestDeployPackagePathTraversal(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)
	t.Setenv("USERPROFILE", td)
	ws, err := workspace.Load(nil)
	if err != nil {
		t.Fatalf("workspace.Load: %v", err)
	}

	mgr := NewManager(ws)
	ctx := context.Background()

	badNames := []string{"../../etc/passwd", "../secret", "foo/bar", "foo\\bar"}
	for _, bad := range badNames {
		if _, err := mgr.DeployPackage(ctx, bad, "bundle"); err == nil {
			t.Errorf("expected error for path traversal name %q, got nil", bad)
		}
	}
}

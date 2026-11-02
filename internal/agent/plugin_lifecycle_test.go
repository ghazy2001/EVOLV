package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dxn111/EVOLV/internal/workspace"
)

func writeTestPlugin(t *testing.T, a *Agent, name, code string) {
	t.Helper()
	dir := filepath.Join(a.ws.Dir(workspace.PluginsDir), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"`+name+`","perms":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.js"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pluginSnapshot(a *Agent) []*pluginVM {
	a.js.mu.Lock()
	defer a.js.mu.Unlock()
	return append([]*pluginVM{}, a.js.vms...)
}

func stopped(p *pluginVM) bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

// LoadAll is a rebuild used by rollback and startup. Before the fix it appended
// a second copy of every plugin on each call, leaving duplicate timers alive.
func TestLoadAllRetiresPreviousPluginSandboxes(t *testing.T) {
	a := testAgent(t)
	writeTestPlugin(t, a, "pulse", `evolv.every(1000, function(){});`)

	a.js.LoadAll()
	first := pluginSnapshot(a)
	if len(first) != 1 {
		t.Fatalf("expected 1 plugin after first load, got %d", len(first))
	}

	a.js.LoadAll()
	second := pluginSnapshot(a)
	if len(second) != 1 {
		t.Fatalf("reload leaked plugin sandboxes: got %d", len(second))
	}
	if first[0] == second[0] {
		t.Fatal("reload kept the old sandbox instead of building a fresh one")
	}
	if !stopped(first[0]) {
		t.Fatal("old plugin sandbox was not stopped on reload")
	}
	if stopped(second[0]) {
		t.Fatal("fresh plugin sandbox is already stopped")
	}
}

func TestEvolvePluginReplacesOlderSandbox(t *testing.T) {
	a := testAgent(t)

	res := execOK(t, a, action{
		Action: "evolve_plugin", Name: "live", Desc: "first",
		Code: `evolv.log("v1");`,
	})
	if res["ok"] != true {
		t.Fatalf("first evolve_plugin failed: %v", res)
	}
	first := pluginSnapshot(a)
	if len(first) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(first))
	}

	res = execOK(t, a, action{
		Action: "evolve_plugin", Name: "live", Desc: "second",
		Code: `evolv.log("v2");`,
	})
	if res["ok"] != true {
		t.Fatalf("second evolve_plugin failed: %v", res)
	}
	if res["replaced"] != float64(1) {
		t.Fatalf("expected replaced=1, got %v", res["replaced"])
	}
	second := pluginSnapshot(a)
	if len(second) != 1 {
		t.Fatalf("re-evolving leaked sandboxes: got %d", len(second))
	}
	if !stopped(first[0]) {
		t.Fatal("old plugin sandbox still running after re-evolve")
	}
	if names := a.js.PluginNames(); len(names) != 1 {
		t.Fatalf("plugin names duplicated: %v", names)
	}
}

// A tool file removed from the genome must disappear from the runtime on the
// next rebuild; LoadAll resets the tool map instead of merging into it.
func TestLoadAllClearsRemovedTools(t *testing.T) {
	a := testAgent(t)
	toolPath := filepath.Join(a.ws.Dir(workspace.ToolsDir), "echo.js")
	if err := os.WriteFile(toolPath, []byte(`function echo(args){ return args; }`), 0o644); err != nil {
		t.Fatal(err)
	}
	a.js.LoadAll()
	if tools := a.js.ListTools(); len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tool not loaded: %+v", tools)
	}

	if err := os.Remove(toolPath); err != nil {
		t.Fatal(err)
	}
	a.js.LoadAll()
	if tools := a.js.ListTools(); len(tools) != 0 {
		t.Fatalf("removed tool still live after rebuild: %+v", tools)
	}
}

func TestLoadAllSkipsPluginsWithoutManifest(t *testing.T) {
	a := testAgent(t)
	dir := filepath.Join(a.ws.Dir(workspace.PluginsDir), "quarantined")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate partially unquarantined or stray plugin.js with plugin.json.quarantine
	if err := os.WriteFile(filepath.Join(dir, "plugin.json.quarantine"), []byte(`{"name":"quarantined"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.js"), []byte(`evolv.log("should not run");`), 0o644); err != nil {
		t.Fatal(err)
	}

	a.js.LoadAll()
	if got := pluginSnapshot(a); len(got) != 0 {
		t.Fatalf("expected plugin without plugin.json to be skipped, got %d sandboxes", len(got))
	}

	if err := os.Rename(filepath.Join(dir, "plugin.json.quarantine"), filepath.Join(dir, "plugin.json")); err != nil {
		t.Fatal(err)
	}
	a.js.LoadAll()
	if got := pluginSnapshot(a); len(got) != 1 {
		t.Fatalf("expected plugin with restored plugin.json to load, got %d sandboxes", len(got))
	}
}

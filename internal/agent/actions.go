package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"

	"github.com/dxn111/EVOLV/internal/update"
	"github.com/dxn111/EVOLV/internal/redact"
	"github.com/dxn111/EVOLV/internal/workspace"
)

// JSRuntime executes tools and plugins the agent grows into itself.
// goja is a pure-Go ES5.1+ interpreter: no cgo, cross-compiles everywhere.
type JSRuntime struct {
	a                 *Agent
	vm                *goja.Runtime
	mu                sync.Mutex
	tools             map[string]toolDef
	vms               []*pluginVM // one isolated runtime per plugin
	toolTimeout       time.Duration
	pluginLoadTimeout time.Duration
}

type toolDef struct {
	Name string `json:"name"`
	Desc string `json:"description"`
	fn   goja.Callable
}

// NewJSRuntime creates the sandbox.
func NewJSRuntime(a *Agent) *JSRuntime {
	vm := goja.New()
	vm.SetMaxCallStackSize(DefaultMaxCallStackSize)
	r := &JSRuntime{
		a:                 a,
		vm:                vm,
		tools:             map[string]toolDef{},
		toolTimeout:       DefaultToolTimeout,
		pluginLoadTimeout: DefaultPluginLoadTimeout,
	}
	_ = vm.Set("evolv", map[string]any{
		"log": func(args ...goja.Value) {
			parts := make([]string, len(args))
			for i, v := range args {
				parts[i] = v.String()
			}
			r.a.bus.Publish("plugin:log", map[string]string{"text": strings.Join(parts, " ")})
		},
		"toast": func(msg string) {
			r.a.bus.Publish("plugin:toast", map[string]string{"text": msg})
		},
		"workspace_dir": func() string { return r.a.ws.Root },
		"http":          httpFetch,
	})
	return r
}

// SetToolTimeout sets the execution timeout for tool execution.
func (r *JSRuntime) SetToolTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolTimeout = d
}

// SetPluginLoadTimeout sets the timeout for compiling/loading plugins.
func (r *JSRuntime) SetPluginLoadTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pluginLoadTimeout = d
}

// LoadAll loads every tool and plugin currently in the genome.
func (r *JSRuntime) LoadAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadToolsLocked()
	r.loadPluginsLocked()
}

func (r *JSRuntime) loadToolsLocked() {
	ents, err := os.ReadDir(r.a.ws.Dir(workspace.ToolsDir))
	if err != nil {
		return
	}
	names := []string{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(r.a.ws.Dir(workspace.ToolsDir), n))
		if err != nil {
			continue
		}
		r.evalToolLocked(strings.TrimSuffix(n, ".js"), string(raw))
	}
}

// evalToolLocked compiles one tool file: `function name(args){...}` exported
// either as a bare function declaration or via `export default`.
func (r *JSRuntime) evalToolLocked(name, code string) {
	src := "(function(){" + code + "; return typeof " + sanitizeIdent(name) + "==='function'? " + sanitizeIdent(name) + " : null})()"
	timeout := r.toolTimeout
	if timeout <= 0 {
		timeout = DefaultPluginLoadTimeout
	}
	var val goja.Value
	err := runWithTimeout(r.vm, timeout, func() error {
		var evalErr error
		val, evalErr = r.vm.RunString(src)
		return evalErr
	})
	if err != nil {
		if r.a != nil && r.a.bus != nil {
			r.a.bus.Publish("plugin:log", map[string]string{"text": "tool " + name + " failed: " + err.Error()})
		}
		return
	}
	if goja.IsUndefined(val) || goja.IsNull(val) {
		return
	}
	if fn, ok := goja.AssertFunction(val); ok {
		r.tools[name] = toolDef{Name: name, fn: fn}
	}
}

var identRe = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func sanitizeIdent(s string) string { return identRe.ReplaceAllString(s, "_") }

func (r *JSRuntime) loadPluginsLocked() {
	ents, err := os.ReadDir(r.a.ws.Dir(workspace.PluginsDir))
	if err != nil {
		return
	}
	names := []string{}
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		codePath := filepath.Join(r.a.ws.Dir(workspace.PluginsDir), n, "plugin.js")
		raw, err := os.ReadFile(codePath)
		if err != nil {
			continue
		}
		// r.mu is held; buildPluginVM must not re-lock it.
		perms := pluginPermsFromManifest(r.a, n)
		p, err := r.buildPluginVM(n, string(raw), perms)
		if err != nil {
			r.a.bus.Publish("plugin:log", map[string]string{"plugin": n, "text": "load failed: " + err.Error()})
			continue
		}
		r.vms = append(r.vms, p)
		go p.loop()
	}
}

// ListTools returns grown tools.
func (r *JSRuntime) ListTools() []toolDef {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]toolDef, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, toolDef{Name: t.Name, Desc: t.Desc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RunTool executes a grown tool with JSON args.
func (r *JSRuntime) RunTool(name string, args map[string]any) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("no such tool: %s", name)
	}
	timeout := r.toolTimeout
	if timeout <= 0 {
		timeout = DefaultToolTimeout
	}
	argVal := r.vm.ToValue(args)
	var res goja.Value
	err := runWithTimeout(r.vm, timeout, func() error {
		var callErr error
		res, callErr = t.fn(goja.Undefined(), argVal)
		return callErr
	})
	if err != nil {
		return nil, err
	}
	return res.Export(), nil
}

// Execute performs one action and returns a kernel result line.
func (a *Agent) Execute(act action) string {
	res := map[string]any{"action": act.Action}
	switch act.Action {
	case "evolve_component":
		name := sanitizeName(act.Name)
		if name == "" || act.HTML == "" {
			res["ok"] = false
			res["error"] = "evolve_component needs name + html"
			break
		}
		path := filepath.Join(a.ws.Dir(workspace.ComponentsDir), name+".html")
		if err := os.WriteFile(path, []byte(act.HTML), 0o644); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		a.commit("component", name, path, act.Desc)
		res["ok"] = true
		res["path"] = "workspace/ui/components/" + name + ".html"
		res["note"] = "component hot-mounted live"

	case "remove_component":
		name := sanitizeName(act.Name)
		path := filepath.Join(a.ws.Dir(workspace.ComponentsDir), name+".html")
		if err := os.Remove(path); err != nil {
			res["ok"], res["error"] = false, err.Error()
		} else {
			a.commit("remove_component", name, path, "removed")
			res["ok"] = true
		}

	case "evolve_tool":
		name := sanitizeName(act.Name)
		if name == "" || act.Code == "" {
			res["ok"] = false
			res["error"] = "evolve_tool needs name + code"
			break
		}
		path := filepath.Join(a.ws.Dir(workspace.ToolsDir), name+".js")
		if err := os.WriteFile(path, []byte(act.Code), 0o644); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		a.js.mu.Lock()
		a.js.evalToolLocked(name, act.Code)
		a.js.mu.Unlock()
		a.commit("tool", name, path, act.Desc)
		res["ok"] = true
		note := "tool compiled into the sandbox and callable via run_tool"
		if len(act.Schema) > 0 {
			// typed contract: persisted manifest, versioned, run_tool now validates
			m := &toolManifest{Name: name, Description: act.Desc, Schema: act.Schema}
			ver, err := writeToolManifest(a, m)
			if err != nil {
				res["ok"], res["error"] = false, err.Error()
				break
			}
			res["version"] = ver
			note = fmt.Sprintf("tool v%d compiled with typed manifest — run_tool validates args against the schema", ver)
		}
		res["note"] = note

	case "run_tool":
		if m := readToolManifest(a, sanitizeName(act.Name)); m != nil && len(m.Schema) > 0 {
			// typed manifest: reject bad calls before the code runs
			args := act.Args
			if args == nil {
				args = map[string]any{}
			}
			if err := validateToolArgs(m.Schema, args); err != nil {
				res["ok"], res["error"] = false, err.Error()
				break
			}
		}
		out, err := a.js.RunTool(act.Name, act.Args)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
		} else {
			raw, _ := json.Marshal(out)
			res["ok"] = true
			res["result"] = json.RawMessage(raw)
		}

	case "list_tools":
		// introspection: every grown tool with its typed manifest
		tools := a.js.ListTools()
		type toolInfo struct {
			Name        string            `json:"name"`
			Description string            `json:"description"`
			Version     int               `json:"version"`
			Schema      map[string]string `json:"schema,omitempty"`
		}
		out := make([]toolInfo, 0, len(tools))
		for _, t := range tools {
			ti := toolInfo{Name: t.Name, Description: t.Desc}
			if m := readToolManifest(a, t.Name); m != nil {
				ti.Version, ti.Schema = m.Version, m.Schema
				if ti.Description == "" {
					ti.Description = m.Description
				}
			}
			out = append(out, ti)
		}
		res["ok"] = true
		res["tools"] = out
		res["count"] = len(out)

	case "evolve_skill":
		name := sanitizeName(act.Name)
		if name == "" || act.Content == "" {
			res["ok"] = false
			res["error"] = "evolve_skill needs name + content"
			break
		}
		path := filepath.Join(a.ws.Dir(workspace.SkillsDir), name+".md")
		if err := os.WriteFile(path, []byte(act.Content), 0o644); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		a.commit("skill", name, path, act.Desc)
		res["ok"] = true

	case "evolve_prompt":
		name := sanitizeName(act.Name)
		if name == "" || act.Content == "" {
			res["ok"] = false
			res["error"] = "evolve_prompt needs name + content"
			break
		}
		path := filepath.Join(a.ws.Dir(workspace.PromptsDir), name+".md")
		if err := os.WriteFile(path, []byte(act.Content), 0o644); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		a.commit("prompt", name, path, act.Desc)
		res["ok"] = true
		res["note"] = "system prompt rewritten; takes effect next turn"

	case "evolve_plugin":
		name := sanitizeName(act.Name)
		if name == "" || act.Code == "" {
			res["ok"] = false
			res["error"] = "evolve_plugin needs name + code"
			break
		}
		dir := filepath.Join(a.ws.Dir(workspace.PluginsDir), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		manifest, _ := json.MarshalIndent(map[string]any{
			"name": name, "description": act.Desc, "version": "0.1.0", "author": "evolv-agent",
			"perms": act.Perms,
		}, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), manifest, 0o644); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.js"), []byte(act.Code), 0o644); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		// hot-load into its own sandbox; the plugin is ALIVE the moment this returns
		a.js.mu.Lock()
		p, err := a.js.buildPluginVM(name, act.Code, act.Perms)
		if err == nil {
			a.js.vms = append(a.js.vms, p)
		}
		a.js.mu.Unlock()
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			a.commit("plugin", name, filepath.Join(dir, "plugin.js"), act.Desc+" (loaded with error)")
			break
		}
		go p.loop()
		a.commit("plugin", name, filepath.Join(dir, "plugin.js"), act.Desc)
		res["ok"] = true
		res["note"] = "plugin installed in its own sandbox; evolv.on/every are live now"

	case "write_file":
		p, ok := a.resolvePath(act.Path, &res)
		if !ok {
			break
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		if err := os.WriteFile(p, []byte(act.Content), 0o644); err != nil {
			res["ok"], res["error"] = false, err.Error()
		} else {
			a.commit("file", act.Path, p, act.Desc)
			res["ok"] = true
			res["path"] = p
		}

	case "edit_file":
		p, ok := a.resolvePath(act.Path, &res)
		if !ok {
			break
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		src := string(raw)
		if act.Find == "" || !strings.Contains(src, act.Find) {
			res["ok"] = false
			res["error"] = "find string not present in " + p
			break
		}
		n := strings.Count(src, act.Find)
		src = strings.ReplaceAll(src, act.Find, act.Replace)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		a.commit("edit", act.Path, p, fmt.Sprintf("replaced %d occurrence(s)", n))
		res["ok"] = true
		res["replacements"] = n

	case "read_file":
		p, ok := a.resolvePath(act.Path, &res)
		if !ok {
			break
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
		} else {
			res["ok"] = true
			res["content"] = string(raw)
		}

	case "list_dir":
		dir, ok := a.resolvePath(act.Path, &res)
		if !ok {
			break
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		items := []map[string]any{}
		for _, e := range ents {
			item := map[string]any{"name": e.Name(), "dir": e.IsDir()}
			if !e.IsDir() {
				if info, err := e.Info(); err == nil {
					item["bytes"] = info.Size()
				}
			}
			items = append(items, item)
		}
		res["ok"], res["path"], res["entries"] = true, dir, items

	case "tree":
		root, ok := a.resolvePath(act.Path, &res)
		if !ok {
			break
		}
		depth := act.Depth
		if depth <= 0 || depth > 5 {
			depth = 2
		}
		var files []string
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= depth {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// skip heavy/vendored dirs (never skip the walk root itself)
			name := d.Name()
			if p != root && d.IsDir() && (name == "node_modules" || name == ".git" || name == "vendor" || name == "dist" || name == ".evolv") {
				return filepath.SkipDir
			}
			if rel != "." {
				files = append(files, rel)
			}
			return nil
		})
		sort.Strings(files)
		res["ok"], res["path"], res["files"] = true, root, files

	case "map_repo":
		root := strings.TrimSpace(act.Path)
		if root == "" {
			root = a.ws.Config.ProjectDir
		}
		if root == "" {
			root = a.ws.Root
		}
		abs, err := filepath.Abs(root)
		if err != nil || !dirExists(abs) {
			res["ok"], res["error"] = false, "directory does not exist: "+root
			break
		}
		mapped, err := buildRepoMap(abs, act.Depth)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		cache, err := writeRepoMapCache(a, mapped)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		res["ok"] = true
		res["root"] = mapped.Root
		res["generated"] = mapped.Generated
		res["files"] = mapped.Files
		res["symbols"] = mapped.Symbols
		res["truncated"] = mapped.Truncated
		res["cache"] = cache

	case "delete_file":
		p, ok := a.resolvePath(act.Path, &res)
		if !ok {
			break
		}
		if err := os.RemoveAll(p); err != nil {
			res["ok"], res["error"] = false, err.Error()
		} else {
			a.commit("delete", act.Path, p, "deleted")
			res["ok"] = true
		}

	case "move":
		from, ok := a.resolvePath(act.Path, &res)
		if !ok {
			break
		}
		to, ok := a.resolvePath(act.To, &res)
		if !ok {
			break
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		if err := os.Rename(from, to); err != nil {
			res["ok"], res["error"] = false, err.Error()
		} else {
			a.commit("move", act.Path, to, "moved")
			res["ok"] = true
		}

	case "snapshot":
		id, err := a.ws.Snapshot()
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		a.mu.Lock()
		a.lastSnapshot = id
		a.mu.Unlock()
		res["ok"], res["snapshot"] = true, id
		res["note"] = "genome frozen; restore anytime with rollback"

	case "rollback":
		id := strings.TrimSpace(act.Path)
		if id == "" || id == "latest" {
			id = a.LastSnapshot()
		}
		if id == "" {
			res["ok"], res["error"] = false, "no snapshots exist yet"
			break
		}
		if err := a.ws.Restore(id); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		a.js.LoadAll() // rebuilt tools/plugins from the restored genome
		a.commit("rollback", id, a.ws.Dir(workspace.SnapshotsDir), "genome rewound in time")
		a.bus.Publish("shell:reload", map[string]string{"reason": "genome rolled back to " + id})
		res["ok"], res["snapshot"] = true, id
		res["note"] = "genome restored from " + id + "; shell recompiling"

	case "set_project":
		p := strings.TrimSpace(act.Path)
		if p == "" {
			res["ok"], res["error"] = false, "set_project needs path"
			break
		}
		abs, err := filepath.Abs(p)
		if err != nil || !dirExists(abs) {
			res["ok"], res["error"] = false, "directory does not exist: "+p
			break
		}
		a.ws.Config.ProjectDir = abs
		a.ws.SaveConfig()
		a.commit("project", abs, abs, "project root bound")
		res["ok"] = true
		res["path"] = abs
		res["note"] = "project bound; takes effect next turn"

	case "plan_advance":
		// one-call progress: mark a step done (by index, default = the
		// doing step), then pull the next todo into doing — the agent's
		// loop through its own plan without re-sending the whole thing
		plan := a.ws.ReadPlan()
		if plan == nil {
			res["ok"], res["error"] = false, "no plan — declare one with set_plan first"
			break
		}
		changed := false
		idx := -1
		if n, ok := act.Args["index"].(float64); ok && n >= 0 && int(n) < len(plan.Steps) {
			idx = int(n)
		} else {
			for i, s := range plan.Steps {
				if s.Status == "doing" {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			res["ok"], res["error"] = false, "nothing is doing and no index given"
			break
		}
		if plan.Steps[idx].Status != "done" {
			plan.Steps[idx].Status = "done"
			changed = true
		}
		// start the next todo, if the doing slot is free
		if !hasDoing(plan.Steps) {
			for i, s := range plan.Steps {
				if s.Status == "todo" {
					plan.Steps[i].Status = "doing"
					changed = true
					break
				}
			}
		}
		if !changed {
			res["ok"] = true
			res["note"] = "plan already fully advanced"
			break
		}
		if err := a.ws.WritePlan(*plan); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		publishPlan(a, plan)
		res["ok"] = true
		res["step"] = plan.Steps[idx].Title

	case "reload_shell":
		a.bus.Publish("shell:reload", map[string]string{"reason": "agent requested a shell recompile"})
		res["ok"] = true
		res["note"] = "browser is recompiling its shell now"

	case "set_plan":
		// persistent task state: the agent declares/updates its plan and
		// the UI renders it live; plan.json survives restarts
		if len(act.Steps) == 0 {
			res["ok"], res["error"] = false, "set_plan needs steps:[{title,status}]"
			break
		}
		plan := workspace.Plan{Title: strings.TrimSpace(act.Title)}
		for _, s := range act.Steps {
			t := strings.TrimSpace(s.Title)
			if t == "" {
				continue
			}
			st := strings.ToLower(strings.TrimSpace(s.Status))
			switch st {
			case "doing", "done":
			default:
				st = "todo"
			}
			plan.Steps = append(plan.Steps, workspace.PlanStep{Title: t, Status: st})
		}
		if len(plan.Steps) == 0 {
			res["ok"], res["error"] = false, "set_plan steps need titles"
			break
		}
		if err := a.ws.WritePlan(plan); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		publishPlan(a, &plan)
		res["ok"] = true
		res["note"] = fmt.Sprintf("plan updated: %d/%d steps done", countDone(plan.Steps), len(plan.Steps))

	case "self_update":
		ctx, cancel := contextWithTimeout(5 * time.Minute)
		defer cancel()
		info, err := update.Latest(ctx, workspace.Version)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		if info == nil {
			res["ok"] = true
			res["note"] = "already the latest organism (v" + workspace.Version + ")"
			break
		}
		ver, err := update.SelfUpdate(ctx)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		a.commit("self_update", "v"+ver, workspace.LauncherPath(), "morphed to a newer body")
		res["ok"] = true
		res["version"] = ver
		res["note"] = "binary swapped; the user restarts evolv to wake the new body"

	case "self_modify", "evolve_kernel":
		opt := selfModifyOptions{
			Message:        act.Message,
			StageOnly:      act.StageOnly,
			SourceRoot:     act.SourceRoot,
			TargetLauncher: act.TargetLauncher,
			TestPkg:        act.TestPkg,
			Files:          act.Files,
		}
		if act.RunTests != nil {
			opt.RunTests = *act.RunTests
		} else {
			opt.RunTests = true
		}
		if opt.Message == "" && act.Args != nil {
			if m, ok := act.Args["message"].(string); ok {
				opt.Message = m
			}
		}
		if act.Args != nil {
			if st, ok := act.Args["stage_only"].(bool); ok {
				opt.StageOnly = st
			}
			if rt, ok := act.Args["run_tests"].(bool); ok {
				opt.RunTests = rt
			}
			if tp, ok := act.Args["test_pkg"].(string); ok {
				opt.TestPkg = tp
			}
			if sr, ok := act.Args["source_root"].(string); ok {
				opt.SourceRoot = sr
			}
			if tl, ok := act.Args["target_launcher"].(string); ok {
				opt.TargetLauncher = tl
			}
			if rawFiles, ok := act.Args["files"].([]any); ok {
				for _, rf := range rawFiles {
					if fm, ok := rf.(map[string]any); ok {
						p, _ := fm["path"].(string)
						c, _ := fm["content"].(string)
						if p != "" {
							opt.Files = append(opt.Files, fileMutation{Path: p, Content: c})
						}
					}
				}
			}
		}
		result := a.SelfModify(opt)
		res["ok"] = result.OK
		if !result.OK {
			res["stage"] = result.Stage
			res["error"] = result.Error
		}
		if result.Tag != "" {
			res["tag"] = result.Tag
		}
		if result.Candidate != "" {
			res["candidate"] = result.Candidate
		}
		if result.SourceRoot != "" {
			res["source_root"] = result.SourceRoot
		}
		res["compiled"] = result.Compiled
		res["tested"] = result.Tested
		res["swapped"] = result.Swapped
		if result.Note != "" {
			res["note"] = result.Note
		}

	case "list_files":
		var files []string
		root := a.ws.Dir(workspace.WorkspaceDir)
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			files = append(files, rel)
			return nil
		})
		sort.Strings(files)
		res["ok"] = true
		res["files"] = files

	case "shell":
		if strings.TrimSpace(act.Command) == "" {
			res["ok"] = false
			res["error"] = "shell needs command"
			break
		}
		cwd := a.ws.Root
		if p := strings.TrimSpace(act.Cwd); p != "" {
			if abs, err := filepath.Abs(p); err == nil && dirExists(abs) {
				cwd = abs
			}
		} else if a.ws.Config.ProjectDir != "" {
			cwd = a.ws.Config.ProjectDir
		}

		confirmed, _ := act.Args["confirm"].(bool)
		if !confirmed {
			if f, _ := act.Args["force"].(bool); f {
				confirmed = true
			}
		}

		if decision := CheckShellCommand(act.Command, cwd, confirmed); decision.Blocked {
			res["ok"] = false
			res["error"] = decision.Reason
			res["requires_confirmation"] = decision.RequiresConfirm
			res["cwd"] = cwd
			break
		}
		ctx, cancel := contextWithTimeout(120 * time.Second)
		defer cancel()
		bin, args := shellCommand(act.Command)
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = cwd
		out, err := cmd.CombinedOutput()
		outStr := string(out)
		if len(outStr) > 8000 {
			outStr = outStr[:8000] + "\n…(truncated)"
		}
		res["ok"] = err == nil
		res["cwd"] = cwd
		res["output"] = redact.Redact(outStr)
		if err != nil {
			res["error"] = err.Error()
		}

	default:
		res["ok"] = false
		res["error"] = "unknown action: " + act.Action
	}

	raw, _ := json.Marshal(res)
	return "kernel:" + string(raw)
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// commit records a mutation: bus event + evolution ledger.
// Mutations made during an idle dream are tagged dream:true, so the UI can
// group them ("it dreamt N times while you were away") and the ledger tells
// prompted work from autonomous work.
func (a *Agent) commit(kind, name, path, desc string) {
	a.evolution++
	desc = redact.Redact(desc)
	name = redact.Redact(name)
	entry := map[string]any{
		"n": a.evolution, "kind": kind, "name": name, "path": path, "description": desc,
	}
	if a.Dreaming() {
		entry["dream"] = true
	}
	a.ws.AppendEvolution(entry)
	a.bus.Publish("evolution", entry)
}

func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// dirExists reports whether p is an existing directory.
func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// resolvePath turns an action path into an absolute path. Empty paths resolve
// to the organism home. Relative paths are rooted at the organism home;
// absolute paths are honored as-is — EVOLV is a full agent workspace with
// access to the whole machine. Returns ok=false and fills res with an error
// when the path is empty.
func (a *Agent) resolvePath(p string, res *map[string]any) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return a.ws.Root, true
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(a.ws.Root, p))
	}
	(*res)["path"] = abs
	return abs, true
}

// ---------- plan helpers ----------

// publishPlan emits the canonical plan bus event after any plan mutation.
func publishPlan(a *Agent, p *workspace.Plan) {
	a.bus.Publish("plan", map[string]any{
		"title": p.Title, "steps": p.Steps,
		"done": countDone(p.Steps), "total": len(p.Steps),
	})
}

func countDone(steps []workspace.PlanStep) int {
	n := 0
	for _, s := range steps {
		if s.Status == "done" {
			n++
		}
	}
	return n
}

func hasDoing(steps []workspace.PlanStep) bool {
	for _, s := range steps {
		if s.Status == "doing" {
			return true
		}
	}
	return false
}

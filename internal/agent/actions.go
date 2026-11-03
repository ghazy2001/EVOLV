package agent

import (
	"context"
	"crypto/sha256"
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

	"github.com/dxn111/EVOLV/internal/candidate"
	"github.com/dxn111/EVOLV/internal/genome"
	"github.com/dxn111/EVOLV/internal/lint"
	"github.com/dxn111/EVOLV/internal/redact"
	"github.com/dxn111/EVOLV/internal/search"
	"github.com/dxn111/EVOLV/internal/update"
	"github.com/dxn111/EVOLV/internal/workspace"
)

func contentHash(raw []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(raw)) }

func sameSchema(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// guardedWrite prevents a mutation planned from stale source from overwriting a
// newer file. Existing files require the hash returned by read_file; identical
// content is treated as a no-op so the organism does not repeatedly mutate the
// same tool or component.
func guardedWrite(path string, next []byte, expected string) (string, bool, error) {
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	if err == nil {
		actual := contentHash(current)
		// identical content is a no-op: nothing to protect, and the organism
		// may re-evolve the same code to bump its typed manifest version.
		if string(current) == string(next) {
			return actual, false, nil
		}
		if expected == "" {
			return actual, false, fmt.Errorf("stale-write guard: read %s first and pass expected_hash=%s", path, actual)
		}
		if expected != actual {
			return actual, false, fmt.Errorf("stale-write guard: expected %s, current is %s; re-read before mutating", expected, actual)
		}
	}
	if err := os.WriteFile(path, next, 0o644); err != nil {
		return "", false, err
	}
	return contentHash(next), true, nil
}

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

// LoadAll reloads every tool and plugin from the genome. It is a full rebuild,
// not a merge: without retiring the old sandboxes, every rollback/reload would
// leave another live copy of each plugin running timers and handlers forever.
func (r *JSRuntime) LoadAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.vms {
		p.Stop()
	}
	r.vms = nil
	r.tools = map[string]toolDef{}
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
		_ = r.evalToolLocked(strings.TrimSuffix(n, ".js"), string(raw))
	}
}

// evalToolLocked compiles one tool file: `function name(args){...}` exported
// either as a bare function declaration or via `export default`. It returns an
// error instead of silently reporting success, so evolve_tool can tell the
// model that its JavaScript did not actually compile.
func (r *JSRuntime) evalToolLocked(name, code string) error {
	ident := sanitizeIdent(name)
	src := "(function(){" + code + "; return typeof " + ident + "==='function'? " + ident + " : null})()"
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
		return fmt.Errorf("tool %q failed to compile: %w", name, err)
	}
	if goja.IsUndefined(val) || goja.IsNull(val) {
		return fmt.Errorf("tool %q does not export a function named %s(args)", name, ident)
	}
	fn, ok := goja.AssertFunction(val)
	if !ok {
		return fmt.Errorf("tool %q exported %T, not a function", name, val.Export())
	}
	r.tools[name] = toolDef{Name: name, fn: fn}
	return nil
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
		pluginDir := filepath.Join(r.a.ws.Dir(workspace.PluginsDir), n)
		manifestInfo, err := os.Stat(filepath.Join(pluginDir, "plugin.json"))
		if err != nil || manifestInfo.IsDir() {
			continue
		}
		codePath := filepath.Join(pluginDir, "plugin.js")
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
		// Security linting for evolved components
		lintRep := lint.LintComponent(name, act.HTML)
		if !lintRep.Valid && !act.Force {
			res["ok"] = false
			res["error"] = fmt.Sprintf("security lint failed: %s; components must remain self-contained (pass force:true to override)", lintRep.Summary())
			res["security"] = lintRep
			if a.bus != nil {
				a.bus.Publish("security:lint", map[string]any{
					"target":      name,
					"target_type": "component",
					"status":      "blocked",
					"report":      lintRep,
				})
			}
			break
		}
		path := filepath.Join(a.ws.Dir(workspace.ComponentsDir), name+".html")
		hash, changed, err := guardedWrite(path, []byte(act.HTML), act.ExpectedHash)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		if changed {
			a.commit("component", name, path, act.Desc)
		}
		asset, _ := a.recordOrgan(genome.KindComponent, name, path, []byte(act.HTML), genome.Provenance{Trigger: "evolve_component", Notes: act.Desc})
		res["ok"] = true
		res["hash"], res["changed"] = hash, changed
		res["path"] = "workspace/ui/components/" + name + ".html"
		res["organ"] = asset.ID
		res["version"] = asset.Version
		res["note"] = "component hot-mounted live"
		if len(lintRep.Findings) > 0 {
			res["security"] = lintRep
		}

	case "lint_component", "lint_code":
		targetType := act.TargetType
		if targetType == "" {
			targetType = "component"
		}
		name := sanitizeName(act.Name)
		content := act.Content
		if content == "" {
			content = act.HTML
		}
		if content == "" {
			content = act.Code
		}
		if content == "" {
			res["ok"], res["error"] = false, "lint requires content, html, or code"
			break
		}
		rep := lint.Lint(targetType, name, content)
		res["ok"] = true
		res["valid"] = rep.Valid
		res["summary"] = rep.Summary()
		res["report"] = rep

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
		// Security linting for evolved tools
		lintRep := lint.LintTool(name, act.Code)
		if !lintRep.Valid && !act.Force {
			res["ok"] = false
			res["error"] = fmt.Sprintf("security lint failed: %s; tools must remain safe and local (pass force:true to override)", lintRep.Summary())
			res["security"] = lintRep
			if a.bus != nil {
				a.bus.Publish("security:lint", map[string]any{
					"target":      name,
					"target_type": "tool",
					"status":      "blocked",
					"report":      lintRep,
				})
			}
			break
		}
		path := filepath.Join(a.ws.Dir(workspace.ToolsDir), name+".js")
		hash, changed, err := guardedWrite(path, []byte(act.Code), act.ExpectedHash)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		a.js.mu.Lock()
		evalErr := a.js.evalToolLocked(name, act.Code)
		if evalErr != nil {
			// keep the runtime honest: a broken file must not leave an older
			// in-memory implementation callable until the next restart
			delete(a.js.tools, name)
		}
		a.js.mu.Unlock()
		if evalErr != nil {
			res["ok"], res["error"] = false, evalErr.Error()
			res["hash"], res["changed"] = hash, changed
			break
		}
		if changed {
			a.commit("tool", name, path, act.Desc)
		}
		asset, _ := a.recordOrgan(genome.KindTool, name, path, []byte(act.Code), genome.Provenance{Trigger: "evolve_tool", Notes: act.Desc})
		res["ok"] = true
		res["hash"], res["changed"] = hash, changed
		res["organ"] = asset.ID
		res["version"] = asset.Version
		note := "tool compiled into the sandbox and callable via run_tool"
		currentManifest := readToolManifest(a, name)
		manifestChanged := len(act.Schema) > 0 && (currentManifest == nil || currentManifest.Description != act.Desc || !sameSchema(currentManifest.Schema, act.Schema))
		if manifestChanged {
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
		if len(lintRep.Findings) > 0 {
			res["security"] = lintRep
		}

	case "run_tool":
		if !a.AllowsTool(sanitizeName(act.Name)) {
			res["ok"], res["error"] = false, "tool is not encoded in this DNA strand"
			break
		}
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
		toolName := sanitizeName(act.Name)
		out, err := a.js.RunTool(act.Name, act.Args)
		// every execution is a data point: health decides which organs to trust
		a.noteOrganUse(genome.KindTool, toolName, err == nil, errText(err))
		if err == nil {
			a.noteUsedTool(toolName)
		}
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
		hash, changed, err := guardedWrite(path, []byte(act.Content), act.ExpectedHash)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		if changed {
			a.commit("skill", name, path, act.Desc)
		}
		asset, _ := a.recordOrgan(genome.KindSkill, name, path, []byte(act.Content), genome.Provenance{Trigger: "evolve_skill", Notes: act.Desc})
		res["ok"] = true
		res["hash"], res["changed"] = hash, changed
		res["organ"] = asset.ID
		res["version"] = asset.Version

	case "evolve_prompt":
		name := sanitizeName(act.Name)
		if name == "" || act.Content == "" {
			res["ok"] = false
			res["error"] = "evolve_prompt needs name + content"
			break
		}
		path := filepath.Join(a.ws.Dir(workspace.PromptsDir), name+".md")
		hash, changed, err := guardedWrite(path, []byte(act.Content), act.ExpectedHash)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		if changed {
			a.commit("prompt", name, path, act.Desc)
		}
		asset, _ := a.recordOrgan(genome.KindPrompt, name, path, []byte(act.Content), genome.Provenance{Trigger: "evolve_prompt", Notes: act.Desc})
		res["ok"] = true
		res["hash"], res["changed"] = hash, changed
		res["organ"] = asset.ID
		res["version"] = asset.Version
		res["note"] = "system prompt rewritten; takes effect next turn"

	case "evolve_plugin":
		name := sanitizeName(act.Name)
		if name == "" || act.Code == "" {
			res["ok"] = false
			res["error"] = "evolve_plugin needs name + code"
			break
		}
		// Security linting for evolved plugins
		lintRep := lint.LintPlugin(name, act.Code)
		if !lintRep.Valid && !act.Force {
			res["ok"] = false
			res["error"] = fmt.Sprintf("security lint failed: %s; plugins must remain safe and local (pass force:true to override)", lintRep.Summary())
			res["security"] = lintRep
			if a.bus != nil {
				a.bus.Publish("security:lint", map[string]any{
					"target":      name,
					"target_type": "plugin",
					"status":      "blocked",
					"report":      lintRep,
				})
			}
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
		// hot-load into its own sandbox; the plugin is ALIVE the moment this
		// returns. Re-evolving a plugin retires every older sandbox with the
		// same name first, so stale timers cannot keep firing.
		a.js.mu.Lock()
		p, err := a.js.buildPluginVM(name, act.Code, act.Perms)
		if err == nil {
			kept := a.js.vms[:0]
			replaced := 0
			for _, old := range a.js.vms {
				if old.name == name {
					old.Stop()
					replaced++
					continue
				}
				kept = append(kept, old)
			}
			a.js.vms = append(kept, p)
			res["replaced"] = replaced
		}
		a.js.mu.Unlock()
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			a.commit("plugin", name, filepath.Join(dir, "plugin.js"), act.Desc+" (loaded with error)")
			break
		}
		go p.loop()
		a.commit("plugin", name, filepath.Join(dir, "plugin.js"), act.Desc)
		asset, _ := a.recordOrgan(genome.KindPlugin, name, filepath.Join(dir, "plugin.js"), []byte(act.Code), genome.Provenance{Trigger: "evolve_plugin", Notes: act.Desc})
		res["ok"] = true
		res["organ"] = asset.ID
		res["version"] = asset.Version
		res["note"] = "plugin installed in its own sandbox; evolv.on/every are live now"
		if len(lintRep.Findings) > 0 {
			res["security"] = lintRep
		}

	case "write_file":
		p, ok := a.resolvePath(act.Path, &res)
		if !ok {
			break
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		hash, changed, err := guardedWrite(p, []byte(act.Content), act.ExpectedHash)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
		} else {
			if changed {
				a.commit("file", act.Path, p, act.Desc)
			}
			res["ok"] = true
			res["path"] = p
			res["hash"] = hash
			res["changed"] = changed
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
		actualHash := contentHash(raw)
		if act.ExpectedHash == "" || act.ExpectedHash != actualHash {
			res["ok"] = false
			res["error"] = fmt.Sprintf("stale-write guard: expected_hash must match current %s; re-read before editing", actualHash)
			res["hash"] = actualHash
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
		hash, changed, err := guardedWrite(p, []byte(src), act.ExpectedHash)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		if changed {
			a.commit("edit", act.Path, p, fmt.Sprintf("replaced %d occurrence(s)", n))
		}
		res["ok"] = true
		res["replacements"] = n
		res["hash"] = hash
		res["changed"] = changed

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
			res["hash"] = contentHash(raw)
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
		if mapped.Profile != nil {
			res["kind"] = string(mapped.Profile.Kind)
			res["languages"] = mapped.Profile.Languages
			res["frameworks"] = mapped.Profile.Frameworks
			res["manifests"] = mapped.Profile.Manifests
			res["entry_points"] = mapped.Profile.EntryPoints
			res["test_roots"] = mapped.Profile.Tests
			res["commands"] = mapped.Profile.Commands
			res["notes"] = mapped.Profile.Notes
		}

	case "search_code", "grep":
		query := strings.TrimSpace(firstNonEmptyText(act.Query, act.Find))
		if raw, ok := act.Args["query"].(string); ok && query == "" {
			query = strings.TrimSpace(raw)
		}
		if raw, ok := act.Args["q"].(string); ok && query == "" {
			query = strings.TrimSpace(raw)
		}
		if query == "" {
			res["ok"] = false
			res["error"] = "search_code requires a non-empty query"
			break
		}

		root := strings.TrimSpace(act.Path)
		if root == "" {
			root = strings.TrimSpace(act.Cwd)
		}
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

		opts := search.Options{
			Query:    query,
			Regex:    act.Regex,
			CaseSens: act.Case,
			Exts:     act.Exts,
			Limit:    act.Limit,
			Context:  2,
		}
		if raw, ok := act.Args["regex"].(bool); ok {
			opts.Regex = raw
		}
		if raw, ok := act.Args["case"].(bool); ok {
			opts.CaseSens = raw
		}
		if raw, ok := act.Args["case_sensitive"].(bool); ok {
			opts.CaseSens = raw
		}
		if raw, ok := act.Args["path_glob"].(string); ok && raw != "" {
			opts.PathGlob = strings.TrimSpace(raw)
		} else if raw, ok := act.Args["glob"].(string); ok && raw != "" {
			opts.PathGlob = strings.TrimSpace(raw)
		}
		if raw, ok := act.Args["exts"].([]any); ok {
			for _, item := range raw {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					opts.Exts = append(opts.Exts, strings.TrimSpace(s))
				}
			}
		} else if raw, ok := act.Args["ext"].(string); ok && raw != "" {
			for _, p := range strings.Split(raw, ",") {
				if p = strings.TrimSpace(p); p != "" {
					opts.Exts = append(opts.Exts, p)
				}
			}
		}
		if raw, ok := act.Args["limit"].(float64); ok && raw > 0 {
			opts.Limit = int(raw)
		}
		if raw, ok := act.Args["context"].(float64); ok && raw >= 0 {
			opts.Context = int(raw)
		}

		searchRes := search.Scan(abs, opts)
		if searchRes.Error != "" {
			res["ok"] = false
			res["error"] = searchRes.Error
			break
		}

		res["ok"] = true
		res["query"] = searchRes.Query
		res["root"] = abs
		res["hits"] = searchRes.Hits
		res["count"] = len(searchRes.Hits)
		res["files"] = searchRes.Files
		res["truncated"] = searchRes.Truncated

	case "run_tests":
		rawTarget := strings.TrimSpace(act.Path)
		if rawTarget == "" {
			rawTarget = strings.TrimSpace(act.Cwd)
		}
		base := a.ws.Config.ProjectDir
		if base == "" {
			base = a.ws.Root
		}
		target := rawTarget
		if target == "" {
			target = base
		} else if !filepath.IsAbs(target) {
			candidate := filepath.Join(base, target)
			if !dirExists(candidate) && base != a.ws.Root {
				if alt := filepath.Join(a.ws.Root, target); dirExists(alt) {
					candidate = alt
				}
			}
			target = candidate
		}
		abs, err := filepath.Abs(target)
		if err != nil || !dirExists(abs) {
			errTarget := rawTarget
			if errTarget == "" {
				errTarget = target
			}
			res["ok"], res["error"] = false, "directory does not exist: "+errTarget
			break
		}

		fw := strings.ToLower(strings.TrimSpace(act.Framework))
		if fw == "" {
			detected, err := detectTestFramework(abs)
			if err != nil {
				res["ok"], res["error"] = false, err.Error()
				break
			}
			fw = detected
		}

		ctx, cancel := contextWithTimeout(180 * time.Second)
		defer cancel()
		tRes, err := runTestsSuite(ctx, fw, abs)
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		res["ok"] = tRes.Failed == 0
		res["framework"] = tRes.Framework
		res["target"] = tRes.Target
		res["passed"] = tRes.Passed
		res["failed"] = tRes.Failed
		res["skipped"] = tRes.Skipped
		res["total"] = tRes.Total
		res["duration_ms"] = tRes.DurationMS
		res["summary"] = tRes.Summary
		if len(tRes.Failures) > 0 {
			res["failures"] = tRes.Failures
		}
		if tRes.Failed > 0 {
			res["error"] = tRes.Summary
			if tRes.Raw != "" {
				res["raw"] = tRes.Raw
			}
		}

	case "delete_file":
		if strings.TrimSpace(act.Path) == "" {
			res["ok"], res["error"] = false, "delete_file needs an explicit path (refusing to delete the organism root)"
			break
		}
		p, ok := a.resolvePath(act.Path, &res)
		if !ok {
			break
		}
		if reason := a.protectedPathReason(p); reason != "" {
			res["ok"], res["error"] = false, reason
			break
		}
		if err := os.RemoveAll(p); err != nil {
			res["ok"], res["error"] = false, err.Error()
		} else {
			a.commit("delete", act.Path, p, "deleted")
			res["ok"] = true
		}

	case "move":
		fromRaw := strings.TrimSpace(act.From)
		if fromRaw == "" {
			fromRaw = strings.TrimSpace(act.Path) // legacy shape: {path, to}
		}
		if fromRaw == "" || strings.TrimSpace(act.To) == "" {
			res["ok"], res["error"] = false, "move needs explicit from + to paths"
			break
		}
		from, ok := a.resolvePath(fromRaw, &res)
		if !ok {
			break
		}
		if reason := a.protectedPathReason(from); reason != "" {
			res["ok"], res["error"] = false, reason
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
			a.commit("move", fromRaw, to, "moved")
			res["ok"], res["from"], res["to"] = true, from, to
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

	case "complete_task":
		summary := strings.TrimSpace(act.Summary)
		if summary == "" {
			summary = strings.TrimSpace(act.Desc)
		}
		if summary == "" {
			summary = strings.TrimSpace(act.Title)
		}
		if summary == "" {
			res["ok"] = false
			res["error"] = "complete_task needs summary"
			break
		}

		status := strings.ToLower(strings.TrimSpace(act.Status))
		if status == "" {
			status = "success"
		}
		switch status {
		case "success", "partial", "blocked", "failed":
		default:
			status = "success"
		}

		confidence := strings.ToLower(strings.TrimSpace(act.Confidence))
		if confidence == "" {
			confidence = "high"
		}
		switch confidence {
		case "high", "medium", "low":
		default:
			confidence = "high"
		}

		// files accepts either a list of paths ("files":["a.go"]) or the
		// staged-mutation shape ("files":[{"path":"a.go",...}]). Models emit
		// both; the parser above normalizes them into one list.
		files := make([]string, 0, len(act.FilesList)+len(act.Files))
		seenFiles := map[string]bool{}
		addFile := func(p string) {
			p = strings.TrimSpace(p)
			if p != "" && !seenFiles[p] {
				seenFiles[p] = true
				files = append(files, p)
			}
		}
		for _, p := range act.FilesList {
			addFile(p)
		}
		for _, fm := range act.Files {
			addFile(fm.Path)
		}

		// When task succeeded, complete any remaining active steps in plan.json
		if status == "success" {
			if plan := a.ws.ReadPlan(); plan != nil && len(plan.Steps) > 0 {
				mutated := false
				for i := range plan.Steps {
					if plan.Steps[i].Status != "done" {
						plan.Steps[i].Status = "done"
						mutated = true
					}
				}
				if mutated {
					_ = a.ws.WritePlan(*plan)
					publishPlan(a, plan)
				}
			}
		}

		report := map[string]any{
			"summary":      summary,
			"status":       status,
			"files":        files,
			"tests_passed": act.TestsPassed,
			"tests_failed": act.TestsFailed,
			"confidence":   confidence,
			"notes":        strings.TrimSpace(act.Notes),
			"time":         time.Now().UTC().Format(time.RFC3339),
		}

		a.commit("task_completion", sanitizeName(summary), "", summary)
		a.bus.Publish("task:completed", report)

		res["ok"] = true
		res["summary"] = summary
		res["status"] = status
		res["files"] = files
		res["tests_passed"] = act.TestsPassed
		res["tests_failed"] = act.TestsFailed
		res["confidence"] = confidence
		if act.Notes != "" {
			res["notes"] = act.Notes
		}

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
			Files:          []fileMutation(act.Files),
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
		res["ok"] = err == nil
		res["cwd"] = cwd
		res["output"] = formatShellOutput(out)
		if err != nil {
			res["error"] = redact.Redact(err.Error())
		}

	case "stage_candidate":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		files := act.FilesMap
		if files == nil {
			files = map[string]string{}
			for _, fm := range act.Files {
				files[fm.Path] = fm.Content
			}
		}
		man, err := a.candidateManager().Stage(ctx, candidate.Proposal{Name: act.Name, Files: files, ExpectedHashes: act.ExpectedHashes})
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			break
		}
		res["ok"] = man.Status == "ready"
		res["candidate"] = man
		if man.Status != "ready" {
			res["error"] = "candidate validation failed; living source was not touched"
		}
		a.bus.Publish("candidate:updated", man)

	case "list_candidates":
		items := a.candidateManager().List()
		res["ok"], res["candidates"] = true, items

	case "organ_list":
		res = a.organList(act)

	case "organ_history":
		res = a.organHistory(act)

	case "organ_rollback":
		res = a.organRollback(act)

	case "organ_disable":
		res = a.organDisable(act)

	case "run_work":
		res = a.runStart(act)

	case "run_begin":
		res = a.runBegin(act)

	case "run_finish":
		res = a.runFinish(act)

	case "run_fail":
		res = a.runFail(act)

	case "run_status":
		res = a.runStatus(act)

	case "run_cancel":
		res = a.runCancel(act)

	case "run_retry":
		res = a.runRetry(act)

	case "run_worktree":
		res = a.runWorktree(act)

	case "report":
		rep := a.buildCompletionReport(firstNonEmptyText(act.Title, act.Summary, act.Desc))
		res["ok"] = true
		res["report"] = rep
		res["markdown"] = renderCompletionReport(rep)
		res["note"] = "this report contains only checks that really ran; do not restate it as more than it is"

	case "promote_candidate":
		confirmed := act.Confirm
		if raw, ok := act.Args["confirm"].(bool); ok && raw {
			confirmed = true
		}
		if !confirmed {
			res["ok"] = false
			res["error"] = "promote_candidate requires explicit user approval — ask the user, then retry with confirm:true"
			break
		}
		opts := candidate.PromoteOptions{AllowUnverified: act.StageOnly}
		if root := workspace.DetectSourceRoot(); root != "" {
			// Swap the freshly built binary next to the running one, but only
			// after it proves it can boot on an isolated home.
			opts.BinaryDir = filepath.Dir(workspace.LauncherPath())
			opts.HealthGate = opts.BinaryDir != "."
		}
		pctx, cancel := contextWithTimeout(6 * time.Minute)
		pres, err := a.candidateManager().PromoteWithOptions(pctx, act.Name, opts)
		cancel()
		if err != nil {
			res["ok"], res["error"] = false, err.Error()
			if pres != nil && pres.Unverified != nil {
				res["unverified"] = pres.Unverified
			}
			break
		}
		man := pres.Manifest
		res["ok"], res["candidate"] = true, man
		res["binary_swapped"] = pres.Swapped.Replaced
		if pres.Swapped.Backup != "" {
			res["binary_backup"] = pres.Swapped.Backup
		}
		if len(pres.Unverified) > 0 {
			res["warning"] = "promoted despite failed checks: " + strings.Join(pres.Unverified, ", ")
		}
		a.bus.Publish("candidate:updated", man)

	default:
		res["ok"] = false
		res["error"] = "unknown action: " + act.Action
	}

	res = redact.RedactMap(res)
	raw, _ := json.Marshal(res)
	return "kernel:" + string(raw)
}

func formatShellOutput(out []byte) string {
	outStr := redact.Redact(string(out))
	if len(outStr) > 8000 {
		outStr = outStr[:8000] + "\n…(truncated)"
	}
	return outStr
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// commit records a mutation: bus event + evolution ledger.
// Mutations made during an idle dream are tagged dream:true, so the UI can
// group them ("it dreamt N times while you were away") and the ledger tells
// prompted work from autonomous work.
func (a *Agent) commit(kind, name, path, desc string) {
	n := a.evolution.Add(1)
	desc = redact.Redact(desc)
	name = redact.Redact(name)
	path = redact.Redact(path)
	entry := map[string]any{
		"n": n, "kind": kind, "name": name, "path": path, "description": desc,
	}
	if a.Dreaming() {
		entry["dream"] = true
	}
	a.ws.AppendEvolution(entry)
	a.bus.Publish("evolution", entry)
}

var unsafeNameRe = regexp.MustCompile(`[^a-z0-9_-]+`)

func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = unsafeNameRe.ReplaceAllString(s, "-")
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

// protectedPathReason refuses the handful of paths whose deletion or move
// would destroy the organism or the user's account in one call. Everything
// below them remains fair game — this is a guard rail, not a sandbox.
func (a *Agent) protectedPathReason(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "refusing to touch an unresolvable path: " + err.Error()
	}
	target := filepath.Clean(abs)
	candidates := []string{a.ws.Root, workspace.DetectSourceRoot(), workspace.LauncherPath()}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, home)
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if ca, err := filepath.Abs(c); err == nil && filepath.Clean(ca) == target {
			return "refusing to delete or move protected path " + target + " (organism root, source root, launcher, or home)"
		}
	}
	if filepath.Dir(target) == target {
		return "refusing to delete or move a filesystem root: " + target
	}
	return ""
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

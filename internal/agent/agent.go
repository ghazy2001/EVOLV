// Package agent is EVOLV's mind. It runs the chat loop, teaches the model the
// evolv-block protocol (self-modification works on ANY model, even tiny free
// ones), parses those blocks out of the stream, executes them against the
// living filesystem, and pushes every mutation to the bus.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dxn111/EVOLV/internal/bus"
	"github.com/dxn111/EVOLV/internal/providers"
	"github.com/dxn111/EVOLV/internal/workspace"
)

// Agent is the living mind.
type Agent struct {
	ws        *workspace.W
	bus       *bus.Bus
	prov      providers.Provider
	Model     string
	mu        sync.Mutex
	history   []providers.Message
	js        *JSRuntime
	evolution int // mutation counter
	maxLoops  int

	lastAct      time.Time // last real user activity (drives dreaming)
	dreaming     bool
	lastSnapshot string
}

// New builds an Agent.
func New(ws *workspace.W, b *bus.Bus, prov providers.Provider, model string) *Agent {
	a := &Agent{ws: ws, bus: b, prov: prov, Model: model, maxLoops: 10, lastAct: time.Now()}
	a.js = NewJSRuntime(a)
	a.js.LoadAll()
	a.loadMemory() // the mind remembers across restarts
	// feed the nervous system to the plugins: every bus event lands in their
	// sandboxes live (evolv.on), so plugins react while the organism thinks
	sub, _ := b.Subscribe()
	go func() {
		for ev := range sub.Events() {
			a.js.Dispatch(ev)
		}
	}()
	return a
}

// ProviderName returns the active backend name.
func (a *Agent) ProviderName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prov == nil {
		return "unknown"
	}
	return a.prov.Name()
}

// SetProvider hot-swaps the model backend mid-life.
func (a *Agent) SetProvider(p providers.Provider, model string) {
	a.mu.Lock()
	a.prov, a.Model = p, model
	a.mu.Unlock()
	a.bus.Publish("agent:provider", map[string]string{"provider": p.Name(), "model": model})
}

// Reset wipes conversation memory (the genome stays).
func (a *Agent) Reset() {
	a.mu.Lock()
	a.history = nil
	a.mu.Unlock()
	a.saveMemory() // empty history → notebook file removed
	a.bus.Publish("agent:reset", map[string]bool{"ok": true})
}

// EvolutionCount returns how many mutations have happened.
func (a *Agent) EvolutionCount() int { return a.evolution }

// ReloadGrownCode re-reads tools and plugins from the genome (after rollback).
func (a *Agent) ReloadGrownCode() { a.js.LoadAll() }

// PluginNames lists the live plugins.
func (a *Agent) PluginNames() []string { return a.js.PluginNames() }

// HistoryLen returns conversation length.
func (a *Agent) HistoryLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.history)
}

// ---------- the evolv-block protocol ----------

type action struct {
	Action         string            `json:"action"`
	Name           string            `json:"name"`
	Title          string            `json:"title"`
	Icon           string            `json:"icon"`
	Desc           string            `json:"description"`
	HTML           string            `json:"html"`
	Code           string            `json:"code"`
	Content        string            `json:"content"`
	Path           string            `json:"path"`
	To             string            `json:"to"`
	Find           string            `json:"find"`
	Replace        string            `json:"replace"`
	Depth          int               `json:"depth"`
	Cwd            string            `json:"cwd"`
	Command        string            `json:"command"`
	Framework      string            `json:"framework"`
	Args           map[string]any    `json:"args"`
	Steps          []planStepArg     `json:"steps"`
	Schema         map[string]string `json:"schema"`
	Perms          []string          `json:"perms"`
	Files          []fileMutation    `json:"files,omitempty"`
	Message        string            `json:"message,omitempty"`
	RunTests       *bool             `json:"run_tests,omitempty"`
	StageOnly      bool              `json:"stage_only,omitempty"`
	SourceRoot     string            `json:"source_root,omitempty"`
	TargetLauncher string            `json:"target_launcher,omitempty"`
	TestPkg        string            `json:"test_pkg,omitempty"`
}

// planStepArg is one step of a set_plan action as it arrives over the wire.
type planStepArg struct {
	Title  string `json:"title"`
	Status string `json:"status"`
}

// projectLine describes the bound user project for the prompt.
func (a *Agent) projectLine() string {
	p := a.ws.Config.ProjectDir
	if p == "" {
		return "no project bound yet — the user's files are still reachable with absolute paths anywhere on this machine"
	}
	return p + " (the user's current project root)"
}

// SystemPrompt assembles the mind: genome constants + evolvable fragments.
func (a *Agent) SystemPrompt() string {
	launcher := workspace.LauncherPath()
	if launcher == "" {
		launcher = "unknown"
	}
	src := workspace.DetectSourceRoot()
	if src == "" {
		src = "not found — you can still edit every file in your organism home"
	}
	prompt := basePrompt
	prompt = strings.ReplaceAll(prompt, "{{EVOLV_HOME}}", a.ws.Root)
	prompt = strings.ReplaceAll(prompt, "{{LAUNCHER}}", launcher)
	prompt = strings.ReplaceAll(prompt, "{{SOURCE_ROOT}}", src)
	prompt = strings.ReplaceAll(prompt, "{{PROJECT}}", a.projectLine())

	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n## ORGANISM STATE\n")
	b.WriteString("- " + a.ws.SystemInfo() + "\n")
	b.WriteString(fmt.Sprintf("- mutations committed: %d\n", a.evolution))
	if p := a.ws.Config.ProjectDir; p != "" {
		b.WriteString(fmt.Sprintf("- project root: %s\n", p))
	}

	// the plan the agent declared for itself — injected so every turn the
	// brain knows where it is in its own multi-step work, not just the UI
	if plan := a.ws.ReadPlan(); plan != nil {
		b.WriteString("\n\n## CURRENT PLAN — " + plan.Title + "\n")
		b.WriteString("(keep this plan alive: work the doing step, call set_plan / plan_advance to update)\n")
		for i, s := range plan.Steps {
			switch s.Status {
			case "done":
				b.WriteString(fmt.Sprintf("%d. [x] %s\n", i+1, s.Title))
			case "doing":
				b.WriteString(fmt.Sprintf("%d. [~] %s  ← you are here\n", i+1, s.Title))
			default:
				b.WriteString(fmt.Sprintf("%d. [ ] %s\n", i+1, s.Title))
			}
		}
	}

	// evolvable system prompt fragments
	if frags, err := a.listDir(a.ws.Dir(workspace.PromptsDir), ".md"); err == nil {
		for _, f := range frags {
			if raw, err := os.ReadFile(f); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
				b.WriteString("\n### prompt fragment: " + filepath.Base(f) + "\n" + string(raw) + "\n")
			}
		}
	}

	// skills (titles + first lines; full file is readable via read_file)
	if skills, err := a.listDir(a.ws.Dir(workspace.SkillsDir), ".md"); err == nil && len(skills) > 0 {
		b.WriteString("\n## SKILLS (grown into yourself)\n")
		for _, s := range skills {
			raw, _ := os.ReadFile(s)
			first := strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)
			title := strings.TrimPrefix(first[0], "# ")
			b.WriteString("- " + title + " (skills/" + filepath.Base(s) + ")\n")
		}
	}

	// grown tools
	if tools := a.js.ListTools(); len(tools) > 0 {
		b.WriteString("\n## GROWN TOOLS (callable via run_tool)\n")
		for _, t := range tools {
			b.WriteString("- " + t.Name + ": " + t.Desc + "\n")
		}
	}

	// mounted components
	if comps := a.ListComponents(); len(comps) > 0 {
		b.WriteString("\n## MOUNTED UI COMPONENTS\n")
		for _, c := range comps {
			b.WriteString("- " + c + "\n")
		}
	}

	// live plugins
	if plugs := a.js.PluginNames(); len(plugs) > 0 {
		b.WriteString("\n## LIVE PLUGINS (own sandbox, evolv.on/every/reacting right now)\n")
		for _, p := range plugs {
			b.WriteString("- " + p + "\n")
		}
	}
	return b.String()
}

func (a *Agent) listDir(dir, ext string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ext) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Chat processes one user turn: stream, parse evolv blocks, execute, feed
// results back, repeat. Returns the final visible text.
func (a *Agent) Chat(ctx context.Context, userMsg string) string {
	a.mu.Lock()
	isDream := strings.HasPrefix(userMsg, "You are DREAMING")
	if !isDream {
		a.lastAct = time.Now()
	}
	a.history = append(a.history, providers.Message{Role: "user", Content: userMsg})
	hist := append([]providers.Message{}, a.history...)
	a.mu.Unlock()

	final := ""
	for loop := 0; loop < a.maxLoops; loop++ {
		msgs := append([]providers.Message{{Role: "system", Content: a.SystemPrompt()}}, hist...)

		a.bus.Publish("agent:phase", map[string]string{"phase": "thinking"})
		stream := func(d providers.Delta) {
			if d.Err != nil {
				return
			}
			if d.Content != "" {
				a.bus.Publish("agent:token", map[string]string{"text": d.Content})
			}
			if d.Reasoning != "" {
				a.bus.Publish("agent:reasoning", map[string]string{"text": d.Reasoning})
			}
		}
		text, err := a.prov.Stream(ctx, a.Model, msgs, stream)
		if err != nil && ctx.Err() == nil {
			// free anonymous gateways hiccup constantly; one quiet
			// retry before surfacing the error to the user
			a.bus.Publish("agent:token", map[string]string{"text": "…\n\n"})
			time.Sleep(1500 * time.Millisecond)
			text, err = a.prov.Stream(ctx, a.Model, msgs, stream)
		}
		if err != nil {
			errMsg := fmt.Sprintf("\n\n⚠️ **model error**: `%s`\n\nTry another model in Settings — free anonymous models rotate often.", err.Error())
			a.bus.Publish("agent:token", map[string]string{"text": errMsg})
			a.mu.Lock()
			a.history = append(a.history, providers.Message{Role: "assistant", Content: errMsg})
			a.mu.Unlock()
			a.saveMemory()
			return errMsg
		}

		hist = append(hist, providers.Message{Role: "assistant", Content: text})
		final = text

		muts := extractMutations(text)
		if len(muts) == 0 {
			break // no mutations requested; conversation turn complete
		}

		// time travel: freeze the genome before this mutation batch lands
		if id, err := a.ws.Snapshot(); err == nil {
			a.mu.Lock()
			a.lastSnapshot = id
			a.mu.Unlock()
		}

		// execute all requested mutations, then feed results back
		var results []string
		for _, line := range muts {
			var act action
			if err := json.Unmarshal([]byte(line), &act); err != nil {
				results = append(results, "parse_error: "+err.Error())
				continue
			}
			// live tool card update in the UI: running → ok/error
			a.bus.Publish("tool:call", map[string]any{
				"action": act.Action,
				"target": act.Name, "path": act.Path,
				"state": "running",
			})
			res := a.Execute(act)
			a.bus.Publish("tool:call", map[string]any{
				"action": act.Action,
				"target": act.Name, "path": act.Path,
				"state": resStatus(res),
			})
			results = append(results, res)
		}
		hist = append(hist, providers.Message{
			Role:    "user",
			Content: "EVOLV KERNEL — tool results:\n" + strings.Join(results, "\n") + "\nContinue the task with the next batch of mutations until it is fully done, then summarize what changed. Chain as many batches as the task needs — never stop after one.",
		})
		a.bus.Publish("agent:phase", map[string]string{"phase": "evolving"})
		// visually separate the next stream round in the chat bubble
		a.bus.Publish("agent:token", map[string]string{"text": "\n\n"})
	}

	a.mu.Lock()
	a.history = hist
	// keep memory bounded
	if len(a.history) > 80 {
		a.history = a.history[len(a.history)-60:]
	}
	a.mu.Unlock()
	a.saveMemory()
	return final
}

// splitJSONObjects is the old decoder-based splitter; kept for compatibility
// with callers that feed it already-clean block bodies.
func splitJSONObjects(s string) []string { return scanJSONObjects(s) }

// extractMutations pulls every mutation request out of one assistant turn.
// Small free models are sloppy, so this is deliberately forgiving — the model
// is never punished for a broken fence with a dropped mutation:
//  1. fenced ```evolv blocks (the real protocol)
//  2. the LAST fence left unterminated (model streams past the closing ```)
//  3. stray {"action":...} JSON leaked into visible prose (model forgot the
//     fence entirely — these still execute, and the UI hides them from chat)
func extractMutations(text string) []string {
	var out []string
	const fence = "```evolv"
	rest := text
	for {
		i := strings.Index(rest, fence)
		if i < 0 {
			break
		}
		after := rest[i+len(fence):]
		after = strings.TrimPrefix(strings.TrimPrefix(after, "\r\n"), "\n")
		j := strings.Index(after, "```")
		if j < 0 {
			// unterminated fence: everything after it is block body
			out = append(out, scanJSONObjects(after)...)
			rest = ""
			break
		}
		out = append(out, scanJSONObjects(after[:j])...)
		rest = after[j+3:]
	}
	// salvage unfenced action JSON from the prose between/around blocks
	for _, obj := range scanJSONObjects(rest) {
		if strings.Contains(obj, `"action"`) {
			out = append(out, obj)
		}
	}
	return dedupeActions(out)
}

// dedupeActions drops exact-duplicate action payloads. Small models love to
// repeat {"action":"read_file",...} three times in one breath; executing the
// same call twice is wasted work and renders as repeated tool cards.
func dedupeActions(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, a := range in {
		key := strings.Join(strings.Fields(a), " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

// resStatus pulls "ok" out of a kernel:<json> result line.
func resStatus(res string) string {
	raw := strings.TrimPrefix(res, "kernel:")
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) == nil {
		if ok, _ := m["ok"].(bool); ok {
			return "ok"
		}
		return "error"
	}
	return "ok"
}

// scanJSONObjects extracts balanced {...} objects with a string-aware brace
// scanner. Tolerates the garbage tiny models emit: `}{`, trailing commas,
// JSON glued to prose, escaped quotes inside strings.
func scanJSONObjects(s string) []string {
	var out []string
	depth, start, inStr, esc := 0, 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		switch {
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				out = append(out, s[start:i+1])
			} else if depth < 0 {
				depth = 0 // stray closer outside any object; ignore
			}
		}
	}
	return out
}

// ListComponents returns mounted component names.
func (a *Agent) ListComponents() []string {
	ents, err := os.ReadDir(a.ws.Dir(workspace.ComponentsDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".html") {
			out = append(out, strings.TrimSuffix(e.Name(), ".html"))
		}
	}
	sort.Strings(out)
	return out
}

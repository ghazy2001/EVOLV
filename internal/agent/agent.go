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
	"sync/atomic"
	"time"

	"github.com/dxn111/EVOLV/internal/bus"
	"github.com/dxn111/EVOLV/internal/candidate"
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
	evolution atomic.Int64 // mutation counter, safe across dream + chat
	maxLoops  int
	profile   *Profile

	modelCalls         atomic.Int64
	modelErrors        atomic.Int64
	lastModelLatencyMS atomic.Int64
	turns              atomic.Int64
	consulted          map[string]bool // skills/tools consulted this turn
	lessons            string          // advice derived from recorded outcomes
	compactions        atomic.Int64
	turnsFailed        atomic.Int64
	turnsCancelled     atomic.Int64
	turnsLimited       atomic.Int64
	toolCalls          atomic.Int64
	toolFailures       atomic.Int64
	lastTurnMS         atomic.Int64

	lastAct      time.Time // last real user activity (drives dreaming)
	dreaming     bool
	dreamStop    chan struct{}
	dreamWG      sync.WaitGroup
	lastSnapshot string
}

type Profile struct {
	ID           string   `json:"id"`
	Number       int      `json:"number"`
	Name         string   `json:"name"`
	Identity     string   `json:"identity"`
	SystemPrompt string   `json:"system_prompt"`
	Tools        []string `json:"tools"`
	Skills       []string `json:"skills"`
}

// New builds an Agent.
func New(ws *workspace.W, b *bus.Bus, prov providers.Provider, model string) *Agent {
	return newAgent(ws, b, prov, model, nil)
}

func NewProfile(ws *workspace.W, b *bus.Bus, prov providers.Provider, model string, profile Profile) *Agent {
	return newAgent(ws, b, prov, model, &profile)
}

func newAgent(ws *workspace.W, b *bus.Bus, prov providers.Provider, model string, profile *Profile) *Agent {
	a := &Agent{ws: ws, bus: b, prov: prov, Model: model, maxLoops: 10, lastAct: time.Now(), profile: profile}
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

func (a *Agent) CloneProfile(profile Profile) *Agent {
	a.mu.Lock()
	p, model := a.prov, a.Model
	a.mu.Unlock()
	return NewProfile(a.ws, a.bus, p, model, profile)
}
func (a *Agent) ProfileID() string {
	if a.profile != nil {
		return a.profile.ID
	}
	return "main"
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

// ProviderModel returns the active backend and model atomically. Callers that
// display status (health, hello frames) should not read Model directly while a
// settings change can swap it.
func (a *Agent) ProviderModel() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	name := "unknown"
	if a.prov != nil {
		name = a.prov.Name()
	}
	return name, a.Model
}

// ProviderSnapshot returns the active provider and model for a swarm that
// needs a stable handle without reaching into the agent's mutex.
func (a *Agent) ProviderSnapshot() (providers.Provider, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prov, a.Model
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
func (a *Agent) EvolutionCount() int { return int(a.evolution.Load()) }

// AgentStats is a cheap, lock-free snapshot of model activity for health and
// metrics surfaces.
// AgentStats is a cheap, allocation-free snapshot for health and metrics.
type AgentStats struct {
	ModelCalls         int64 `json:"model_calls"`
	ModelErrors        int64 `json:"model_errors"`
	LastModelLatencyMS int64 `json:"last_model_latency_ms"`
	Turns              int64 `json:"turns"`
	TurnsFailed        int64 `json:"turns_failed"`
	TurnsCancelled     int64 `json:"turns_cancelled"`
	TurnsLimited       int64 `json:"turns_limited"`
	ToolCalls          int64 `json:"tool_calls"`
	ToolFailures       int64 `json:"tool_failures"`
	LastTurnMS         int64 `json:"last_turn_ms"`
	Compactions        int64 `json:"context_compactions"`
}

func (a *Agent) Stats() AgentStats {
	return AgentStats{
		ModelCalls:         a.modelCalls.Load(),
		ModelErrors:        a.modelErrors.Load(),
		LastModelLatencyMS: a.lastModelLatencyMS.Load(),
		Turns:              a.turns.Load(),
		TurnsFailed:        a.turnsFailed.Load(),
		TurnsCancelled:     a.turnsCancelled.Load(),
		TurnsLimited:       a.turnsLimited.Load(),
		ToolCalls:          a.toolCalls.Load(),
		ToolFailures:       a.toolFailures.Load(),
		LastTurnMS:         a.lastTurnMS.Load(),
		Compactions:        a.compactions.Load(),
	}
}

// ReloadGrownCode re-reads tools and plugins from the genome (after rollback).
func (a *Agent) ReloadGrownCode() { a.js.LoadAll() }

// PluginNames lists the live plugins.
func (a *Agent) PluginNames() []string { return a.js.PluginNames() }

// ToolCount, PluginCount and ComponentCount are cheap status snapshots for
// health/metrics surfaces.
func (a *Agent) ToolCount() int      { return len(a.js.ListTools()) }
func (a *Agent) PluginCount() int    { return len(a.js.PluginNames()) }
func (a *Agent) ComponentCount() int { return len(a.ListComponents()) }

// HistoryLen returns conversation length.
func (a *Agent) HistoryLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.history)
}

// HistoryPage returns a copy of a bounded transcript slice plus the total
// length. Copies matter: callers serialize while a chat turn may append.
func (a *Agent) HistoryPage(limit, offset int) ([]providers.Message, int) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	total := len(a.history)
	if offset >= total {
		return []providers.Message{}, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]providers.Message, end-offset)
	copy(out, a.history[offset:end])
	return out, total
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
	From           string            `json:"from"`
	To             string            `json:"to"`
	Find           string            `json:"find"`
	Replace        string            `json:"replace"`
	ExpectedHash   string            `json:"expected_hash"`
	Depth          int               `json:"depth"`
	Cwd            string            `json:"cwd"`
	Command        string            `json:"command"`
	Framework      string            `json:"framework"`
	Args           map[string]any    `json:"args"`
	Steps          []planStepArg     `json:"steps"`
	Schema         map[string]string `json:"schema"`
	Perms          []string          `json:"perms"`
	Files          fileMutationList  `json:"files,omitempty"`
	FilesMap       map[string]string `json:"files_map,omitempty"`
	ExpectedHashes map[string]string `json:"expected_hashes,omitempty"`
	Summary        string            `json:"summary,omitempty"`
	Status         string            `json:"status,omitempty"`
	FilesList      []string          `json:"files_list,omitempty"`
	TestsPassed    int               `json:"tests_passed,omitempty"`
	TestsFailed    int               `json:"tests_failed,omitempty"`
	Confidence     string            `json:"confidence,omitempty"`
	Notes          string            `json:"notes,omitempty"`
	Message        string            `json:"message,omitempty"`
	RunTests       *bool             `json:"run_tests,omitempty"`
	StageOnly      bool              `json:"stage_only,omitempty"`
	Confirm        bool              `json:"confirm,omitempty"`
	SourceRoot     string            `json:"source_root,omitempty"`
	Run            string            `json:"run,omitempty"`            // durable run id
	Task           string            `json:"task,omitempty"`           // durable task id
	VerifyCommand  string            `json:"verify_command,omitempty"` // command whose exit status is the evidence
	Reason         string            `json:"reason,omitempty"`         // why a task failed or is blocked
	TargetLauncher string            `json:"target_launcher,omitempty"`
	TestPkg        string            `json:"test_pkg,omitempty"`
	TargetType     string            `json:"target_type,omitempty"`
	Force          bool              `json:"force,omitempty"`
	Strict         bool              `json:"strict,omitempty"`
	Kind           string            `json:"kind,omitempty"`    // organ kind for organ_* actions
	Version        int               `json:"version,omitempty"` // organ version for history/rollback
	Lesson         string            `json:"lesson,omitempty"`  // what was learned, for playbooks
	Isolate        bool              `json:"isolate,omitempty"` // run in isolated git worktree
	Merge          bool              `json:"merge,omitempty"`   // merge worktree into base repository
	CommitMsg      string            `json:"commit_msg,omitempty"`
	Op             string            `json:"op,omitempty"`      // sub-operation (e.g. for run_worktree)
	Outcome        string            `json:"outcome,omitempty"` // verified | unverified | failed
	Query          string            `json:"query,omitempty"`   // query for search_code
	Regex          bool              `json:"regex,omitempty"`   // regex search flag
	Case           bool              `json:"case,omitempty"`    // case-sensitive search flag
	Exts           []string          `json:"exts,omitempty"`    // file extensions to filter
	Limit          int               `json:"limit,omitempty"`   // max search hits or items
}

func (a *Agent) candidateManager() *candidate.Manager {
	return &candidate.Manager{WS: a.ws, SourceRoot: workspace.DetectSourceRoot()}
}

// planStepArg is one step of a set_plan action as it arrives over the wire.
type planStepArg struct {
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Text   string   `json:"text,omitempty"`   // objective / detail for durable runs
	After  []string `json:"after,omitempty"`  // task ids this step waits for
	Files  []string `json:"files,omitempty"`  // files this step expects to touch
	Verify string   `json:"verify,omitempty"` // command that proves the step
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
	prompt = strings.ReplaceAll(prompt, "{{SYSTEM_INFO}}", a.ws.SystemInfo())
	prompt = strings.ReplaceAll(prompt, "{{EVOLV_HOME}}", a.ws.Root)
	prompt = strings.ReplaceAll(prompt, "{{LAUNCHER}}", launcher)
	prompt = strings.ReplaceAll(prompt, "{{SOURCE_ROOT}}", src)
	prompt = strings.ReplaceAll(prompt, "{{PROJECT}}", a.projectLine())

	var b strings.Builder
	b.WriteString(prompt)
	if a.lessons != "" {
		b.WriteString(a.lessons)
	}
	if a.profile != nil {
		b.WriteString("\n\n## CLONED DNA IDENTITY\nYou are " + a.profile.Name + ", a distinct sub-agent strand of EVOLV.\n")
		if a.profile.Identity != "" {
			b.WriteString(a.profile.Identity + "\n")
		}
		if a.profile.SystemPrompt != "" {
			b.WriteString("\n## STRAND DIRECTIVE\n" + a.profile.SystemPrompt + "\n")
		}
	}
	b.WriteString("\n\n## ORGANISM STATE\n")
	b.WriteString("- " + a.ws.SystemInfo() + "\n")
	b.WriteString(fmt.Sprintf("- mutations committed: %d\n", a.EvolutionCount()))
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

	// durable engineering work in flight — keeps long assignments anchored
	b.WriteString(a.engineeringSection())

	// organs that are failing, so the agent repairs instead of piling on more
	b.WriteString(a.organHealthSection())

	// evolvable system prompt fragments
	if frags, err := a.listDir(a.ws.Dir(workspace.PromptsDir), ".md"); err == nil {
		for _, f := range frags {
			if raw, err := os.ReadFile(f); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
				b.WriteString("\n### prompt fragment: " + filepath.Base(f) + "\n" + string(raw) + "\n")
			}
		}
	}

	// skills: a compact, filtered index. The body is deliberately NOT inlined —
	// the agent reads the named file when its trigger matches, which keeps the
	// prompt bounded as the organism learns more playbooks.
	if skills := loadSkillIndex(a.ws.Dir(workspace.SkillsDir)); len(skills) > 0 {
		shown := 0
		var lines []string
		for _, s := range skills {
			if a.profile != nil && !containsName(a.profile.Skills, s.Name) {
				continue
			}
			line := "- " + s.Name
			if s.Title != "" && !strings.EqualFold(s.Title, humanizeName(s.Name)) {
				line += " · " + s.Title
			}
			if s.Summary != "" {
				line += " — " + s.Summary
			}
			line += "  (" + s.Path + ")"
			lines = append(lines, line)
			shown++
			if shown >= skillIndexLimit {
				break
			}
		}
		if len(lines) > 0 {
			b.WriteString("\n## SKILLS — read the matching playbook before acting\n")
			for _, line := range lines {
				b.WriteString(line + "\n")
			}
			if shown < len(skills) {
				b.WriteString(fmt.Sprintf("(+%d more under workspace/skills; use list_dir to inspect them)\n", len(skills)-shown))
			}
		}
	}

	// grown tools
	if tools := a.js.ListTools(); len(tools) > 0 {
		b.WriteString("\n## GROWN TOOLS (callable via run_tool)\n")
		for _, t := range tools {
			if a.profile != nil && !containsName(a.profile.Tools, t.Name) {
				continue
			}
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

func containsName(items []string, name string) bool {
	for _, item := range items {
		if item == name {
			return true
		}
	}
	return false
}
func (a *Agent) AllowsTool(name string) bool {
	return a.profile == nil || containsName(a.profile.Tools, name)
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
	prov, model := a.prov, a.Model
	a.consulted = nil
	a.history = append(a.history, providers.Message{Role: "user", Content: userMsg})
	hist := append([]providers.Message{}, a.history...)
	a.mu.Unlock()

	// Lessons from previously recorded outcomes for tasks like this one. This
	// reads the playbook book and takes no agent lock (calling it while holding
	// a.mu deadlocked the loop), so it runs after the lock is released.
	a.mu.Lock()
	a.lessons = a.lessonsFor(userMsg)
	a.mu.Unlock()

	if prov == nil {
		msg := "\n\n⚠️ **model error**: no provider is configured. Open Settings and choose a model.\n"
		a.bus.Publish("agent:token", map[string]string{"text": msg})
		a.mu.Lock()
		a.history = append(a.history, providers.Message{Role: "assistant", Content: msg})
		a.mu.Unlock()
		a.saveMemory()
		return msg
	}

	turnStart := time.Now()
	turnStatus := "done"
	defer func() {
		a.turns.Add(1)
		a.lastTurnMS.Store(time.Since(turnStart).Milliseconds())
		switch turnStatus {
		case "failed":
			a.turnsFailed.Add(1)
		case "cancelled":
			a.turnsCancelled.Add(1)
		case "limited":
			a.turnsLimited.Add(1)
		}
	}()
	final := ""
	guard := newLoopGuard(defaultLoopBudget())
	if a.maxLoops > 0 {
		guard.budget.MaxTurns = a.maxLoops
	}
	for {
		if reason := guard.nextTurn(); reason != "" {
			turnStatus = "limited"
			note := "\n\n⏸ **stopped**: " + reason
			a.bus.Publish("agent:token", map[string]string{"text": note})
			hist = append(hist, providers.Message{Role: "assistant", Content: reason})
			final += note
			break
		}
		if ctx.Err() != nil {
			turnStatus = "cancelled"
			note := "\n\n⏹ **cancelled** by the user."
			a.bus.Publish("agent:token", map[string]string{"text": note})
			hist = append(hist, providers.Message{Role: "assistant", Content: "cancelled by the user"})
			final += note
			break
		}

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
		callModel := func() (string, error) {
			started := time.Now()
			text, err := providers.StreamWithRetry(ctx, prov, model, msgs, stream, providers.DefaultRetryPolicy())
			a.modelCalls.Add(1)
			a.lastModelLatencyMS.Store(time.Since(started).Milliseconds())
			if err != nil {
				a.modelErrors.Add(1)
			}
			return text, err
		}
		text, err := callModel()
		if err != nil {
			turnStatus = "failed"
			errMsg := fmt.Sprintf("\n\n⚠️ **model error**: `%s`\n\n%s", providers.SafeErrorText(err), providers.ErrorHint(err))
			a.bus.Publish("agent:token", map[string]string{"text": errMsg})
			a.mu.Lock()
			a.history = append(a.history, providers.Message{Role: "assistant", Content: errMsg})
			a.mu.Unlock()
			a.saveMemory()
			return errMsg
		}
		if text == "" {
			// An empty turn with no actions ends the conversation cleanly.
			hist = append(hist, providers.Message{Role: "assistant", Content: "(empty response)"})
			break
		}

		hist = append(hist, providers.Message{Role: "assistant", Content: text})
		final = text

		specs, problems := extractProtocol(text)
		if len(specs) == 0 && len(problems) == 0 {
			break // no mutations requested; conversation turn complete
		}

		if len(specs) > 0 {
			// time travel: freeze the genome before this mutation batch lands
			if id, err := a.ws.Snapshot(); err == nil {
				a.mu.Lock()
				a.lastSnapshot = id
				a.mu.Unlock()
			}
		}

		results := make([]string, 0, len(specs)+len(problems))
		keyParts := make([]string, 0, len(specs))
		failed := false
		for _, problem := range problems {
			results = append(results, "protocol_error: "+problem.Reason)
			if problem.Raw != "" {
				results[len(results)-1] += " in " + clipProtocol(problem.Raw, 300)
			}
			failed = true
		}
		for _, spec := range specs {
			keyParts = append(keyParts, spec.Raw)
			act, parseErr := parseAction(spec.Raw)
			if parseErr != nil {
				results = append(results, "parse_error: "+parseErr.Error())
				failed = true
				continue
			}
			// live tool card update in the UI: running → ok/error
			a.bus.Publish("tool:call", map[string]any{
				"action": act.Action,
				"target": act.Name, "path": act.Path,
				"state": "running",
			})
			res := a.Execute(act)
			status := resStatus(res)
			a.toolCalls.Add(1)
			if status != "ok" {
				failed = true
				a.toolFailures.Add(1)
			}
			a.bus.Publish("tool:call", map[string]any{
				"action": act.Action,
				"target": act.Name, "path": act.Path,
				"state": status,
			})
			results = append(results, res)
		}

		if reason := guard.observeBatch(strings.Join(keyParts, "\n"), failed); reason != "" {
			turnStatus = "limited"
			note := "\n\n⏸ **stopped**: " + reason
			a.bus.Publish("agent:token", map[string]string{"text": note})
			hist = append(hist, providers.Message{Role: "assistant", Content: reason})
			final += note
			break
		}

		payload := "EVOLV KERNEL — tool results:\n" + strings.Join(results, "\n") + "\nContinue the task with the next batch of mutations until it is fully done, then summarize what changed. Chain as many batches as the task needs — never stop after one."
		if guard.accountBytes(len(payload)) {
			payload += "\n\n(result budget reached: summarise what you changed and stop asking for more tool output this turn)"
		}
		hist = append(hist, providers.Message{Role: "user", Content: payload})
		a.bus.Publish("agent:phase", map[string]string{"phase": "evolving"})
		// visually separate the next stream round in the chat bubble
		a.bus.Publish("agent:token", map[string]string{"text": "\n\n"})
	}

	a.mu.Lock()
	a.history = hist
	// keep memory bounded without losing the assignment: anchors (the original
	// request, user constraints, declared decisions) survive in a summary.
	if compacted, changed := compactHistory(a.history); changed {
		a.history = compacted
		a.compactions.Add(1)
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

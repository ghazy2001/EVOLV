// Package workspace manages ~/.evolv — EVOLV's living body.
// Everything the organism is made of lives here as plain files the agent
// can rewrite at runtime: the UI it wears, the tools it grows, the skills
// it learns, the prompts it thinks with.
package workspace

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dxn111/EVOLV/internal/redact"
)

// Layout of the organism on disk.
const (
	RootDir        = ".evolv"
	WorkspaceDir   = "workspace" // the whole served UI + everything evolvable
	UIDir          = "workspace/ui"
	ComponentsDir  = "workspace/ui/components" // hot-mounted panels
	PluginsDir     = "workspace/plugins"       // JS plugins (goja)
	ToolsDir       = "workspace/tools"         // JS tool handlers
	SkillsDir      = "workspace/skills"        // markdown skills
	PromptsDir     = "workspace/prompts"       // system prompt fragments (evolvable)
	LogsDir        = "logs"
	ConfigFile     = "config.json"
	ManifestFile   = "workspace/manifest.json"
	EvolutionFile  = "workspace/evolution.jsonl" // append-only mutation history
	PlanFile       = "workspace/plan.json"       // the agent's current task plan
	RepoMapFile    = "workspace/repo-map.json"   // cached repository tree + symbol outline
	CandidatesDir  = "workspace/candidates"      // isolated, auditable self-evolution proposals
	ScreenshotsDir = "workspace/screenshots"     // visual canvas & organ screenshots
)

// Config is the user-editable kernel config.
type Config struct {
	Provider          string            `json:"provider"`       // kilo | openai | anthropic | openrouter | groq | ollama | custom | mock
	Model             string            `json:"model"`          // model id for the provider
	APIKeys           map[string]string `json:"api_keys"`       // BYOK: provider -> key
	BaseURLs          map[string]string `json:"base_urls"`      // custom endpoints
	Port              int               `json:"port"`           // 0 = auto (7373 then free)
	OpenBrowser       bool              `json:"open_browser"`   // auto open on boot
	AutoEvolve        bool              `json:"auto_evolve"`    // ambient heartbeat evolutions
	HeartbeatMins     int               `json:"heartbeat_mins"` // cadence of ambient evolution
	UserName          string            `json:"user_name"`      // how the organism addresses you
	Theme             string            `json:"theme"`          // ui theme hint the agent may rewrite
	ProjectDir        string            `json:"project_dir"`    // user's project root the agent works on (optional)
	DisabledWidgets   map[string]bool   `json:"disabled_widgets,omitempty"`
	PinnedCreations   map[string]bool   `json:"pinned_creations,omitempty"`
	ArchivedCreations map[string]bool   `json:"archived_creations,omitempty"`
	Contributing      string            `json:"contributing,omitempty"` // "" | "on" — may contribute upstream
	Version           string            `json:"version"`
	InstalledAt       string            `json:"installed_at"`
}

// W is the active workspace.
type W struct {
	Root   string // absolute path to ~/.evolv
	Config *Config
	uiFS   fs.FS // embedded pristine UI for (re)seeding

	migrated     MigrateReport
	migrationErr error
}

// Dir returns an absolute path inside the workspace.
func (w *W) Dir(rel string) string { return filepath.Join(w.Root, rel) }

// ---------- task plan (the agent's persistent to-do state) ----------

// PlanStep is one line of the plan: todo → doing → done.
type PlanStep struct {
	Title  string `json:"title"`
	Status string `json:"status"`
}

// Plan is the agent's current multi-step task state, persisted so it
// survives restarts and renders live in the UI.
type Plan struct {
	Title   string     `json:"title"`
	Steps   []PlanStep `json:"steps"`
	Updated string     `json:"updated"`
}

// WritePlan persists the plan (atomic: temp file + rename).
func (w *W) WritePlan(p Plan) error {
	if p.Updated == "" {
		p.Updated = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := w.Dir(PlanFile + ".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.Dir(PlanFile))
}

// ReadPlan returns the persisted plan, or nil when there is none.
func (w *W) ReadPlan() *Plan {
	raw, err := os.ReadFile(w.Dir(PlanFile))
	if err != nil {
		return nil
	}
	var p Plan
	if json.Unmarshal(raw, &p) != nil || len(p.Steps) == 0 {
		return nil
	}
	return &p
}

// Default config.
func defaultConfig() *Config {
	return &Config{
		Provider:          "kilo",
		Model:             "kilo-auto/free",
		APIKeys:           map[string]string{},
		BaseURLs:          map[string]string{},
		DisabledWidgets:   map[string]bool{},
		PinnedCreations:   map[string]bool{},
		ArchivedCreations: map[string]bool{},
		Port:              7373,
		OpenBrowser:       true,
		AutoEvolve:        false,
		HeartbeatMins:     15,
		UserName:          "friend",
		Theme:             "obsidian",
		Version:           Version,
		InstalledAt:       time.Now().UTC().Format(time.RFC3339),
	}
}

// Version is the organism's genome version (overridable at build time via
// -ldflags "-X github.com/dxn111/EVOLV/internal/workspace.Version=...").
var Version = "0.33.0"

// versionLess reports whether semantic version a is older than b. It is a
// deliberately small parser: three numeric fields, optional leading "v".
func versionLess(a, b string) bool {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

// Load opens (creating if needed) the workspace at ~/.evolv.
// uiFS is the embedded factory genome used to seed on first run.
func Load(uiFS fs.FS) (*W, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	w := &W{Root: filepath.Join(home, RootDir), uiFS: uiFS, Config: defaultConfig()}
	if err := os.MkdirAll(w.Root, 0o755); err != nil {
		return nil, err
	}
	// config
	cfgPath := w.Dir(ConfigFile)
	if raw, err := os.ReadFile(cfgPath); err == nil {
		var prev struct {
			Version string `json:"version"`
		}
		_ = json.Unmarshal(raw, &prev)
		_ = json.Unmarshal(raw, w.Config) // partial merge; missing keys keep defaults
		if w.Config.APIKeys == nil {
			w.Config.APIKeys = map[string]string{}
		}
		if w.Config.BaseURLs == nil {
			w.Config.BaseURLs = map[string]string{}
		}
		// v0.29.0 turned browser auto-open on by default. Older genomes wrote
		// `open_browser: false` as the old default without the user choosing
		// it, so migrate them once. Explicit opt-outs on current genomes stay.
		migrated := false
		if !w.Config.OpenBrowser && (prev.Version == "" || versionLess(prev.Version, "0.29.0")) {
			w.Config.OpenBrowser = true
			migrated = true
		}
		w.Config.Version = Version
		if migrated {
			w.SaveConfig()
		}
	} else {
		w.SaveConfig()
	}
	// directories
	for _, d := range []string{WorkspaceDir, UIComponentsRel(), PluginsDir, ToolsDir, SkillsDir, PromptsDir, CandidatesDir, LogsDir, SnapshotsDir, ScreenshotsDir} {
		if err := os.MkdirAll(w.Dir(d), 0o755); err != nil {
			return nil, err
		}
	}
	// seed UI on first run (or after `evolv reset --ui`)
	if err := w.seedUI(); err != nil {
		return nil, err
	}
	// seed prompts/skills if missing
	if err := w.seedSeeds(); err != nil {
		return nil, err
	}
	// versioned workspace migrations. A migration failure must not brick the
	// boot: the workspace still opens, and the failure is surfaced through
	// LastMigrationError for the UI and logs.
	if report, err := w.Migrate(MigrateOptions{}); err != nil {
		w.migrationErr = err
		w.migrated = report
	} else {
		w.migrated = report
	}
	return w, nil
}

// MigrationReport returns the migrations applied during the last Load.
func (w *W) MigrationReport() MigrateReport { return w.migrated }

// MigrationError returns the error from the last migration run, if any.
func (w *W) MigrationError() error { return w.migrationErr }

// UIComponentsRel is the workspace-relative components path.
func UIComponentsRel() string { return ComponentsDir }

// seedUI copies embedded ui/ into the workspace if the workspace UI is absent,
// or older than the embedded shell. The shell (index.html/app.js/style.css) is
// version-stamped with .uiversion: when the binary ships a newer shell, the
// installed genome's shell is upgraded in place. Components (workspace/ui/
// components/) are the organism's own grown tissue and are NEVER clobbered.
func (w *W) seedUI() error {
	if w.uiFS == nil {
		return nil
	}
	marker := filepath.Join(w.Dir(UIRel()), ".uiversion")
	embeddedVer, _ := fs.ReadFile(w.uiFS, "ui/.uiversion")
	installedVer, err := os.ReadFile(marker)
	if err == nil && strings.TrimSpace(string(installedVer)) == strings.TrimSpace(string(embeddedVer)) {
		return nil // same shell generation; never clobber a living organism
	}
	return fs.WalkDir(w.uiFS, "ui", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// grown components survive every shell upgrade
		if strings.HasPrefix(p, "ui/components/") {
			return nil
		}
		// embedded "ui/x" → "workspace/ui/x"
		rel := WorkspaceDir + "/" + p
		dest := filepath.Join(w.Root, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		data, err := fs.ReadFile(w.uiFS, p)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
}

// seedSeeds writes default prompts & skills when missing.
func (w *W) seedSeeds() error {
	if w.uiFS == nil {
		return nil
	}
	return fs.WalkDir(w.uiFS, "seeds", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// embedded "seeds/skills/x.md" → "workspace/skills/x.md"
		rel := WorkspaceDir + "/" + strings.TrimPrefix(p, "seeds/")
		dest := filepath.Join(w.Root, rel)
		if _, err := os.Stat(dest); err == nil {
			return nil
		}
		data, err := fs.ReadFile(w.uiFS, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
}

// UIRel is the workspace-relative UI path.
func UIRel() string { return UIDir }

// SaveConfig persists config.json atomically.
func (w *W) SaveConfig() {
	raw, _ := json.MarshalIndent(w.Config, "", "  ")
	tmp := w.Dir(ConfigFile + ".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		_ = os.Rename(tmp, w.Dir(ConfigFile))
	}
}

// AppendEvolution records every mutation in evolution.jsonl with credentials redacted.
func (w *W) AppendEvolution(entry map[string]any) {
	entry = redact.RedactMap(entry)
	entry["time"] = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(w.Dir(EvolutionFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(raw, '\n'))
}

// ReadEvolution returns the last n mutations.
func (w *W) ReadEvolution(n int) []map[string]any {
	raw, err := os.ReadFile(w.Dir(EvolutionFile))
	if err != nil {
		return nil
	}
	var out []map[string]any
	start := 0
	if len(raw) > 0 {
		lines := splitLines(raw)
		if n > 0 && len(lines) > n {
			start = len(lines) - n
		}
		for _, ln := range lines[start:] {
			var m map[string]any
			if json.Unmarshal(ln, &m) == nil && len(m) > 0 {
				out = append(out, m)
			}
		}
	}
	return out
}

func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	cur := []byte{}
	for _, b := range raw {
		if b == '\n' {
			if len(cur) > 0 {
				lines = append(lines, cur)
			}
			cur = []byte{}
		} else {
			cur = append(cur, b)
		}
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	return lines
}

// OpenBrowser launches the user's default browser at url (best effort).
// It tries the platform opener first, then common browser binaries, so a
// minimal desktop without xdg-utils still gets a window.
func OpenBrowser(url string) {
	type attempt struct {
		cmd  string
		args []string
	}
	var tries []attempt
	switch runtime.GOOS {
	case "darwin":
		tries = []attempt{{"open", []string{url}}}
	case "windows":
		tries = []attempt{{"rundll32", []string{"url.dll,FileProtocolHandler", url}}}
	default:
		tries = []attempt{
			{"xdg-open", []string{url}},
			{"sensible-browser", []string{url}},
			{"firefox", []string{url}},
			{"chromium", []string{url}},
			{"google-chrome", []string{url}},
		}
	}
	for _, a := range tries {
		path, err := exec.LookPath(a.cmd)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, a.args...)
		if err := cmd.Start(); err != nil {
			continue
		}
		go func() { _ = cmd.Wait() }()
		return
	}
}

// PickPort finds a free TCP port starting from prefer.
func PickPort(prefer int) int {
	if free(prefer) {
		return prefer
	}
	for p := prefer + 1; p < prefer+200; p++ {
		if free(p) {
			return p
		}
	}
	return prefer
}

func free(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// SystemInfo returns a small factsheet used in prompts.
func (w *W) SystemInfo() string {
	return fmt.Sprintf("os=%s arch=%s workspace=%s version=%s", runtime.GOOS, runtime.GOARCH, w.Root, Version)
}

// LauncherPath finds the running evolv executable (the launcher the agent may
// rewrite the source of). Returns "" if unknown.
func LauncherPath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// DetectSourceRoot walks up from the launcher and the working directory
// looking for the EVOLV Go source tree (go.mod with module github.com/dxn111/EVOLV).
// Returns "" when running as a plain installed binary.
func DetectSourceRoot() string {
	candidates := []string{}
	if lp := LauncherPath(); lp != "" {
		candidates = append(candidates, filepath.Dir(lp))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	for _, start := range candidates {
		d := start
		for i := 0; i < 6 && d != filepath.Dir(d); i++ {
			if mod, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil && strings.Contains(string(mod), "github.com/dxn111/EVOLV") {
				return d
			}
			d = filepath.Dir(d)
		}
	}
	return ""
}

// Package candidate builds self-evolution proposals away from the living tree.
package candidate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dxn111/EVOLV/internal/workspace"
)

type Change struct {
	Path, BeforeHash, AfterHash string
	Bytes                       int64
}
type Check struct {
	Command    string `json:"command"`
	OK         bool   `json:"ok"`
	Output     string `json:"output"`
	DurationMS int64  `json:"duration_ms"`
}
type Manifest struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	SourceRoot string   `json:"source_root"`
	Status     string   `json:"status"`
	Created    string   `json:"created"`
	Promoted   string   `json:"promoted,omitempty"`
	Artifact   string   `json:"artifact,omitempty"`
	Rollback   string   `json:"rollback,omitempty"`
	Changes    []Change `json:"changes"`
	Checks     []Check  `json:"checks"`
}
type Proposal struct {
	Name           string
	Files          map[string]string
	ExpectedHashes map[string]string
}
type Manager struct {
	WS         *workspace.W
	SourceRoot string
}

func hash(raw []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(raw)) }
func cleanRel(p string) (string, error) {
	// Normalise both separators before cleaning. Windows-style traversals such
	// as `..\escape.go` must be rejected on every platform, not just Windows.
	raw := strings.ReplaceAll(p, "\\", "/")
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
		return "", fmt.Errorf("candidate path escapes source: %q", p)
	}
	return filepath.FromSlash(clean), nil
}
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && b.String()[b.Len()-1] != '-' {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func (m *Manager) Stage(ctx context.Context, p Proposal) (*Manifest, error) {
	if m.SourceRoot == "" {
		return nil, fmt.Errorf("EVOLV source root not found")
	}
	if len(p.Files) == 0 {
		return nil, fmt.Errorf("candidate needs at least one file")
	}
	id := time.Now().UTC().Format("20060102-150405.000000000") + "-" + slug(p.Name)
	if strings.HasSuffix(id, "-") {
		id += "candidate"
	}
	root := m.WS.Dir(filepath.Join(workspace.CandidatesDir, id))
	tree := filepath.Join(root, "source")
	if err := copyTree(m.SourceRoot, tree); err != nil {
		return nil, err
	}
	man := &Manifest{ID: id, Name: p.Name, SourceRoot: m.SourceRoot, Status: "testing", Created: time.Now().UTC().Format(time.RFC3339)}
	paths := make([]string, 0, len(p.Files))
	for path := range p.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, rawPath := range paths {
		rel, err := cleanRel(rawPath)
		if err != nil {
			return nil, err
		}
		live := filepath.Join(m.SourceRoot, rel)
		before, readErr := os.ReadFile(live)
		beforeHash := ""
		if readErr == nil {
			beforeHash = hash(before)
		} else if !os.IsNotExist(readErr) {
			return nil, readErr
		}
		if want := p.ExpectedHashes[rawPath]; beforeHash != "" && (want == "" || want != beforeHash) {
			return nil, fmt.Errorf("stale candidate input for %s: current %s", rel, beforeHash)
		}
		next := []byte(p.Files[rawPath])
		dest := filepath.Join(tree, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dest, next, 0o644); err != nil {
			return nil, err
		}
		man.Changes = append(man.Changes, Change{Path: filepath.ToSlash(rel), BeforeHash: beforeHash, AfterHash: hash(next), Bytes: int64(len(next))})
	}
	commands := [][]string{{"go", "test", "./..."}, {"go", "build", "./..."}}
	for _, args := range commands {
		check := runCommand(ctx, tree, args...)
		man.Checks = append(man.Checks, check)
		if !check.OK {
			man.Status = "failed"
			_ = writeManifest(root, man)
			return man, nil
		}
	}
	if _, err := os.Stat(filepath.Join(tree, "cmd", "evolv")); err == nil {
		artifactDir := filepath.Join(root, "artifacts")
		if err := os.MkdirAll(artifactDir, 0o755); err != nil {
			return nil, err
		}
		binary := filepath.Join(artifactDir, "evolv-candidate")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		check := runCommand(ctx, tree, "go", "build", "-o", binary, "./cmd/evolv")
		check.Command = "go build candidate artifact"
		man.Checks = append(man.Checks, check)
		if !check.OK {
			man.Status = "failed"
			_ = writeManifest(root, man)
			return man, nil
		}
		man.Artifact = filepath.ToSlash(filepath.Join("artifacts", filepath.Base(binary)))
		boot := bootHealthCheck(ctx, binary, filepath.Join(root, "runtime-home"))
		man.Checks = append(man.Checks, boot)
		if !boot.OK {
			man.Status = "failed"
			_ = writeManifest(root, man)
			return man, nil
		}
	}
	man.Status = "ready"
	return man, writeManifest(root, man)
}

func runCommand(parent context.Context, dir string, args ...string) Check {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if len(out) > 64<<10 {
		out = out[len(out)-(64<<10):]
	}
	check := Check{Command: strings.Join(args, " "), OK: err == nil, Output: string(out), DurationMS: time.Since(start).Milliseconds()}
	if err != nil {
		check.Output += fmt.Sprintf("\nerror: %v", err)
	}
	return check
}

func bootHealthCheck(parent context.Context, binary, isolatedHome string) Check {
	start := time.Now()
	check := Check{Command: "boot smoke GET /api/health"}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		check.Output = err.Error()
		return check
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	if err = os.MkdirAll(isolatedHome, 0o755); err != nil {
		check.Output = err.Error()
		return check
	}
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--port", strconv.Itoa(port), "--no-open")
	cmd.Env = isolatedHomeEnv(isolatedHome)
	cmd.Dir = filepath.Dir(binary)
	if err = cmd.Start(); err != nil {
		check.Output = err.Error()
		return check
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	url := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
	client := &http.Client{Timeout: 700 * time.Millisecond}
	var last string
	for {
		select {
		case err = <-done:
			check.Output = fmt.Sprintf("candidate exited before health check: %v", err)
			check.DurationMS = time.Since(start).Milliseconds()
			return check
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			check.Output = "health timeout: " + last
			check.DurationMS = time.Since(start).Milliseconds()
			return check
		case <-time.After(150 * time.Millisecond):
			resp, requestErr := client.Get(url)
			if requestErr != nil {
				last = requestErr.Error()
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				check.OK, check.Output = true, fmt.Sprintf("healthy on isolated port %d", port)
				_ = cmd.Process.Kill()
				<-done
				check.DurationMS = time.Since(start).Milliseconds()
				return check
			}
			last = resp.Status
		}
	}
}

func isolatedHomeEnv(home string) []string {
	out := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		upper := strings.ToUpper(item)
		if strings.HasPrefix(upper, "HOME=") || strings.HasPrefix(upper, "USERPROFILE=") {
			continue
		}
		out = append(out, item)
	}
	return append(out, "HOME="+home, "USERPROFILE="+home)
}

func (m *Manager) Promote(id string) (*Manifest, error) {
	man, root, err := m.load(id)
	if err != nil {
		return nil, err
	}
	if man.Status != "ready" {
		return nil, fmt.Errorf("candidate %s is %s, not ready", id, man.Status)
	}
	type prepared struct {
		change       Change
		next, before []byte
		existed      bool
	}
	files := make([]prepared, 0, len(man.Changes))
	for _, c := range man.Changes {
		live := filepath.Join(m.SourceRoot, filepath.FromSlash(c.Path))
		raw, err := os.ReadFile(live)
		existed := err == nil
		current := ""
		if err == nil {
			current = hash(raw)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if current != c.BeforeHash {
			return nil, fmt.Errorf("promotion blocked: %s changed since staging", c.Path)
		}
		next, err := os.ReadFile(filepath.Join(root, "source", filepath.FromSlash(c.Path)))
		if err != nil {
			return nil, err
		}
		if hash(next) != c.AfterHash {
			return nil, fmt.Errorf("candidate evidence mismatch for %s", c.Path)
		}
		files = append(files, prepared{change: c, next: next, before: raw, existed: existed})
	}
	backupRoot := filepath.Join(root, "promotion-backup", time.Now().UTC().Format("20060102-150405.000000000"))
	for _, file := range files {
		if !file.existed {
			continue
		}
		backup := filepath.Join(backupRoot, filepath.FromSlash(file.change.Path))
		if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(backup, file.before, 0o644); err != nil {
			return nil, err
		}
	}
	written := make([]prepared, 0, len(files))
	rollback := func(cause error) error {
		for i := len(written) - 1; i >= 0; i-- {
			file := written[i]
			dst := filepath.Join(m.SourceRoot, filepath.FromSlash(file.change.Path))
			if file.existed {
				_ = os.WriteFile(dst, file.before, 0o644)
			} else {
				_ = os.Remove(dst)
			}
		}
		man.Status = "promotion_failed"
		man.Rollback = filepath.ToSlash(backupRoot)
		_ = writeManifest(root, man)
		return fmt.Errorf("promotion rolled back: %w", cause)
	}
	for _, file := range files {
		dst := filepath.Join(m.SourceRoot, filepath.FromSlash(file.change.Path))
		if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return man, rollback(err)
		}
		if err = os.WriteFile(dst, file.next, 0o644); err != nil {
			return man, rollback(err)
		}
		written = append(written, file)
	}
	for _, args := range [][]string{{"go", "test", "./..."}, {"go", "build", "./..."}} {
		check := runCommand(context.Background(), m.SourceRoot, args...)
		check.Command = "post-promote " + check.Command
		man.Checks = append(man.Checks, check)
		if !check.OK {
			return man, rollback(fmt.Errorf("%s failed", check.Command))
		}
	}
	man.Status = "promoted"
	man.Promoted = time.Now().UTC().Format(time.RFC3339)
	man.Rollback = filepath.ToSlash(backupRoot)
	return man, writeManifest(root, man)
}
func (m *Manager) List() []Manifest {
	ents, _ := os.ReadDir(m.WS.Dir(workspace.CandidatesDir))
	out := []Manifest{}
	for _, e := range ents {
		if e.IsDir() {
			if man, _, err := m.load(e.Name()); err == nil {
				out = append(out, *man)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}
func (m *Manager) load(id string) (*Manifest, string, error) {
	clean, err := cleanRel(id)
	if err != nil || strings.Contains(clean, string(filepath.Separator)) {
		return nil, "", fmt.Errorf("invalid candidate id")
	}
	root := m.WS.Dir(filepath.Join(workspace.CandidatesDir, clean))
	raw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil, "", err
	}
	var man Manifest
	if err = json.Unmarshal(raw, &man); err != nil {
		return nil, "", err
	}
	return &man, root, nil
}
func writeManifest(root string, man *Manifest) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "manifest.json"), raw, 0o644)
}
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "upstream-work" || d.Name() == "dist") {
			return fs.SkipDir
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow links outside the source tree
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > 8<<20 {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, raw, info.Mode().Perm())
	})
}

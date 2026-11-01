package workspace

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// PackManifest describes the contents of an .evolvpack package archive.
type PackManifest struct {
	Format      string         `json:"format"`      // "evolvpack/v1"
	Version     string         `json:"version"`     // EVOLV version
	CreatedAt   string         `json:"created_at"`  // RFC3339 timestamp
	Author      string         `json:"author"`      // pack author
	Description string         `json:"description"` // optional notes
	Files       []PackFileMeta `json:"files"`
}

// PackFileMeta holds cryptographic and structural metadata for one packed file.
type PackFileMeta struct {
	Path   string `json:"path"`   // workspace-relative path (slash-separated)
	SHA256 string `json:"sha256"` // hex-encoded sha256 checksum
	Bytes  int64  `json:"bytes"`  // size in bytes
	Kind   string `json:"kind"`   // component | tool | skill | prompt | plugin | ui | ledger | other
}

// ExportOptions controls workspace export.
type ExportOptions struct {
	Author        string
	Description   string
	IncludeLedger bool // whether to bundle evolution.jsonl history
}

// ImportOptions controls package import and safety enforcement.
type ImportOptions struct {
	DryRun            bool // preview changes without writing files
	Force             bool // overwrite conflicting local files
	QuarantinePlugins bool // import plugins with quarantine protection
}

// ImportConflict describes what will happen to an individual file during import.
type ImportConflict struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Identical bool   `json:"identical"`
	Action    string `json:"action"` // created | overwritten | skipped | quarantined
}

// ImportReport summarizes the outcome of importing an .evolvpack.
type ImportReport struct {
	Manifest    *PackManifest    `json:"manifest"`
	Conflicts   []ImportConflict `json:"conflicts"`
	Created     []string         `json:"created"`
	Overwritten []string         `json:"overwritten"`
	Skipped     []string         `json:"skipped"`
	Quarantined []string         `json:"quarantined"`
}

const (
	PackFormatV1     = "evolvpack/v1"
	ManifestEntryName = "manifest.json"
	QuarantineNotice = "QUARANTINE.md"
	QuarantineSuffix = ".quarantine"
)

// secretPatterns catches common token shapes to prevent accidental credential leakage.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)sk-[a-zA-Z0-9]{20,}`),
	regexp.MustCompile(`(?i)sk-ant-[a-zA-Z0-9_-]{20,}`),
	regexp.MustCompile(`(?i)(?:ghp_[a-zA-Z0-9]{20,}|github_pat_[a-zA-Z0-9_]{20,})`),
	regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`),
}

// scanSecrets inspects file content for credentials before packaging.
func scanSecrets(relPath string, data []byte) error {
	base := filepath.Base(relPath)
	if strings.EqualFold(base, ConfigFile) {
		return fmt.Errorf("refusing to pack %s: configuration files may contain private API keys", relPath)
	}
	for _, re := range secretPatterns {
		if re.Match(data) {
			return fmt.Errorf("refusing to pack %s: detected credential or API key token", relPath)
		}
	}
	return nil
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func classifyKind(rel string) string {
	rel = filepath.ToSlash(rel)
	switch {
	case strings.HasPrefix(rel, "workspace/ui/components/"):
		return "component"
	case strings.HasPrefix(rel, "workspace/tools/"):
		return "tool"
	case strings.HasPrefix(rel, "workspace/skills/"):
		return "skill"
	case strings.HasPrefix(rel, "workspace/prompts/"):
		return "prompt"
	case strings.HasPrefix(rel, "workspace/plugins/"):
		return "plugin"
	case strings.HasPrefix(rel, "workspace/ui/"):
		return "ui"
	case rel == EvolutionFile:
		return "ledger"
	default:
		return "other"
	}
}

// Export streams an .evolvpack tar.gz archive into out.
func (w *W) Export(out io.Writer, opts ExportOptions) (*PackManifest, error) {
	gw := gzip.NewWriter(out)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	author := strings.TrimSpace(opts.Author)
	if author == "" && w.Config != nil {
		author = w.Config.UserName
	}
	if author == "" {
		author = "friend"
	}

	manifest := &PackManifest{
		Format:      PackFormatV1,
		Version:     Version,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Author:      author,
		Description: strings.TrimSpace(opts.Description),
		Files:       []PackFileMeta{},
	}

	// Candidate file parts to collect
	dirs := []string{
		UIComponentsRel(),
		ToolsDir,
		SkillsDir,
		PromptsDir,
		PluginsDir,
	}

	// UI assets if present
	for _, f := range []string{"workspace/ui/style.css", "workspace/ui/app.js", "workspace/ui/index.html"} {
		if fileExists(w.Dir(f)) {
			dirs = append(dirs, f)
		}
	}

	if opts.IncludeLedger && fileExists(w.Dir(EvolutionFile)) {
		dirs = append(dirs, EvolutionFile)
	}

	filesMap := map[string][]byte{}

	for _, d := range dirs {
		full := w.Dir(d)
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			data, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			rel := filepath.ToSlash(d)
			if err := scanSecrets(rel, data); err != nil {
				return nil, err
			}
			filesMap[rel] = data
			continue
		}

		walkErr := filepath.WalkDir(full, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !entry.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(w.Root, p)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			if err := scanSecrets(rel, data); err != nil {
				return err
			}
			filesMap[rel] = data
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}

	// Sort file paths for reproducible archives
	keys := make([]string, 0, len(filesMap))
	for k := range filesMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		data := filesMap[k]
		sum := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, PackFileMeta{
			Path:   k,
			SHA256: hex.EncodeToString(sum[:]),
			Bytes:  int64(len(data)),
			Kind:   classifyKind(k),
		})
	}

	// Write manifest.json as the first entry
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	mHeader := &tar.Header{
		Name:    ManifestEntryName,
		Mode:    0o644,
		Size:    int64(len(manifestJSON)),
		ModTime: time.Now().UTC(),
	}
	if err := tw.WriteHeader(mHeader); err != nil {
		return nil, err
	}
	if _, err := tw.Write(manifestJSON); err != nil {
		return nil, err
	}

	// Write payload files
	for _, k := range keys {
		data := filesMap[k]
		header := &tar.Header{
			Name:    k,
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: time.Now().UTC(),
		}
		if err := tw.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}

	return manifest, nil
}

// ExportFile exports workspace contents to a file path atomically.
func (w *W) ExportFile(destPath string, opts ExportOptions) (*PackManifest, error) {
	if destPath == "" {
		destPath = fmt.Sprintf("evolv-%s.evolvpack", time.Now().UTC().Format("20060102-150405"))
	}
	dir := filepath.Dir(destPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	} else {
		dir = "."
	}

	tmpFile, err := os.CreateTemp(dir, ".evolvpack-*.tmp")
	if err != nil {
		return nil, err
	}
	tmpName := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
	}()

	manifest, err := w.Export(tmpFile, opts)
	if err != nil {
		return nil, err
	}
	if err := tmpFile.Close(); err != nil {
		return nil, err
	}

	if fileExists(destPath) {
		_ = os.Remove(destPath)
	}
	if err := os.Rename(tmpName, destPath); err != nil {
		return nil, err
	}
	return manifest, nil
}

// Import unpacks an .evolvpack from in into the workspace.
func (w *W) Import(in io.Reader, opts ImportOptions) (*ImportReport, error) {
	gr, err := gzip.NewReader(in)
	if err != nil {
		return nil, fmt.Errorf("invalid package: not a gzip archive: %w", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	tarFiles := map[string][]byte{}
	var manifest *PackManifest

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading archive entry: %w", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}

		clean := filepath.ToSlash(filepath.Clean(hdr.Name))
		if strings.HasPrefix(clean, "../") || clean == ".." || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("security violation: illegal path in archive: %q", hdr.Name)
		}

		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", hdr.Name, err)
		}

		if clean == ManifestEntryName {
			var m PackManifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("invalid manifest.json in package: %w", err)
			}
			manifest = &m
			continue
		}

		tarFiles[clean] = data
	}

	if manifest == nil {
		return nil, errors.New("invalid package: missing manifest.json")
	}
	if manifest.Format != PackFormatV1 {
		return nil, fmt.Errorf("unsupported package format: %s", manifest.Format)
	}

	// Validate checksums against manifest
	metaByPath := map[string]PackFileMeta{}
	for _, meta := range manifest.Files {
		metaByPath[filepath.ToSlash(meta.Path)] = meta
	}

	// Sort file paths for deterministic processing
	keys := make([]string, 0, len(tarFiles))
	for k := range tarFiles {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type pendingWrite struct {
		destPath  string
		data      []byte
		noticeDir string
		noticeMsg string
	}
	var writes []pendingWrite

	report := &ImportReport{
		Manifest:    manifest,
		Conflicts:   make([]ImportConflict, 0, len(keys)),
		Created:     []string{},
		Overwritten: []string{},
		Skipped:     []string{},
		Quarantined: []string{},
	}

	// Pass 1: Pre-flight validation of all entries before modifying disk
	for _, path := range keys {
		data := tarFiles[path]
		meta, ok := metaByPath[path]
		if !ok {
			return nil, fmt.Errorf("archive contains undeclared file: %s", path)
		}
		sum := sha256.Sum256(data)
		actualSum := hex.EncodeToString(sum[:])
		if actualSum != meta.SHA256 {
			return nil, fmt.Errorf("checksum mismatch for %s: manifest=%s actual=%s", path, meta.SHA256, actualSum)
		}

		// Security scan imported files as well
		if err := scanSecrets(path, data); err != nil {
			return nil, err
		}

		dest := w.Dir(path)
		// Ensure destination is strictly inside w.Root
		relCheck, err := filepath.Rel(w.Root, dest)
		if err != nil || strings.HasPrefix(filepath.ToSlash(relCheck), "../") || filepath.IsAbs(relCheck) {
			return nil, fmt.Errorf("security violation: path escapes workspace root: %s", path)
		}

		exists := fileExists(dest)
		identical := false
		action := "create"

		if exists {
			cur, err := os.ReadFile(dest)
			if err == nil {
				curSum := sha256.Sum256(cur)
				if hex.EncodeToString(curSum[:]) == meta.SHA256 {
					identical = true
					action = "skip"
				} else if opts.Force {
					action = "overwrite"
				} else {
					action = "skip"
				}
			}
		}

		// Plugin quarantine: if quarantine is enabled, quarantine incoming plugins
		isPlugin := strings.HasPrefix(path, "workspace/plugins/")
		if isPlugin && opts.QuarantinePlugins {
			action = "quarantine"
		}

		report.Conflicts = append(report.Conflicts, ImportConflict{
			Path:      path,
			Exists:    exists,
			Identical: identical,
			Action:    action,
		})

		if action == "skip" {
			report.Skipped = append(report.Skipped, path)
			continue
		}

		finalDest := dest
		var noticeDir, noticeMsg string
		if action == "quarantine" && strings.HasSuffix(path, "plugin.json") {
			finalDest = dest + QuarantineSuffix
			noticeDir = filepath.Dir(dest)
			noticeMsg = fmt.Sprintf("# QUARANTINE NOTICE\n\nPlugin %s was imported from an external .evolvpack.\nReview plugin.js before enabling.\nTo enable: rename plugin.json.quarantine -> plugin.json\n", filepath.Base(filepath.Dir(dest)))
			report.Quarantined = append(report.Quarantined, path)
		} else if action == "create" {
			report.Created = append(report.Created, path)
		} else if action == "overwrite" {
			report.Overwritten = append(report.Overwritten, path)
		}

		writes = append(writes, pendingWrite{
			destPath:  finalDest,
			data:      data,
			noticeDir: noticeDir,
			noticeMsg: noticeMsg,
		})
	}

	if opts.DryRun {
		return report, nil
	}

	// Pass 2: Execute disk writes only after full validation passed
	for _, pw := range writes {
		if pw.noticeDir != "" {
			if err := os.MkdirAll(pw.noticeDir, 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(pw.noticeDir, QuarantineNotice), []byte(pw.noticeMsg), 0o644); err != nil {
				return nil, err
			}
		}

		if err := os.MkdirAll(filepath.Dir(pw.destPath), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(pw.destPath, pw.data, 0o644); err != nil {
			return nil, err
		}
	}

	return report, nil
}

// ImportFile reads an .evolvpack archive from disk and applies it.
func (w *W) ImportFile(packPath string, opts ImportOptions) (*ImportReport, error) {
	f, err := os.Open(packPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return w.Import(f, opts)
}

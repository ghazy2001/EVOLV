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
	Action    string `json:"action"` // create | overwrite | skip | quarantine
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
	PackFormatV1      = "evolvpack/v1"
	ManifestEntryName = "manifest.json"
	QuarantineNotice  = "QUARANTINE.md"
	QuarantineSuffix  = ".quarantine"

	// Decompression limits to protect against malformed or malicious archive bombs.
	MaxPackEntryBytes = 16 * 1024 * 1024 // 16 MiB per archive entry
	MaxPackTotalBytes = 64 * 1024 * 1024 // 64 MiB total unpacked archive payload
	MaxPackEntries    = 4096
)

// secretPatterns catches common token shapes to prevent accidental credential leakage.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsk-[a-zA-Z0-9_-]{20,}`),
	regexp.MustCompile(`(?i)\bsk-ant-[a-zA-Z0-9_-]{20,}`),
	regexp.MustCompile(`(?i)(?:gh[pousr]_[a-zA-Z0-9]{20,}|github_pat_[a-zA-Z0-9_]{20,})`),
	regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`),
}

// scanSecrets inspects file content for credentials before packaging or importing.
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

// isAllowedPackPath enforces an exact-case allowlist of workspace paths permitted in an .evolvpack.
func isAllowedPackPath(clean string) bool {
	if clean == "" || strings.ContainsRune(clean, '\\') {
		return false
	}
	switch clean {
	case "workspace/ui/style.css", "workspace/ui/app.js", "workspace/ui/index.html", EvolutionFile:
		return true
	}
	if rest, ok := strings.CutPrefix(clean, "workspace/plugins/"); ok {
		// Require workspace/plugins/<plugin>/<file>
		idx := strings.IndexByte(rest, '/')
		return idx > 0 && idx < len(rest)-1
	}
	for _, prefix := range []string{
		"workspace/ui/components/",
		"workspace/tools/",
		"workspace/skills/",
		"workspace/prompts/",
	} {
		if rest, ok := strings.CutPrefix(clean, prefix); ok && rest != "" {
			return true
		}
	}
	return false
}

func extractPluginName(clean string) (string, bool) {
	rest, ok := strings.CutPrefix(clean, "workspace/plugins/")
	if !ok {
		return "", false
	}
	idx := strings.IndexByte(rest, '/')
	if idx <= 0 {
		return "", false
	}
	return rest[:idx], true
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
	tw := tar.NewWriter(gw)
	closed := false
	defer func() {
		if !closed {
			_ = tw.Close()
			_ = gw.Close()
		}
	}()

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
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", d, err)
		}
		if !info.IsDir() {
			data, err := os.ReadFile(full)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", d, err)
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
				return fmt.Errorf("resolving relative path for %s: %w", p, err)
			}
			rel = filepath.ToSlash(rel)
			data, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("reading %s: %w", rel, err)
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

	closed = true
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
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

func readBoundedEntry(r io.Reader, name string, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(r, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("archive entry %s exceeds maximum allowed size (%d bytes)", name, maxBytes)
	}
	return data, nil
}

func checkNoSymlinkComponents(root *os.Root, slashRel string) error {
	parts := strings.Split(slashRel, "/")
	var cur string
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if cur == "" {
			cur = part
		} else {
			cur = cur + "/" + part
		}
		info, err := root.Lstat(filepath.FromSlash(cur))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			return fmt.Errorf("security violation: cannot inspect destination %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("security violation: destination traverses symlink %s", cur)
		}
	}
	return nil
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
	var totalBytes int64
	entryCount := 0

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
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("security violation: unsupported archive entry type for %q", hdr.Name)
		}

		entryCount++
		if entryCount > MaxPackEntries {
			return nil, fmt.Errorf("archive exceeds maximum allowed file count (%d)", MaxPackEntries)
		}

		if strings.ContainsRune(hdr.Name, '\\') {
			return nil, fmt.Errorf("security violation: illegal path separator in archive: %q", hdr.Name)
		}
		clean := filepath.ToSlash(filepath.Clean(hdr.Name))
		if strings.HasPrefix(clean, "../") || clean == ".." || strings.HasPrefix(clean, "/") || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("security violation: illegal path in archive: %q", hdr.Name)
		}
		if hdr.Size > MaxPackEntryBytes {
			return nil, fmt.Errorf("archive entry %s exceeds maximum allowed size (%d bytes)", hdr.Name, MaxPackEntryBytes)
		}

		data, err := readBoundedEntry(tr, hdr.Name, MaxPackEntryBytes)
		if err != nil {
			return nil, err
		}
		totalBytes += int64(len(data))
		if totalBytes > MaxPackTotalBytes {
			return nil, fmt.Errorf("archive exceeds maximum total unpacked size (%d bytes)", MaxPackTotalBytes)
		}

		if clean == ManifestEntryName {
			if manifest != nil {
				return nil, errors.New("invalid package: duplicate manifest.json entry")
			}
			var m PackManifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("invalid manifest.json in package: %w", err)
			}
			manifest = &m
			continue
		}

		if _, dup := tarFiles[clean]; dup {
			return nil, fmt.Errorf("invalid package: duplicate archive entry %q", clean)
		}
		tarFiles[clean] = data
	}

	if manifest == nil {
		return nil, errors.New("invalid package: missing manifest.json")
	}
	if manifest.Format != PackFormatV1 {
		return nil, fmt.Errorf("unsupported package format: %s", manifest.Format)
	}

	// Build manifest index and verify every declared file is valid and present in archive
	metaByPath := make(map[string]PackFileMeta, len(manifest.Files))
	for _, meta := range manifest.Files {
		if strings.ContainsRune(meta.Path, '\\') {
			return nil, fmt.Errorf("security violation: illegal path in manifest: %q", meta.Path)
		}
		norm := filepath.ToSlash(filepath.Clean(meta.Path))
		if !isAllowedPackPath(norm) {
			return nil, fmt.Errorf("security violation: disallowed target path in manifest: %q", meta.Path)
		}
		if _, dup := metaByPath[norm]; dup {
			return nil, fmt.Errorf("invalid manifest: duplicate file entry %q", norm)
		}
		if _, ok := tarFiles[norm]; !ok {
			return nil, fmt.Errorf("archive missing declared file: %s", norm)
		}
		metaByPath[norm] = meta
	}

	// Open workspace-confined root handle to prevent symlink escape during checks and writes
	root, err := os.OpenRoot(w.Root)
	if err != nil {
		return nil, fmt.Errorf("opening workspace root: %w", err)
	}
	defer root.Close()

	// Sort file paths for deterministic validation and dry-run plans
	keys := make([]string, 0, len(tarFiles))
	for k := range tarFiles {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type pendingWrite struct {
		relDest   string
		removeRel string
		data      []byte
		noticeDir string
		noticeMsg string
	}
	var writes []pendingWrite
	writtenNotices := map[string]bool{}
	existingPluginDirs := map[string]bool{}

	for _, path := range keys {
		if pluginName, ok := extractPluginName(path); ok {
			if _, checked := existingPluginDirs[pluginName]; !checked {
				_, statErr := root.Lstat(filepath.FromSlash("workspace/plugins/" + pluginName))
				if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
					return nil, fmt.Errorf("inspecting plugin dir %s: %w", pluginName, statErr)
				}
				existingPluginDirs[pluginName] = statErr == nil
			}
		}
	}

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
		if !isAllowedPackPath(path) {
			return nil, fmt.Errorf("security violation: disallowed target path in archive: %q", path)
		}
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

		if err := scanSecrets(path, data); err != nil {
			return nil, err
		}

		if err := checkNoSymlinkComponents(root, path); err != nil {
			return nil, err
		}
		if err := checkNoSymlinkComponents(root, path+QuarantineSuffix); err != nil {
			return nil, err
		}

		osRel := filepath.FromSlash(path)
		info, statErr := root.Stat(osRel)
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspecting existing %s: %w", path, statErr)
		}
		if statErr == nil && info.IsDir() {
			return nil, fmt.Errorf("destination %s is a directory", path)
		}
		exists := statErr == nil
		identical := false
		action := "create"

		if exists {
			cur, readErr := root.ReadFile(osRel)
			if readErr != nil {
				return nil, fmt.Errorf("reading existing %s: %w", path, readErr)
			}
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

		pluginName, isPlugin := extractPluginName(path)
		if isPlugin {
			if existingPluginDirs[pluginName] && !opts.Force {
				// Never overwrite or inject code into an existing plugin directory without --force.
				action = "skip"
			} else if opts.QuarantinePlugins {
				action = "quarantine"
			}
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

		relDest := path
		var removeRel, noticeDir, noticeMsg string
		if action == "quarantine" {
			relDest = path + QuarantineSuffix
			if exists {
				removeRel = path
			}
			pDir := "workspace/plugins/" + pluginName
			if err := checkNoSymlinkComponents(root, pDir+"/"+QuarantineNotice); err != nil {
				return nil, err
			}
			if !writtenNotices[pDir] {
				writtenNotices[pDir] = true
				noticeDir = pDir
				noticeMsg = fmt.Sprintf(
					"# QUARANTINE NOTICE\n\nPlugin %s was imported from an external .evolvpack.\nReview plugin.js.quarantine and plugin.json.quarantine before enabling.\nTo enable: remove the %s suffix from quarantined files.\n",
					pluginName, QuarantineSuffix,
				)
			}
			report.Quarantined = append(report.Quarantined, path)
		} else if action == "create" {
			report.Created = append(report.Created, path)
		} else if action == "overwrite" {
			report.Overwritten = append(report.Overwritten, path)
		}

		writes = append(writes, pendingWrite{
			relDest:   relDest,
			removeRel: removeRel,
			data:      data,
			noticeDir: noticeDir,
			noticeMsg: noticeMsg,
		})
	}

	if opts.DryRun {
		return report, nil
	}

	// Pass 2: Execute all filesystem writes strictly through workspace-confined os.Root
	for _, pw := range writes {
		if pw.noticeDir != "" {
			osNoticeDir := filepath.FromSlash(pw.noticeDir)
			if err := root.MkdirAll(osNoticeDir, 0o755); err != nil {
				return nil, err
			}
			noticePath := filepath.FromSlash(pw.noticeDir + "/" + QuarantineNotice)
			if err := root.WriteFile(noticePath, []byte(pw.noticeMsg), 0o644); err != nil {
				return nil, err
			}
		}

		if pw.removeRel != "" {
			if err := root.Remove(filepath.FromSlash(pw.removeRel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}

		parentSlash := filepath.ToSlash(filepath.Dir(filepath.FromSlash(pw.relDest)))
		if parentSlash != "." && parentSlash != "" {
			if err := root.MkdirAll(filepath.FromSlash(parentSlash), 0o755); err != nil {
				return nil, err
			}
		}
		if err := root.WriteFile(filepath.FromSlash(pw.relDest), pw.data, 0o644); err != nil {
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

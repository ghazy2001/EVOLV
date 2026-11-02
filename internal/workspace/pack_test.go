package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestWorkspace(t *testing.T) *W {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	w, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestExportAndImportRoundTrip(t *testing.T) {
	ws1 := newTestWorkspace(t)

	// Add sample evolv assets
	compPath := filepath.Join(ws1.Dir(UIComponentsRel()), "clock.html")
	os.WriteFile(compPath, []byte("<div class='clock'>12:00</div>"), 0o644)

	toolPath := filepath.Join(ws1.Dir(ToolsDir), "calc.js")
	os.WriteFile(toolPath, []byte("function calc(a, b) { return a + b; }"), 0o644)

	skillPath := filepath.Join(ws1.Dir(SkillsDir), "refactor.md")
	os.WriteFile(skillPath, []byte("# Refactor Skill\nAlways test first."), 0o644)

	promptPath := filepath.Join(ws1.Dir(PromptsDir), "tone.md")
	os.WriteFile(promptPath, []byte("Speak warmly and directly."), 0o644)

	plugDir := filepath.Join(ws1.Dir(PluginsDir), "ticker")
	os.MkdirAll(plugDir, 0o755)
	os.WriteFile(filepath.Join(plugDir, "plugin.json"), []byte(`{"name":"ticker"}`), 0o644)
	os.WriteFile(filepath.Join(plugDir, "plugin.js"), []byte(`evolv.on("tick", function(){});`), 0o644)

	// 1. Export to memory buffer
	var buf bytes.Buffer
	manifest, err := ws1.Export(&buf, ExportOptions{
		Author:      "mahmoud",
		Description: "A test genome pack",
	})
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	if manifest.Format != PackFormatV1 {
		t.Fatalf("bad format: %s", manifest.Format)
	}
	if manifest.Author != "mahmoud" {
		t.Fatalf("bad author: %s", manifest.Author)
	}
	if len(manifest.Files) < 5 {
		t.Fatalf("expected at least 5 files, got %d", len(manifest.Files))
	}

	// 2. Import into a fresh workspace
	ws2 := newTestWorkspace(t)

	// Dry run first
	dryReport, err := ws2.Import(bytes.NewReader(buf.Bytes()), ImportOptions{
		DryRun:            true,
		QuarantinePlugins: true,
	})
	if err != nil {
		t.Fatalf("dry run import failed: %v", err)
	}
	if len(dryReport.Conflicts) < 5 {
		t.Fatalf("dry run conflicts count too low: %d", len(dryReport.Conflicts))
	}
	// Verify nothing was written during dry run
	if fileExists(filepath.Join(ws2.Dir(UIComponentsRel()), "clock.html")) {
		t.Fatal("dry run should not write files")
	}

	// Real import
	report, err := ws2.Import(bytes.NewReader(buf.Bytes()), ImportOptions{
		DryRun:            false,
		QuarantinePlugins: true,
	})
	if err != nil {
		t.Fatalf("real import failed: %v", err)
	}

	if len(report.Created) == 0 {
		t.Fatal("expected created files in import report")
	}

	// Verify imported file content matches
	importedClock, err := os.ReadFile(filepath.Join(ws2.Dir(UIComponentsRel()), "clock.html"))
	if err != nil || string(importedClock) != "<div class='clock'>12:00</div>" {
		t.Fatalf("clock component not restored correctly: %v", err)
	}

	// Verify plugin quarantine: all plugin files quarantined, no live plugin files written
	if !fileExists(filepath.Join(ws2.Dir(PluginsDir), "ticker", "plugin.json.quarantine")) {
		t.Fatal("plugin.json was not quarantined")
	}
	if !fileExists(filepath.Join(ws2.Dir(PluginsDir), "ticker", "plugin.js.quarantine")) {
		t.Fatal("plugin.js was not quarantined")
	}
	if fileExists(filepath.Join(ws2.Dir(PluginsDir), "ticker", "plugin.json")) {
		t.Fatal("live plugin.json must not exist when quarantined")
	}
	if fileExists(filepath.Join(ws2.Dir(PluginsDir), "ticker", "plugin.js")) {
		t.Fatal("live plugin.js must not exist when quarantined")
	}
	if !fileExists(filepath.Join(ws2.Dir(PluginsDir), "ticker", "QUARANTINE.md")) {
		t.Fatal("QUARANTINE.md notice missing")
	}
}

func TestExportSecretScanner(t *testing.T) {
	ws := newTestWorkspace(t)

	// Inject a fake OpenAI key into a component
	leakyComp := filepath.Join(ws.Dir(UIComponentsRel()), "leak.html")
	os.WriteFile(leakyComp, []byte("const key = 'sk-123456789012345678901234';"), 0o644)

	var buf bytes.Buffer
	_, err := ws.Export(&buf, ExportOptions{})
	if err == nil {
		t.Fatal("export should have failed on secret token")
	}
	if !strings.Contains(err.Error(), "detected credential") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestExportNeverPackagesConfig(t *testing.T) {
	ws := newTestWorkspace(t)

	// Set API keys in config
	ws.Config.APIKeys["openai"] = "super-secret"
	ws.SaveConfig()

	var buf bytes.Buffer
	manifest, err := ws.Export(&buf, ExportOptions{})
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	for _, f := range manifest.Files {
		if strings.Contains(f.Path, ConfigFile) {
			t.Fatalf("config file must never be packaged: %s", f.Path)
		}
	}
}

func TestExportFileAtomicFailure(t *testing.T) {
	ws := newTestWorkspace(t)

	// Inject secret to trigger export failure
	leakyComp := filepath.Join(ws.Dir(UIComponentsRel()), "leak.html")
	os.WriteFile(leakyComp, []byte("const token = 'ghp_123456789012345678901234567890';"), 0o644)

	destPath := filepath.Join(t.TempDir(), "failed-output.evolvpack")
	_, err := ws.ExportFile(destPath, ExportOptions{})
	if err == nil {
		t.Fatal("expected export failure on credential token")
	}

	// Verify destPath was NOT created or left as zero-byte file
	if fileExists(destPath) {
		t.Fatalf("destPath should not exist after export failure: %s", destPath)
	}
}

func TestScanSecretsCaseInsensitive(t *testing.T) {
	if err := scanSecrets("workspace/Config.JSON", []byte("{}")); err == nil {
		t.Fatal("expected rejection of Config.JSON regardless of case")
	}
	if err := scanSecrets("workspace/CONFIG.json", []byte("{}")); err == nil {
		t.Fatal("expected rejection of CONFIG.json regardless of case")
	}
}

func TestImportPreflightAtomicity(t *testing.T) {
	// Build a package manually with:
	// 1. good.html (declared and valid)
	// 2. bad.html (checksum mismatch)
	goodData := []byte("<div>Good</div>")
	goodSum := sha256.Sum256(goodData)

	badData := []byte("<div>Tampered</div>")

	manifest := PackManifest{
		Format:    PackFormatV1,
		Version:   "0.1.0",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Author:    "tester",
		Files: []PackFileMeta{
			{
				Path:   "workspace/ui/components/good.html",
				SHA256: hex.EncodeToString(goodSum[:]),
				Bytes:  int64(len(goodData)),
				Kind:   "component",
			},
			{
				Path:   "workspace/ui/components/bad.html",
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000", // invalid
				Bytes:  int64(len(badData)),
				Kind:   "component",
			},
		},
	}
	manifestRaw, _ := json.Marshal(manifest)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Write manifest.json
	_ = tw.WriteHeader(&tar.Header{
		Name: ManifestEntryName,
		Mode: 0o644,
		Size: int64(len(manifestRaw)),
	})
	_, _ = tw.Write(manifestRaw)

	// Write good.html
	_ = tw.WriteHeader(&tar.Header{
		Name: "workspace/ui/components/good.html",
		Mode: 0o644,
		Size: int64(len(goodData)),
	})
	_, _ = tw.Write(goodData)

	// Write bad.html
	_ = tw.WriteHeader(&tar.Header{
		Name: "workspace/ui/components/bad.html",
		Mode: 0o644,
		Size: int64(len(badData)),
	})
	_, _ = tw.Write(badData)

	_ = tw.Close()
	_ = gw.Close()

	// Attempt import into fresh workspace
	targetWS := newTestWorkspace(t)
	_, err := targetWS.Import(bytes.NewReader(buf.Bytes()), ImportOptions{})
	if err == nil {
		t.Fatal("expected import to fail due to checksum mismatch on bad.html")
	}

	// Verify that good.html was NEVER written to disk due to pre-flight check
	goodOnDisk := filepath.Join(targetWS.Dir(UIComponentsRel()), "good.html")
	if fileExists(goodOnDisk) {
		t.Fatalf("atomicity violation: good.html was written to disk despite package corruption: %s", goodOnDisk)
	}
}

func TestImportChecksumMismatch(t *testing.T) {
	ws := newTestWorkspace(t)

	// Test corrupt reader import fails cleanly
	_, err := ws.Import(bytes.NewReader([]byte("not a gzip file")), ImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "not a gzip archive") {
		t.Fatalf("expected gzip error, got: %v", err)
	}
}

func buildCustomPackBytes(t *testing.T, manifestFiles []PackFileMeta, tarEntries map[string][]byte) []byte {
	t.Helper()
	manifest := PackManifest{
		Format:    PackFormatV1,
		Version:   "0.1.0",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Author:    "tester",
		Files:     manifestFiles,
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	if err := tw.WriteHeader(&tar.Header{
		Name: ManifestEntryName,
		Mode: 0o644,
		Size: int64(len(manifestRaw)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(manifestRaw); err != nil {
		t.Fatal(err)
	}

	for name, data := range tarEntries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportRejectsDisallowedPaths(t *testing.T) {
	disallowed := []string{
		"Config.json",
		"Workspace/Plugins/evil/plugin.js",
		"snapshots/snap.json",
		"logs/app.log",
		"workspace/plugins/rootfile.js",
	}

	for _, badPath := range disallowed {
		payload := []byte("console.log('bad');")
		sum := sha256.Sum256(payload)
		packBytes := buildCustomPackBytes(t, []PackFileMeta{
			{
				Path:   badPath,
				SHA256: hex.EncodeToString(sum[:]),
				Bytes:  int64(len(payload)),
				Kind:   "other",
			},
		}, map[string][]byte{
			badPath: payload,
		})

		ws := newTestWorkspace(t)
		_, err := ws.Import(bytes.NewReader(packBytes), ImportOptions{Force: true})
		if err == nil {
			t.Fatalf("expected disallowed path %q to be rejected", badPath)
		}
		if !strings.Contains(err.Error(), "disallowed target path") && !strings.Contains(err.Error(), "refusing to pack") {
			t.Fatalf("unexpected error for %q: %v", badPath, err)
		}
	}
}

func TestImportMissingDeclaredFile(t *testing.T) {
	presentData := []byte("function ok() {}")
	presentSum := sha256.Sum256(presentData)
	missingData := []byte("function missing() {}")
	missingSum := sha256.Sum256(missingData)

	packBytes := buildCustomPackBytes(t, []PackFileMeta{
		{
			Path:   "workspace/tools/ok.js",
			SHA256: hex.EncodeToString(presentSum[:]),
			Bytes:  int64(len(presentData)),
			Kind:   "tool",
		},
		{
			Path:   "workspace/tools/missing.js",
			SHA256: hex.EncodeToString(missingSum[:]),
			Bytes:  int64(len(missingData)),
			Kind:   "tool",
		},
	}, map[string][]byte{
		"workspace/tools/ok.js": presentData,
	})

	ws := newTestWorkspace(t)
	_, err := ws.Import(bytes.NewReader(packBytes), ImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "archive missing declared file") {
		t.Fatalf("expected missing declared file error, got: %v", err)
	}
	if fileExists(filepath.Join(ws.Dir(ToolsDir), "ok.js")) {
		t.Fatal("ok.js must not be written when archive is missing a declared file")
	}
}

func TestImportSkipsExistingPluginWithoutForce(t *testing.T) {
	ws := newTestWorkspace(t)
	plugDir := filepath.Join(ws.Dir(PluginsDir), "ticker")
	if err := os.MkdirAll(plugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	origCode := []byte(`evolv.on("tick", function(){ return "local"; });`)
	if err := os.WriteFile(filepath.Join(plugDir, "plugin.json"), []byte(`{"name":"ticker"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugDir, "plugin.js"), origCode, 0o644); err != nil {
		t.Fatal(err)
	}

	newJSON := []byte(`{"name":"ticker","version":"2"}`)
	newJSONSum := sha256.Sum256(newJSON)
	newJS := []byte(`evolv.on("tick", function(){ return "remote"; });`)
	newJSSum := sha256.Sum256(newJS)
	extraJS := []byte(`console.log("injected");`)
	extraSum := sha256.Sum256(extraJS)

	packBytes := buildCustomPackBytes(t, []PackFileMeta{
		{Path: "workspace/plugins/ticker/plugin.json", SHA256: hex.EncodeToString(newJSONSum[:]), Bytes: int64(len(newJSON)), Kind: "plugin"},
		{Path: "workspace/plugins/ticker/plugin.js", SHA256: hex.EncodeToString(newJSSum[:]), Bytes: int64(len(newJS)), Kind: "plugin"},
		{Path: "workspace/plugins/ticker/extra.js", SHA256: hex.EncodeToString(extraSum[:]), Bytes: int64(len(extraJS)), Kind: "plugin"},
	}, map[string][]byte{
		"workspace/plugins/ticker/plugin.json": newJSON,
		"workspace/plugins/ticker/plugin.js":   newJS,
		"workspace/plugins/ticker/extra.js":    extraJS,
	})

	// Without Force, existing plugin directory must not be modified or partially injected
	report, err := ws.Import(bytes.NewReader(packBytes), ImportOptions{
		Force:             false,
		QuarantinePlugins: true,
	})
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if len(report.Skipped) != 3 {
		t.Fatalf("expected all 3 plugin files to be skipped without --force, got skipped=%v", report.Skipped)
	}
	gotJS, err := os.ReadFile(filepath.Join(plugDir, "plugin.js"))
	if err != nil || string(gotJS) != string(origCode) {
		t.Fatalf("existing plugin.js was modified without --force: %s", string(gotJS))
	}
	if fileExists(filepath.Join(plugDir, "extra.js")) || fileExists(filepath.Join(plugDir, "extra.js.quarantine")) {
		t.Fatal("extra plugin file must not be injected into existing plugin dir without --force")
	}

	// With Force + QuarantinePlugins, live plugin.json/plugin.js must be replaced by .quarantine files
	report, err = ws.Import(bytes.NewReader(packBytes), ImportOptions{
		Force:             true,
		QuarantinePlugins: true,
	})
	if err != nil {
		t.Fatalf("force quarantine import failed: %v", err)
	}
	if len(report.Quarantined) != 3 {
		t.Fatalf("expected all 3 plugin files quarantined with --force, got %v", report.Quarantined)
	}
	if fileExists(filepath.Join(plugDir, "plugin.js")) || fileExists(filepath.Join(plugDir, "plugin.json")) {
		t.Fatal("live plugin.js or plugin.json remained active after quarantined --force import")
	}
	if !fileExists(filepath.Join(plugDir, "plugin.js.quarantine")) || !fileExists(filepath.Join(plugDir, "plugin.json.quarantine")) {
		t.Fatal("expected quarantined plugin files on disk")
	}
}

func TestImportRejectsOversizedEntry(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	// Write a tar header claiming Size > MaxPackEntryBytes without needing to allocate 16 MiB+ in tests
	if err := tw.WriteHeader(&tar.Header{
		Name: "workspace/tools/huge.js",
		Mode: 0o644,
		Size: MaxPackEntryBytes + 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Close gzip directly (tar complains on short write, which is fine because Import checks hdr.Size before reading body)
	_ = gw.Close()

	ws := newTestWorkspace(t)
	_, err := ws.Import(bytes.NewReader(buf.Bytes()), ImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("expected oversized entry error, got: %v", err)
	}
}

func TestImportRejectsSymlinkEscape(t *testing.T) {
	ws := newTestWorkspace(t)
	outsideDir := t.TempDir()

	linkDir := filepath.Join(ws.Dir(ToolsDir), "escaped")
	if err := os.Symlink(outsideDir, linkDir); err != nil {
		t.Skipf("skipping symlink escape test (os.Symlink unavailable): %v", err)
	}

	payload := []byte("escaped payload")
	sum := sha256.Sum256(payload)
	targetRel := "workspace/tools/escaped/pwn.js"
	packBytes := buildCustomPackBytes(t, []PackFileMeta{
		{
			Path:   targetRel,
			SHA256: hex.EncodeToString(sum[:]),
			Bytes:  int64(len(payload)),
			Kind:   "tool",
		},
	}, map[string][]byte{
		targetRel: payload,
	})

	_, err := ws.Import(bytes.NewReader(packBytes), ImportOptions{Force: true})
	if err == nil || !strings.Contains(err.Error(), "security violation") {
		t.Fatalf("expected symlink escape security violation, got: %v", err)
	}
	if fileExists(filepath.Join(outsideDir, "pwn.js")) {
		t.Fatal("symlink escape wrote file outside workspace root")
	}
}

func TestScanSecretsWordBoundary(t *testing.T) {
	// Words containing "sk-" like task-management-workflow-plan must NOT be flagged as secrets
	safe := []byte("This is a task-management-workflow-plan with risk-assessment and disk-usage.")
	if err := scanSecrets("workspace/skills/task.md", safe); err != nil {
		t.Fatalf("scanSecrets flagged harmless word containing sk-: %v", err)
	}

	// Real sk- key must be flagged
	leaked := []byte("export OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwxyz1234567890")
	if err := scanSecrets("workspace/skills/leak.md", leaked); err == nil {
		t.Fatal("scanSecrets failed to catch real sk- key")
	}
}

func TestImportDestinationIsDirectory(t *testing.T) {
	ws := newTestWorkspace(t)
	// Create a directory at workspace/tools/conflict.js
	dirTarget := filepath.Join(ws.Dir(ToolsDir), "conflict.js")
	if err := os.MkdirAll(dirTarget, 0o755); err != nil {
		t.Fatal(err)
	}

	payload := []byte("content")
	sum := sha256.Sum256(payload)
	targetRel := "workspace/tools/conflict.js"
	packBytes := buildCustomPackBytes(t, []PackFileMeta{
		{
			Path:   targetRel,
			SHA256: hex.EncodeToString(sum[:]),
			Bytes:  int64(len(payload)),
			Kind:   "tool",
		},
	}, map[string][]byte{
		targetRel: payload,
	})

	_, err := ws.Import(bytes.NewReader(packBytes), ImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "destination workspace/tools/conflict.js is a directory") {
		t.Fatalf("expected error when destination is a directory, got: %v", err)
	}
}

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

	// Verify plugin quarantine
	if !fileExists(filepath.Join(ws2.Dir(PluginsDir), "ticker", "plugin.json.quarantine")) {
		t.Fatal("plugin.json was not quarantined")
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

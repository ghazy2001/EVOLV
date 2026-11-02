package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dxn111/EVOLV/internal/workspace"
)

func TestPackExport(t *testing.T) {
	s, _ := newWbServer(t)

	// Create a component so there is content to pack
	compPath := filepath.Join(s.ws.Dir(workspace.ComponentsDir), "test-clock.html")
	if err := os.WriteFile(compPath, []byte("<div>12:00</div>"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/pack/export?ledger=1&author=tester&desc=test-pack", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("export pack -> %d: %s", rec.Code, rec.Body.String())
	}

	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("content-type = %q, want application/octet-stream", got)
	}
	disp := rec.Header().Get("Content-Disposition")
	if !strings.Contains(disp, ".evolvpack") {
		t.Fatalf("content-disposition = %q, want .evolvpack", disp)
	}

	// Verify it is a valid gzip archive containing manifest.json
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("failed to open gzip: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	foundManifest, foundClock := false, false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read error: %v", err)
		}
		if hdr.Name == "manifest.json" {
			foundManifest = true
			var m workspace.PackManifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				t.Fatalf("invalid manifest json: %v", err)
			}
			if m.Author != "tester" || m.Description != "test-pack" {
				t.Fatalf("manifest author/desc mismatch: %+v", m)
			}
		}
		if strings.HasSuffix(hdr.Name, "test-clock.html") {
			foundClock = true
		}
	}
	if !foundManifest {
		t.Fatal("manifest.json not found in exported pack")
	}
	if !foundClock {
		t.Fatal("test-clock.html not found in exported pack")
	}
}

func TestPackInspectAndImport(t *testing.T) {
	s1, _ := newWbServer(t)
	compPath := filepath.Join(s1.ws.Dir(workspace.ComponentsDir), "timer.html")
	if err := os.WriteFile(compPath, []byte("<div>timer</div>"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Export from server 1
	rec := httptest.NewRecorder()
	s1.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/pack/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export -> %d", rec.Code)
	}
	packBytes := rec.Body.Bytes()

	s2, _ := newWbServer(t)

	// Inspect with raw body
	inspectReq := httptest.NewRequest(http.MethodPost, "/api/pack/inspect", bytes.NewReader(packBytes))
	inspectRec := httptest.NewRecorder()
	s2.Handler().ServeHTTP(inspectRec, inspectReq)

	if inspectRec.Code != http.StatusOK {
		t.Fatalf("inspect -> %d: %s", inspectRec.Code, inspectRec.Body.String())
	}
	var report workspace.ImportReport
	if err := json.Unmarshal(inspectRec.Body.Bytes(), &report); err != nil {
		t.Fatalf("invalid inspect report: %v", err)
	}
	if report.Manifest == nil {
		t.Fatal("missing manifest in inspect report")
	}
	// Verify dry-run: timer.html must not be on disk yet in server 2
	destTimer := filepath.Join(s2.ws.Dir(workspace.ComponentsDir), "timer.html")
	if _, err := os.Stat(destTimer); !os.IsNotExist(err) {
		t.Fatal("dry-run inspect wrote file to disk")
	}

	// Import via multipart form
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("pack", "test.evolvpack")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(packBytes)
	writer.Close()

	importReq := httptest.NewRequest(http.MethodPost, "/api/pack/import?force=1", &body)
	importReq.Header.Set("Content-Type", writer.FormDataContentType())
	importRec := httptest.NewRecorder()
	s2.Handler().ServeHTTP(importRec, importReq)

	if importRec.Code != http.StatusOK {
		t.Fatalf("import -> %d: %s", importRec.Code, importRec.Body.String())
	}

	var importReport workspace.ImportReport
	if err := json.Unmarshal(importRec.Body.Bytes(), &importReport); err != nil {
		t.Fatalf("invalid import report: %v", err)
	}
	if len(importReport.Created) == 0 && len(importReport.Overwritten) == 0 {
		t.Fatalf("expected created files, got %+v", importReport)
	}

	// Verify file is now on disk
	importedContent, err := os.ReadFile(destTimer)
	if err != nil {
		t.Fatalf("file was not imported: %v", err)
	}
	if string(importedContent) != "<div>timer</div>" {
		t.Fatalf("content mismatch: %s", string(importedContent))
	}
}

func TestPackInvalidInput(t *testing.T) {
	s, _ := newWbServer(t)

	// Send corrupted bytes
	req := httptest.NewRequest(http.MethodPost, "/api/pack/inspect", strings.NewReader("not a gzip pack"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for corrupt pack, got %d", rec.Code)
	}
}

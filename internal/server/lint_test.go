package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dxn111/EVOLV/internal/lint"
	"github.com/dxn111/EVOLV/internal/workspace"
)

func TestServerLintEndpoint(t *testing.T) {
	s, _ := newWbServer(t)

	// Clean component
	cleanReqBody, _ := json.Marshal(map[string]string{
		"type":    "component",
		"name":    "clean_card",
		"content": `<div class="card">Hello</div>`,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/lint", bytes.NewReader(cleanReqBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("clean lint -> %d: %s", rec.Code, rec.Body.String())
	}
	var rep lint.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !rep.Valid || rep.ErrorsCount != 0 {
		t.Fatalf("expected clean report, got %+v", rep)
	}

	// Insecure component
	insecureReqBody, _ := json.Marshal(map[string]string{
		"type":    "component",
		"name":    "bad_card",
		"content": `<div class="card"></div><script src="https://evil.com/leak.js"></script>`,
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/lint", bytes.NewReader(insecureReqBody))
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("insecure lint -> %d: %s", rec2.Code, rec2.Body.String())
	}
	var rep2 lint.Report
	if err := json.Unmarshal(rec2.Body.Bytes(), &rep2); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if rep2.Valid || rep2.ErrorsCount != 1 {
		t.Fatalf("expected 1 error, got %+v", rep2)
	}

	// Method not allowed
	getReq := httptest.NewRequest(http.MethodGet, "/api/lint", nil)
	getRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", getRec.Code)
	}
}

func TestCreationInventorySecurityStatus(t *testing.T) {
	s, _ := newWbServer(t)

	// Write clean widget
	cleanPath := filepath.Join(s.ws.Dir(workspace.ComponentsDir), "clean.html")
	if err := os.WriteFile(cleanPath, []byte("<div>clean widget</div>"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write insecure widget
	insecurePath := filepath.Join(s.ws.Dir(workspace.ComponentsDir), "insecure.html")
	if err := os.WriteFile(insecurePath, []byte("<div><script src='https://cdn.com/bad.js'></script></div>"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/creations", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/creations -> %d", rec.Code)
	}

	var data struct {
		Creations []creationInfo `json:"creations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	var foundClean, foundInsecure bool
	for _, c := range data.Creations {
		if c.Name == "clean" {
			foundClean = true
			if c.SecurityStatus != "clean" {
				t.Fatalf("expected clean widget security_status to be 'clean', got %q", c.SecurityStatus)
			}
		}
		if c.Name == "insecure" {
			foundInsecure = true
			if c.SecurityStatus != "error" {
				t.Fatalf("expected insecure widget security_status to be 'error', got %q", c.SecurityStatus)
			}
		}
	}
	if !foundClean || !foundInsecure {
		t.Fatalf("missing creations: foundClean=%v, foundInsecure=%v", foundClean, foundInsecure)
	}
}

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dxn111/EVOLV/internal/bus"
	"github.com/dxn111/EVOLV/internal/workspace"
)

func TestServerDeployEndpointsAndPreview(t *testing.T) {
	tmpDir := t.TempDir()
	ws := &workspace.W{Root: tmpDir, Config: &workspace.Config{}}
	b := bus.New()
	s := New(ws, b, nil, nil)

	// 1. POST /api/deploy
	body := `{"component":"calculator","target":"local_preview"}`
	req := httptest.NewRequest(http.MethodPost, "/api/deploy", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/deploy failed: %d: %s", rec.Code, rec.Body.String())
	}

	var depResp struct {
		OK         bool `json:"ok"`
		Deployment struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"deployment"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&depResp); err != nil {
		t.Fatalf("decode deploy resp: %v", err)
	}
	if !depResp.OK || depResp.Deployment.ID == "" {
		t.Fatalf("unexpected deploy response: %+v", depResp)
	}

	// 2. GET /api/deploy/list - must contain the created deployment
	reqList := httptest.NewRequest(http.MethodGet, "/api/deploy/list", nil)
	recList := httptest.NewRecorder()
	s.Handler().ServeHTTP(recList, reqList)

	if recList.Code != http.StatusOK {
		t.Fatalf("GET /api/deploy/list failed: %d", recList.Code)
	}
	var listResp struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if err := json.NewDecoder(recList.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list resp: %v", err)
	}
	if listResp.Count != 1 {
		t.Errorf("expected 1 deployment in list, got %d", listResp.Count)
	}

	// 3. GET /deploy/<id>/index.html - preview URL must be served
	reqPreview := httptest.NewRequest(http.MethodGet, depResp.Deployment.URL, nil)
	recPreview := httptest.NewRecorder()
	s.Handler().ServeHTTP(recPreview, reqPreview)

	if recPreview.Code != http.StatusOK {
		t.Fatalf("GET %s failed: %d", depResp.Deployment.URL, recPreview.Code)
	}
	if !strings.Contains(recPreview.Body.String(), "calculator") {
		t.Errorf("preview HTML missing component name: %s", recPreview.Body.String())
	}
}

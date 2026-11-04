package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

func createTestPNG(w, h int) string {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{18, 23, 21, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestCanvasScreenshotEndpoints(t *testing.T) {
	s, _ := newWbServer(t)

	// 1. POST screenshot
	postBody, _ := json.Marshal(map[string]string{
		"target": "pomodoro",
		"data":   createTestPNG(320, 240),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/canvas/screenshot", bytes.NewReader(postBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST screenshot expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if res["ok"] != true || res["target"] != "pomodoro" {
		t.Fatalf("unexpected POST response: %+v", res)
	}

	// 2. GET screenshot by target
	getReq := httptest.NewRequest(http.MethodGet, "/api/canvas/screenshot?target=pomodoro", nil)
	getRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("GET screenshot expected 200, got %d", getRec.Code)
	}
	if ct := getRec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("expected Content-Type image/png, got %s", ct)
	}

	// 3. GET list of screenshots
	listReq := httptest.NewRequest(http.MethodGet, "/api/canvas/screenshots", nil)
	listRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("GET screenshots list expected 200, got %d", listRec.Code)
	}
	var listRes map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &listRes); err != nil {
		t.Fatalf("invalid list json: %v", err)
	}
	if listRes["ok"] != true {
		t.Fatalf("expected ok=true, got %+v", listRes)
	}
	shots, ok := listRes["screenshots"].([]any)
	if !ok || len(shots) == 0 {
		t.Fatalf("expected at least 1 screenshot in list, got %+v", listRes)
	}

	// 4. Path escape check
	escapeReq := httptest.NewRequest(http.MethodGet, "/api/canvas/screenshot?path=../../outside.png", nil)
	escapeRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(escapeRec, escapeReq)
	if escapeRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 forbidden on path escape, got %d", escapeRec.Code)
	}

	// 5. Check path containment strictly within screenshots directory (e.g. config.json in root must be forbidden)
	escapeRootReq := httptest.NewRequest(http.MethodGet, "/api/canvas/screenshot?path=config.json", nil)
	escapeRootRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(escapeRootRec, escapeRootReq)
	if escapeRootRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 forbidden for path outside screenshots dir (config.json), got %d", escapeRootRec.Code)
	}
}

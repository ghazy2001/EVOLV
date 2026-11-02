package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dxn111/EVOLV/internal/bus"
)

func TestSwarmRoute(t *testing.T) {
	s, _ := newWbServer(t)
	page := filepath.Join(s.ws.Root, "workspace/ui/swarm.html")
	if err := os.WriteFile(page, []byte("<!doctype html><h1>swarm mode</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/swarm", "/swarm/", "/swarm.html"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d: %s", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Fatalf("GET %s content-type = %q", path, got)
		}
		if !strings.Contains(strings.ToLower(rec.Body.String()), "swarm mode") {
			t.Fatalf("GET %s body = %q", path, rec.Body.String())
		}
	}
}

func TestSwarmAPIStartStatusStop(t *testing.T) {
	s, _ := newWbServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/swarm", strings.NewReader(`{"mission":"build a tiny timer"}`))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start swarm -> %d: %s", rec.Code, rec.Body.String())
	}
	var start map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if start["status"] != "running" || start["mission"] != "build a tiny timer" {
		t.Fatalf("bad start state: %v", start)
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/swarm", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get swarm -> %d", rec.Code)
	}
	var current map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current["id"] != start["id"] {
		t.Fatalf("swarm id changed: %v vs %v", current["id"], start["id"])
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/swarm/stop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stop swarm -> %d", rec.Code)
	}
	// Let the run goroutine observe cancellation and finish its persistence
	// write before TempDir cleanup removes the workspace.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && s.swarm.Snapshot().Status == "running" {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
}

func TestSwarmAPIRejectsEmptyMission(t *testing.T) {
	s, _ := newWbServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/swarm", strings.NewReader(`{"mission":"  "}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty mission -> %d", rec.Code)
	}
}

func TestSwarmStatePersists(t *testing.T) {
	s, _ := newWbServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/swarm", strings.NewReader(`{"mission":"persist this colony","agents":10}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start -> %d", rec.Code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && s.swarm.Snapshot().Status == "running" {
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.swarm.Snapshot().Status; got != "done" {
		t.Fatalf("run did not finish: %s", got)
	}
	// The state publish callback persists last.json and the archive after the
	// status flips; give it time to finish before TempDir cleanup.
	time.Sleep(100 * time.Millisecond)

	// A fresh server pointed at the same genome must restore the last run.
	reloaded := New(s.ws, bus.New(), nil, nil)
	st := reloaded.swarm.Snapshot()
	if st.ID == "" || st.Mission != "persist this colony" || len(st.Messages) == 0 {
		t.Fatalf("restored state = %+v", st)
	}
	if len(st.Agents) != 10 {
		t.Fatalf("restored agents = %d", len(st.Agents))
	}
}

func TestSwarmRunHistory(t *testing.T) {
	s, _ := newWbServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/swarm", strings.NewReader(`{"mission":"archive this run","agents":5}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start -> %d", rec.Code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && s.swarm.Snapshot().Status == "running" {
		time.Sleep(20 * time.Millisecond)
	}
	id := s.swarm.Snapshot().ID
	time.Sleep(30 * time.Millisecond)

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/swarm/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("runs -> %d", rec.Code)
	}
	var list struct {
		Runs []swarmRunSummary `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) == 0 || list.Runs[0].ID != id {
		t.Fatalf("runs = %+v, want %s", list.Runs, id)
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/swarm/history?id="+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("history -> %d: %s", rec.Code, rec.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["id"] != id || st["status"] != "done" {
		t.Fatalf("history state = %v", st)
	}
}

func TestSwarmSteerEndpoint(t *testing.T) {
	s, _ := newWbServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/swarm", strings.NewReader(`{"mission":"steer me","agents":5}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start -> %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/swarm/steer", strings.NewReader(`{"message":"keep it keyboard-first"}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("steer -> %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/swarm/stop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stop -> %d", rec.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && s.swarm.Snapshot().Status == "running" {
		time.Sleep(10 * time.Millisecond)
	}
	found := false
	for _, m := range s.swarm.Snapshot().Messages {
		if m.Kind == "steer" && m.Agent == "human" {
			found = true
		}
	}
	if !found {
		t.Fatal("human steer not persisted in transcript")
	}
}

func TestSwarmApplyComponent(t *testing.T) {
	s, _ := newWbServer(t)

	// Start a swarm run and let it produce artifacts
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/swarm", strings.NewReader(`{"mission":"build a focus timer","agents":5}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start -> %d", rec.Code)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && s.swarm.Snapshot().Status == "running" {
		time.Sleep(20 * time.Millisecond)
	}
	if s.swarm.Snapshot().Status != "done" {
		t.Fatalf("swarm run did not complete: %s", s.swarm.Snapshot().Status)
	}

	artifacts := s.swarm.Snapshot().Artifacts
	if len(artifacts) == 0 {
		t.Fatal("no artifacts produced by swarm run")
	}

	// Apply the first artifact as a component
	art := artifacts[0]
	applyReq := httptest.NewRequest(http.MethodPost, "/api/swarm/apply", strings.NewReader(`{"artifact":"`+art.Name+`","target":"component","name":"focus-timer"}`))
	applyRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(applyRec, applyReq)

	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply component -> %d: %s", applyRec.Code, applyRec.Body.String())
	}

	var res map[string]any
	if err := json.Unmarshal(applyRec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res["ok"] != true || res["name"] != "focus-timer" {
		t.Fatalf("unexpected res: %v", res)
	}

	dest := filepath.Join(s.ws.Dir("workspace/ui/components"), "focus-timer.html")
	content, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("component file not written: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("component file is empty")
	}

	// Verify evolution was logged
	evolutions := s.ws.ReadEvolution(10)
	foundEvo := false
	for _, e := range evolutions {
		if e["kind"] == "component" && e["name"] == "focus-timer" && e["source"] == "swarm" {
			foundEvo = true
			break
		}
	}
	if !foundEvo {
		t.Fatal("evolution entry for component not found in ledger")
	}
}

func TestSwarmApplyProjectAndSecurity(t *testing.T) {
	s, proj := newWbServer(t)

	// Direct content application as a project file
	applyReq := httptest.NewRequest(http.MethodPost, "/api/swarm/apply", strings.NewReader(`{"content":"package main\n\nfunc main() {}","target":"project","path":"sub/main.go"}`))
	applyRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(applyRec, applyReq)

	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply project -> %d: %s", applyRec.Code, applyRec.Body.String())
	}

	written, err := os.ReadFile(filepath.Join(proj, "sub", "main.go"))
	if err != nil {
		t.Fatalf("project file not written: %v", err)
	}
	if !strings.Contains(string(written), "package main") {
		t.Fatalf("unexpected content: %s", string(written))
	}

	// Security: path escape attempts must be rejected
	badPaths := []string{"../escape.txt", "../../evil.sh", "/abs/path.txt", `..\windows\bad.txt`}
	for _, p := range badPaths {
		badReq := httptest.NewRequest(http.MethodPost, "/api/swarm/apply", strings.NewReader(`{"content":"evil","target":"project","path":"`+p+`"}`))
		badRec := httptest.NewRecorder()
		s.Handler().ServeHTTP(badRec, badReq)
		if badRec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for path %q, got %d", p, badRec.Code)
		}
	}

	// Nonexistent artifact should return 404
	missingReq := httptest.NewRequest(http.MethodPost, "/api/swarm/apply", strings.NewReader(`{"artifact":"does-not-exist.txt","target":"component"}`))
	missingRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing artifact, got %d", missingRec.Code)
	}
}

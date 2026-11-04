package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dxn111/EVOLV/internal/bus"
	"github.com/dxn111/EVOLV/internal/providers"
	"github.com/dxn111/EVOLV/internal/workspace"
)

func TestVisionManagerRegisterAndResolve(t *testing.T) {
	vm := NewVisionManager()
	reqID := "req-1"
	ch := vm.Register(reqID)

	expected := &CaptureResult{
		OK:     true,
		Target: "clock",
		Path:   "workspace/screenshots/clock.png",
		Width:  800,
		Height: 600,
	}

	ok := vm.Resolve(reqID, expected)
	if !ok {
		t.Fatalf("expected Resolve to succeed for %s", reqID)
	}

	select {
	case res := <-ch:
		if res.Target != "clock" || res.Width != 800 {
			t.Fatalf("unexpected result: %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for resolve channel")
	}

	// second resolve should fail
	if vm.Resolve(reqID, expected) {
		t.Fatal("expected second resolve to return false")
	}
}

func TestSynthesizeVisualSnapshot(t *testing.T) {
	data := synthesizeVisualSnapshot("test-organ", "Test Organ", 400, 300, "note")
	if len(data) == 0 {
		t.Fatal("expected non-empty png bytes")
	}

	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to decode synthesized png: %v", err)
	}
	if cfg.Width != 400 || cfg.Height != 300 {
		t.Fatalf("expected 400x300, got %dx%d", cfg.Width, cfg.Height)
	}
}

func TestSaveScreenshot(t *testing.T) {
	td := t.TempDir()
	ws := &workspace.W{Root: td}

	data := synthesizeVisualSnapshot("pomodoro", "Pomodoro Timer", 640, 480, "")
	res, err := SaveScreenshot(ws, "pomodoro", data, "", "test")
	if err != nil {
		t.Fatalf("SaveScreenshot failed: %v", err)
	}

	if !res.OK || res.Target != "pomodoro" || res.Width != 640 || res.Height != 480 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Verify file on disk
	latest := filepath.Join(td, "workspace", "screenshots", "pomodoro.png")
	if _, err := os.Stat(latest); err != nil {
		t.Fatalf("expected latest screenshot at %s: %v", latest, err)
	}
}

func TestCaptureCanvasFallback(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)
	t.Setenv("USERPROFILE", td)
	ws, err := workspace.Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	b := bus.New()
	reg := providers.NewRegistry()
	prov, model := providers.Build("mock", "", "", nil)
	reg.Add(prov)

	a := New(ws, b, prov, model)

	res, err := a.CaptureCanvas(context.Background(), "flappy", "")
	if err != nil {
		t.Fatalf("CaptureCanvas failed: %v", err)
	}

	if !res.OK {
		t.Fatalf("expected OK result, got %+v", res)
	}
	if res.Target != "flappy" {
		t.Fatalf("expected target 'flappy', got '%s'", res.Target)
	}
	if res.Source != "layout_synthesizer" {
		t.Fatalf("expected source 'layout_synthesizer', got '%s'", res.Source)
	}
}

func TestActionCaptureCanvas(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)
	t.Setenv("USERPROFILE", td)
	ws, err := workspace.Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	b := bus.New()
	prov, model := providers.Build("mock", "", "", nil)
	a := New(ws, b, prov, model)

	outJSON := a.Execute(action{
		Action: "capture_canvas",
		Target: "clock",
	})
	raw := strings.TrimPrefix(outJSON, "kernel:")

	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("failed to parse execute output: %v (%s)", err, outJSON)
	}

	if out["ok"] != true {
		t.Fatalf("expected ok=true, got: %s", outJSON)
	}
	if out["target"] != "clock" {
		t.Fatalf("expected target=clock, got %v", out["target"])
	}
	if out["path"] != "workspace/screenshots/clock.png" {
		t.Fatalf("expected path='workspace/screenshots/clock.png', got %v", out["path"])
	}
	if imgs := a.PopPendingImages(); len(imgs) != 1 || !strings.HasPrefix(imgs[0], "data:image/png;base64,") {
		t.Fatalf("expected 1 attached image in agent pendingImages, got %d", len(imgs))
	}
	if _, ok := out["data_url"]; ok {
		t.Fatalf("data_url should not be in tool output json to keep context clean; got %v", out["data_url"])
	}
}

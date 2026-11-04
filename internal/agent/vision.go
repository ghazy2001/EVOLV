// Package agent vision implements canvas and organ visual screenshots for EVOLV.
// It allows the agent (and the user) to visually inspect rendered components,
// verify UI layout, critique styling, and feed visual state back to multimodal models.
package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dxn111/EVOLV/internal/workspace"
)

// CaptureResult is the structured visual outcome of a canvas or organ screenshot.
type CaptureResult struct {
	OK        bool   `json:"ok"`
	Target    string `json:"target"`
	Path      string `json:"path"`
	AbsPath   string `json:"abs_path,omitempty"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	SizeBytes int64  `json:"size_bytes"`
	Source    string `json:"source"` // browser_live | layout_synthesizer
	DataURL   string `json:"data_url,omitempty"`
	Summary   string `json:"summary"`
	Error     string `json:"error,omitempty"`
}

// VisionManager manages pending visual capture requests between agent turns and
// connected browser sessions.
type VisionManager struct {
	mu      sync.Mutex
	pending map[string]chan *CaptureResult
}

// NewVisionManager creates a new vision manager.
func NewVisionManager() *VisionManager {
	return &VisionManager{
		pending: make(map[string]chan *CaptureResult),
	}
}

// Register registers a request ID and returns a receiving channel.
func (vm *VisionManager) Register(id string) chan *CaptureResult {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	ch := make(chan *CaptureResult, 1)
	vm.pending[id] = ch
	return ch
}

// Unregister removes a pending request channel.
func (vm *VisionManager) Unregister(id string) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	delete(vm.pending, id)
}

// Resolve fulfills a pending capture request with a result.
func (vm *VisionManager) Resolve(id string, res *CaptureResult) bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	ch, ok := vm.pending[id]
	if !ok {
		return false
	}
	select {
	case ch <- res:
	default:
	}
	delete(vm.pending, id)
	return true
}

// SaveScreenshot saves raw PNG bytes to workspace/screenshots/ and creates both a
// timestamped version and a latest `<target>.png` version.
func SaveScreenshot(ws *workspace.W, target string, rawBytes []byte, customPath string, source string) (*CaptureResult, error) {
	target = sanitizeName(target)
	if target == "" {
		target = "canvas"
	}
	screensDir := ws.Dir(workspace.ScreenshotsDir)
	if err := os.MkdirAll(screensDir, 0o755); err != nil {
		return nil, fmt.Errorf("create screenshots dir: %w", err)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(rawBytes))
	w, h := cfg.Width, cfg.Height
	if err != nil || w <= 0 || h <= 0 {
		w, h = 800, 600
	}

	ts := time.Now().Format("20060102-150405")
	fileName := fmt.Sprintf("%s-%s.png", target, ts)
	absPath := filepath.Join(screensDir, fileName)
	if customPath != "" {
		cleanRel := filepath.Clean(filepath.FromSlash(customPath))
		candidate := filepath.Join(screensDir, filepath.Base(cleanRel))
		if err := workspace.CheckWithin(screensDir, candidate); err == nil {
			absPath = candidate
		}
	}

	if err := os.WriteFile(absPath, rawBytes, 0o644); err != nil {
		return nil, fmt.Errorf("write screenshot: %w", err)
	}

	// Also write/update latest alias: workspace/screenshots/<target>.png
	latestPath := filepath.Join(screensDir, target+".png")
	_ = os.WriteFile(latestPath, rawBytes, 0o644)

	relPath := filepath.ToSlash(filepath.Join(workspace.ScreenshotsDir, target+".png"))
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(rawBytes)

	if source == "" {
		source = "browser_live"
	}

	summary := fmt.Sprintf("Captured %dx%d visual screenshot of '%s' (%d KB)", w, h, target, len(rawBytes)/1024)
	if source == "layout_synthesizer" {
		summary = fmt.Sprintf("Generated %dx%d layout placeholder for '%s' (%d KB) (synthetic snapshot: headless environment without live browser window)", w, h, target, len(rawBytes)/1024)
	}

	res := &CaptureResult{
		OK:        true,
		Target:    target,
		Path:      relPath,
		AbsPath:   absPath,
		Width:     w,
		Height:    h,
		SizeBytes: int64(len(rawBytes)),
		Source:    source,
		DataURL:   dataURL,
		Summary:   summary,
	}
	return res, nil
}

// DecodeBase64Image decodes a data URL or raw base64 string into raw image bytes.
func DecodeBase64Image(data string) ([]byte, error) {
	if idx := strings.Index(data, ","); idx != -1 && strings.HasPrefix(data, "data:") {
		data = data[idx+1:]
	}
	data = strings.TrimSpace(data)
	return base64.StdEncoding.DecodeString(data)
}

// synthesizeVisualSnapshot creates a valid, stylized dark-theme PNG layout card
// when no live browser tab is connected.
func synthesizeVisualSnapshot(target, title string, w, h int, note string) []byte {
	if w <= 0 {
		w = 800
	}
	if h <= 0 {
		h = 600
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	// Colors matching EVOLV's dark theme
	bg := color.RGBA{18, 23, 21, 255}        // #121715
	cardBg := color.RGBA{22, 28, 26, 255}    // #161c1a
	border := color.RGBA{39, 56, 49, 255}    // #273831
	accent := color.RGBA{201, 239, 114, 255} // #c9ef72
	muted := color.RGBA{140, 163, 152, 255}  // #8ca398

	// Background
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)

	// Inner card
	innerRect := image.Rect(20, 20, w-20, h-20)
	draw.Draw(img, innerRect, &image.Uniform{cardBg}, image.Point{}, draw.Src)

	// Border
	strokeRect(img, innerRect, border, 2)

	// Header bar inside card
	headRect := image.Rect(20, 20, w-20, 64)
	headBg := color.RGBA{26, 35, 32, 255}
	draw.Draw(img, headRect, &image.Uniform{headBg}, image.Point{}, draw.Src)
	strokeRect(img, headRect, border, 1)

	// Draw indicator dots
	drawDot(img, 40, 42, 6, accent)
	drawDot(img, 58, 42, 4, muted)

	// Fill middle body grid lines
	for y := 90; y < h-40; y += 40 {
		drawHLine(img, 40, w-40, y, color.RGBA{28, 38, 34, 255})
	}
	for x := 60; x < w-40; x += 60 {
		drawVLine(img, 90, h-40, x, color.RGBA{28, 38, 34, 255})
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func strokeRect(img *image.RGBA, r image.Rectangle, c color.Color, width int) {
	for w := 0; w < width; w++ {
		drawHLine(img, r.Min.X, r.Max.X, r.Min.Y+w, c)
		drawHLine(img, r.Min.X, r.Max.X, r.Max.Y-1-w, c)
		drawVLine(img, r.Min.Y, r.Max.Y, r.Min.X+w, c)
		drawVLine(img, r.Min.Y, r.Max.Y, r.Max.X-1-w, c)
	}
}

func drawHLine(img *image.RGBA, x1, x2, y int, c color.Color) {
	if y < img.Bounds().Min.Y || y >= img.Bounds().Max.Y {
		return
	}
	for x := x1; x < x2; x++ {
		if x >= img.Bounds().Min.X && x < img.Bounds().Max.X {
			img.Set(x, y, c)
		}
	}
}

func drawVLine(img *image.RGBA, y1, y2, x int, c color.Color) {
	if x < img.Bounds().Min.X || x >= img.Bounds().Max.X {
		return
	}
	for y := y1; y < y2; y++ {
		if y >= img.Bounds().Min.Y && y < img.Bounds().Max.Y {
			img.Set(x, y, c)
		}
	}
}

func drawDot(img *image.RGBA, cx, cy, radius int, c color.Color) {
	r2 := radius * radius
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			if dx*dx+dy*dy <= r2 {
				img.Set(cx+dx, cy+dy, c)
			}
		}
	}
}

// CaptureCanvas captures a visual screenshot of an organ or the whole canvas.
// If a browser is connected, it requests an instant live client-side snapshot.
// If no browser responds within the timeout, it falls back to a clean layout synthesizer.
func (a *Agent) CaptureCanvas(ctx context.Context, target string, savePath string) (*CaptureResult, error) {
	target = sanitizeName(target)
	if target == "" {
		target = "canvas"
	}

	if a.bus != nil && a.visionMgr != nil {
		reqID := fmt.Sprintf("vis-%d", time.Now().UnixNano())
		ch := a.visionMgr.Register(reqID)
		defer a.visionMgr.Unregister(reqID)

		// Ask connected browsers over the bus
		a.bus.Publish("canvas:request_capture", map[string]any{
			"id":        reqID,
			"target":    target,
			"timestamp": time.Now().UnixMilli(),
		})

		// Wait up to 2.5s for live browser capture
		select {
		case res := <-ch:
			if res != nil && res.OK {
				return res, nil
			}
		case <-time.After(2500 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Fallback: Synthesize layout snapshot
	title := target
	note := "synthetic visual layout snapshot"
	rawBytes := synthesizeVisualSnapshot(target, title, 800, 600, note)
	return SaveScreenshot(a.ws, target, rawBytes, savePath, "layout_synthesizer")
}

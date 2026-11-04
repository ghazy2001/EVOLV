// evolv — the self-evolving AI workspace.
// One command. Browser opens. The organism boots and starts mutating.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/dxn111/EVOLV/assets"
	"github.com/dxn111/EVOLV/internal/agent"
	"github.com/dxn111/EVOLV/internal/bus"
	"github.com/dxn111/EVOLV/internal/providers"
	"github.com/dxn111/EVOLV/internal/server"
	"github.com/dxn111/EVOLV/internal/watcher"
	"github.com/dxn111/EVOLV/internal/workspace"
)

// ── colour ──────────────────────────────────────────────────────────────────
// Everything degrades to plain text when piped, in a dumb terminal, or when
// NO_COLOR is set — so `evolv | tee log` still looks like a normal program.
var colorOn = os.Getenv("NO_COLOR") == "" &&
	os.Getenv("TERM") != "dumb" &&
	isTTY(os.Stdout)

const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	violet = "\033[38;5;141m"
	cyan   = "\033[38;5;80m"
	pink   = "\033[38;5;213m"
	green  = "\033[38;5;114m"
	yellow = "\033[38;5;221m"
	red    = "\033[38;5;203m"
)

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func paint(code, s string) string {
	if !colorOn {
		return s
	}
	return code + s + reset
}

// banner is the organism mark — same gradient the installer shows, so the
// install and the first boot feel like one continuous thing.
func banner() {
	rows := []struct{ color, line string }{
		{violet, `  ███████╗██╗   ██╗ ██████╗ ██╗    ██╗   ██╗`},
		{violet, `  ██╔════╝██║   ██║██╔═══██╗██║    ██║   ██║`},
		{cyan, `  █████╗  ██║   ██║██║   ██║██║    ██║   ██║`},
		{cyan, `  ██╔══╝  ╚██╗ ██╔╝██║   ██║██║    ██║   ██║`},
		{pink, `  ███████╗ ╚████╔╝ ╚██████╔╝███████╗╚██████╔╝`},
		{pink, `  ╚══════╝  ╚═══╝   ╚═════╝ ╚══════╝ ╚═════╝`},
	}
	fmt.Println()
	for _, r := range rows {
		fmt.Println(paint(r.color, r.line))
		if colorOn {
			time.Sleep(30 * time.Millisecond)
		}
	}
	fmt.Println()
	fmt.Println(paint(dim, "  the workspace that rewrites itself"))
	fmt.Println("  " + paint(dim, fmt.Sprintf("v%s  ·  %s/%s  ·  github.com/dxn111/EVOLV",
		workspace.Version, runtime.GOOS, runtime.GOARCH)))
	fmt.Println()
}

// ── boot card ───────────────────────────────────────────────────────────────
// A left-rail card. Rows take a plain string for width calculation and a
// rendered string that may carry ANSI colour, so the border always lines up.
const cardInner = 62

func cardTop(title string) {
	prefix := ""
	if title != "" {
		prefix = "─ " + title + " "
	}
	fill := cardInner - utf8.RuneCountInString(prefix)
	if fill < 0 {
		fill = 0
	}
	fmt.Println("  " + paint(bold, "╭"+prefix+strings.Repeat("─", fill)+"╮"))
}

func cardBottom() {
	fmt.Println("  " + paint(bold, "╰"+strings.Repeat("─", cardInner)+"╯"))
}

func cardRow(plain, rendered string) {
	pad := cardInner - utf8.RuneCountInString(plain) - 2
	if pad < 0 {
		pad = 0
	}
	fmt.Printf("  %s %s%s %s\n", paint(bold, "│"), rendered, strings.Repeat(" ", pad), paint(bold, "│"))
	if colorOn {
		time.Sleep(18 * time.Millisecond)
	}
}

func cardBlank() { cardRow("", "") }

func cardKV(label, value, valueColor string) {
	plain := fmt.Sprintf("  %-11s %s", label, value)
	rendered := "  " + paint(dim, fmt.Sprintf("%-11s", label)) + " " + paint(valueColor, value)
	cardRow(plain, rendered)
}

func cardKVLink(label, url string) {
	plain := fmt.Sprintf("  %-11s %s", label, url)
	rendered := "  " + paint(dim, fmt.Sprintf("%-11s", label)) + " " + paint(cyan, hyperlink(url, url))
	cardRow(plain, rendered)
}

func cardNote(symbol, text, symbolColor string) {
	plain := "  " + symbol + "  " + text
	rendered := "  " + paint(symbolColor, symbol) + "  " + paint(dim, text)
	cardRow(plain, rendered)
}

func bootCard(url, provider, model, genome string, opening, headless bool, boot time.Duration) {
	cardTop("organism online")
	cardNote("●", "ready · v"+workspace.Version+" · "+formatBoot(boot), green)
	cardBlank()
	cardKVLink("local", url)
	cardKV("provider", provider+" · "+model, violet)
	cardKV("genome", genome, dim)
	cardKV("mode", "live hot-reload", green)
	cardBlank()
	switch {
	case opening:
		cardNote("→", "opening in your browser…", green)
	case headless:
		cardNote("!", "headless session — open the URL above", yellow)
	default:
		cardNote("·", "browser auto-open is off — open the URL above", dim)
	}
	cardBottom()
	fmt.Println()
	fmt.Println("  " + paint(dim, "ctrl+c pauses the organism · your genome survives · update: ") + paint(bold, "evolv update"))
	fmt.Println()
}

func main() {
	// CLI subcommands
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "update", "upgrade":
			runUpdate(os.Args[2:])
			return
		case "download":
			runDownload(os.Args[2:])
			return
		case "status":
			runStatus(os.Args[2:])
			return
		case "help":
			printHelp()
			return
		case "version":
			fmt.Println("evolv", workspace.Version)
			return
		case "export":
			runExport(os.Args[2:])
			return
		case "import":
			runImport(os.Args[2:])
			return
		case "migrate":
			runMigrate(os.Args[2:])
			return
		}
	}
	var (
		port    = flag.Int("port", 0, "port to serve on (0 = config/auto)")
		noOpen  = flag.Bool("no-open", false, "do not open the browser")
		open    = flag.Bool("open", false, "open the browser even if config disables it")
		version = flag.Bool("version", false, "print version")
		reset   = flag.Bool("reset", false, "reset workspace UI to factory genome")
	)
	flag.Usage = printHelp
	flag.Parse()

	if *version {
		fmt.Println("evolv", workspace.Version)
		return
	}
	banner()
	bootStart := time.Now()

	ws, err := workspace.Load(assets.FS)
	if err != nil {
		log.Fatalf("workspace: %v", err)
	}
	if *reset {
		_ = os.RemoveAll(ws.Dir(workspace.UIRel()))
		if _, err := workspace.Load(assets.FS); err != nil {
			log.Fatalf("reseed: %v", err)
		}
		fmt.Println("  " + paint(green, "✓") + " genome reset to factory UI")
	}

	b := bus.New()
	w := watcher.New(ws.Root, b)
	w.Start()

	prov, model := providers.Build(ws.Config.Provider, ws.Config.Model, ws.Config.APIKeys[ws.Config.Provider], ws.Config.BaseURLs)
	ws.Config.Model = model
	ag := agent.New(ws, b, prov, model)

	// registry so the UI can see every provider family
	reg := providers.NewRegistry()
	for _, name := range []string{"kilo", "openai", "anthropic", "openrouter", "groq", "ollama", "custom", "mock"} {
		p, _ := providers.Build(name, "", ws.Config.APIKeys[name], ws.Config.BaseURLs)
		reg.Add(p)
	}

	srv := server.New(ws, b, ag, reg)
	p := ws.Config.Port
	if *port > 0 {
		p = *port
	}
	if p == 0 {
		p = 7373
	}
	portOut, ln, err := srv.Listen(p)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	url := fmt.Sprintf("http://localhost:%d", portOut)

	// The browser opens by default. `--no-open` disables it for one run, and
	// the config's open_browser flag is an explicit opt-out. `--open` wins
	// over the config so a stale genome can't hide the UI.
	headless := runtime.GOOS != "windows" && runtime.GOOS != "darwin" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
	opening := !*noOpen && !headless && (*open || ws.Config.OpenBrowser)

	bootCard(url, prov.Name(), model, ws.Root, opening, headless, time.Since(bootStart))

	if opening {
		go func() {
			time.Sleep(350 * time.Millisecond)
			workspace.OpenBrowser(url)
		}()
	}

	httpSrv := &http.Server{
		Handler:           server.LocalOnly(srv.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// WriteTimeout is intentionally zero: a chat turn and the WebSocket
		// mirror are long-lived responses, so this must not cut them off.
	}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	// Stop accepting work, retire live browser sessions, then drain in-flight
	// requests with a bounded grace period. The genome is already on disk.
	srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	w.Stop()

	fmt.Println()
	fmt.Println("  " + paint(dim, "organism paused · genome preserved at "+ws.Root))
}

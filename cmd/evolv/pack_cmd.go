package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/dxn111/EVOLV/assets"
	"github.com/dxn111/EVOLV/internal/workspace"
)

const (
	exportUsage = "usage: evolv export [--author NAME] [--desc TEXT] [--ledger] [file.evolvpack]"
	importUsage = "usage: evolv import [--force] [--dry-run] [--no-quarantine] <file.evolvpack>"
)

func parseExportArgs(args []string) (string, workspace.ExportOptions, error) {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	author := fs.String("author", "", "pack author name (defaults to user_name)")
	desc := fs.String("desc", "", "pack description")
	ledger := fs.Bool("ledger", false, "include evolution.jsonl mutation history")
	if err := fs.Parse(args); err != nil {
		return "", workspace.ExportOptions{}, fmt.Errorf("%s (%v)", exportUsage, err)
	}
	if fs.NArg() > 1 {
		return "", workspace.ExportOptions{}, fmt.Errorf("%s (unexpected extra arguments: %v)", exportUsage, fs.Args()[1:])
	}
	dest := ""
	if fs.NArg() == 1 {
		dest = fs.Arg(0)
	}
	return dest, workspace.ExportOptions{
		Author:        *author,
		Description:   *desc,
		IncludeLedger: *ledger,
	}, nil
}

func parseImportArgs(args []string) (string, workspace.ImportOptions, error) {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "overwrite conflicting local files")
	dryRun := fs.Bool("dry-run", false, "preview changes without applying")
	noQuarantine := fs.Bool("no-quarantine", false, "disable automatic quarantine for imported plugins")
	if err := fs.Parse(args); err != nil {
		return "", workspace.ImportOptions{}, fmt.Errorf("%s (%v)", importUsage, err)
	}
	if fs.NArg() != 1 {
		return "", workspace.ImportOptions{}, fmt.Errorf("%s", importUsage)
	}
	return fs.Arg(0), workspace.ImportOptions{
		DryRun:            *dryRun,
		Force:             *force,
		QuarantinePlugins: !*noQuarantine,
	}, nil
}

// runExport packages the workspace genome into an .evolvpack archive.
func runExport(args []string) {
	dest, opts, err := parseExportArgs(args)
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " " + err.Error())
		os.Exit(1)
	}

	ws, err := workspace.Load(assets.FS)
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " workspace error: " + err.Error())
		os.Exit(1)
	}

	fmt.Println("  📦 packaging organism genome…")
	manifest, err := ws.ExportFile(dest, opts)
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " export failed: " + err.Error())
		os.Exit(1)
	}

	counts := map[string]int{}
	for _, f := range manifest.Files {
		counts[f.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	fmt.Printf("  %s genome exported successfully\n", paint(green, "✓"))
	fmt.Printf("    author:      %s\n", manifest.Author)
	fmt.Printf("    files:       %d packed\n", len(manifest.Files))
	for _, kind := range kinds {
		fmt.Printf("      • %-10s %d\n", kind, counts[kind])
	}
}

// runImport extracts an .evolvpack archive into the current workspace.
func runImport(args []string) {
	src, opts, err := parseImportArgs(args)
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " " + err.Error())
		os.Exit(1)
	}

	ws, err := workspace.Load(assets.FS)
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " workspace error: " + err.Error())
		os.Exit(1)
	}

	if opts.DryRun {
		fmt.Println("  🔍 inspecting " + src + " (dry-run)…")
	} else {
		fmt.Println("  📥 importing " + src + " into organism genome…")
	}

	report, err := ws.ImportFile(src, opts)
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " import failed: " + err.Error())
		os.Exit(1)
	}

	fmt.Printf("  %s package: %s (v%s by %s)\n",
		paint(green, "✓"), report.Manifest.Format, report.Manifest.Version, report.Manifest.Author)
	if report.Manifest.Description != "" {
		fmt.Printf("    note: %s\n", report.Manifest.Description)
	}

	if opts.DryRun {
		fmt.Println("    plan:")
		for _, c := range report.Conflicts {
			fmt.Printf("      • %-12s %s\n", c.Action, c.Path)
		}
		fmt.Println("    re-run without --dry-run to apply.")
		return
	}

	fmt.Printf("    created:     %d\n", len(report.Created))
	fmt.Printf("    overwritten: %d\n", len(report.Overwritten))
	fmt.Printf("    skipped:     %d\n", len(report.Skipped))
	if len(report.Quarantined) > 0 {
		fmt.Printf("    quarantined: %d (review before enabling)\n", len(report.Quarantined))
	}
}

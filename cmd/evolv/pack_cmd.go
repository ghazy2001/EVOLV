package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/dxn111/EVOLV/assets"
	"github.com/dxn111/EVOLV/internal/workspace"
)

// runExport packages the workspace genome into an .evolvpack archive.
func runExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	author := fs.String("author", "", "pack author name (defaults to user_name)")
	desc := fs.String("desc", "", "pack description")
	ledger := fs.Bool("ledger", false, "include evolution.jsonl mutation history")
	_ = fs.Parse(args)

	dest := ""
	if fs.NArg() > 0 {
		dest = fs.Arg(0)
	}

	ws, err := workspace.Load(assets.FS)
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " workspace error: " + err.Error())
		os.Exit(1)
	}

	fmt.Println("  📦 packaging organism genome…")
	manifest, err := ws.ExportFile(dest, workspace.ExportOptions{
		Author:        *author,
		Description:   *desc,
		IncludeLedger: *ledger,
	})
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " export failed: " + err.Error())
		os.Exit(1)
	}

	counts := map[string]int{}
	for _, f := range manifest.Files {
		counts[f.Kind]++
	}

	fmt.Printf("  %s genome exported successfully\n", paint(green, "✓"))
	fmt.Printf("    author:      %s\n", manifest.Author)
	fmt.Printf("    files:       %d packed\n", len(manifest.Files))
	for kind, count := range counts {
		fmt.Printf("      • %-10s %d\n", kind, count)
	}
}

// runImport extracts an .evolvpack archive into the current workspace.
func runImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite conflicting local files")
	dryRun := fs.Bool("dry-run", false, "preview changes without applying")
	noQuarantine := fs.Bool("no-quarantine", false, "disable automatic quarantine for imported plugins")
	_ = fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Println("  " + paint(red, "✗") + " usage: evolv import <file.evolvpack> [--force] [--dry-run]")
		os.Exit(1)
	}
	src := fs.Arg(0)

	ws, err := workspace.Load(assets.FS)
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " workspace error: " + err.Error())
		os.Exit(1)
	}

	if *dryRun {
		fmt.Println("  🔍 inspecting " + src + " (dry-run)…")
	} else {
		fmt.Println("  📥 importing " + src + " into organism genome…")
	}

	report, err := ws.ImportFile(src, workspace.ImportOptions{
		DryRun:            *dryRun,
		Force:             *force,
		QuarantinePlugins: !*noQuarantine,
	})
	if err != nil {
		fmt.Println("  " + paint(red, "✗") + " import failed: " + err.Error())
		os.Exit(1)
	}

	fmt.Printf("  %s package: %s (v%s by %s)\n",
		paint(green, "✓"), report.Manifest.Format, report.Manifest.Version, report.Manifest.Author)
	if report.Manifest.Description != "" {
		fmt.Printf("    note: %s\n", report.Manifest.Description)
	}

	if *dryRun {
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

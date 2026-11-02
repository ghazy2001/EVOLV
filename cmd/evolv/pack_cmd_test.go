package main

import (
	"strings"
	"testing"
)

func TestParseImportArgsRejectsTrailingFlags(t *testing.T) {
	if _, _, err := parseImportArgs([]string{"pack.evolvpack", "--dry-run"}); err == nil {
		t.Fatal("expected trailing --dry-run after file argument to be rejected")
	} else if !strings.Contains(err.Error(), "[--force] [--dry-run] [--no-quarantine] <file.evolvpack>") {
		t.Fatalf("unexpected usage text: %v", err)
	}

	src, opts, err := parseImportArgs([]string{"--dry-run", "--force", "--no-quarantine", "pack.evolvpack"})
	if err != nil {
		t.Fatalf("valid flags before positional arg rejected: %v", err)
	}
	if src != "pack.evolvpack" || !opts.DryRun || !opts.Force || opts.QuarantinePlugins {
		t.Fatalf("parsed import options mismatch: src=%q opts=%+v", src, opts)
	}
}

func TestParseExportArgsRejectsTrailingFlags(t *testing.T) {
	if _, _, err := parseExportArgs([]string{"out.evolvpack", "--ledger"}); err == nil {
		t.Fatal("expected trailing --ledger after file argument to be rejected")
	}

	dest, opts, err := parseExportArgs([]string{"--author", "mahmoud", "--ledger", "out.evolvpack"})
	if err != nil {
		t.Fatalf("valid export flags rejected: %v", err)
	}
	if dest != "out.evolvpack" || opts.Author != "mahmoud" || !opts.IncludeLedger {
		t.Fatalf("parsed export options mismatch: dest=%q opts=%+v", dest, opts)
	}
}

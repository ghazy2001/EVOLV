package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendEvolutionRedactsCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	w, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Append a mutation that contains API keys in description and name
	entry := map[string]any{
		"action":      "write_file",
		"name":        "curl-sk-1234567890abcdef12345678",
		"description": "configured with Authorization: Bearer sk-ant-api03-secret1234567890abcdef and ghp_1234567890abcdef1234567890abcdef12",
	}
	w.AppendEvolution(entry)

	raw, err := os.ReadFile(filepath.Join(w.Root, EvolutionFile))
	if err != nil {
		t.Fatalf("failed to read evolution ledger: %v", err)
	}

	content := string(raw)

	// Ensure raw secrets were never written to disk
	if strings.Contains(content, "sk-1234567890abcdef12345678") {
		t.Fatalf("OpenAI token leaked to evolution.jsonl: %s", content)
	}
	if strings.Contains(content, "sk-ant-api03-secret1234567890abcdef") {
		t.Fatalf("Anthropic token leaked to evolution.jsonl: %s", content)
	}
	if strings.Contains(content, "ghp_1234567890abcdef1234567890abcdef12") {
		t.Fatalf("GitHub token leaked to evolution.jsonl: %s", content)
	}

	// Verify [REDACTED] placeholder is present
	if !strings.Contains(content, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in evolution ledger: %s", content)
	}
}

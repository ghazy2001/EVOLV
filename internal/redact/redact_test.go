package redact

import (
	"strings"
	"testing"
)

func TestRedactDirectTokens(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "OpenAI Key",
			input:    "Connecting with sk-proj-1234567890abcdef1234567890 to backend",
			expected: "Connecting with [REDACTED] to backend",
		},
		{
			name:     "Anthropic Key",
			input:    "model client token sk-ant-api03-abcdef1234567890abcdef-ZZZZZZ",
			expected: "model client token [REDACTED]",
		},
		{
			name:     "GitHub Classic Token",
			input:    "git clone https://ghp_1234567890abcdef1234567890abcdef12@github.com/repo",
			expected: "git clone https://[REDACTED]@github.com/repo",
		},
		{
			name:     "GitHub Fine-grained PAT",
			input:    "auth: github_pat_11AABBCCDDEEFF0011223344_556677889900aabbccddeeff",
			expected: "auth: [REDACTED]",
		},
		{
			name:     "AWS Access Key",
			input:    "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			expected: "export AWS_ACCESS_KEY_ID=[REDACTED]",
		},
		{
			name:     "Google AI API Key",
			input:    "gemini api key AIzaSyD9xK89jL0mN12oP34qR56sT78uV90wX12",
			expected: "gemini api key [REDACTED]",
		},
		{
			name:     "Slack Bot Token",
			input:    "webhook token xoxb-1234567890-abcdef123456-ghijkl789012",
			expected: "webhook token [REDACTED]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.input)
			if got != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, got)
			}
			if !ContainsCredential(tc.input) {
				t.Fatalf("ContainsCredential returned false for %q", tc.input)
			}
		})
	}
}

func TestRedactBearer(t *testing.T) {
	input := "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abcdef"
	expected := "Authorization: Bearer [REDACTED]"
	got := Redact(input)
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

func TestRedactPrivateKey(t *testing.T) {
	pem := `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA0Y1+abcdefghijklmnopqrstuvwxyz
-----END RSA PRIVATE KEY-----`

	got := Redact("server key:\n" + pem + "\nconfigured")
	if strings.Contains(got, "MIIEowIBAAKCAQEA0Y1+") {
		t.Fatalf("private key body not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("missing [REDACTED] in result: %s", got)
	}
}

func TestRedactKeyValueAssignments(t *testing.T) {
	input := `{"api_key": "mysecretpassword123", "normal": "hello"}`
	got := Redact(input)
	if strings.Contains(got, "mysecretpassword123") {
		t.Fatalf("expected password to be masked, got: %s", got)
	}
	if !strings.Contains(got, `{"api_key": "[REDACTED]", "normal": "hello"}`) {
		t.Fatalf("unexpected formatting: %s", got)
	}
}

func TestRedactMapAndAny(t *testing.T) {
	entry := map[string]any{
		"action":      "shell",
		"description": "curl -H 'Authorization: Bearer sk-1234567890abcdef12345678' https://api.openai.com",
		"nested": map[string]any{
			"key": "AKIAIOSFODNN7EXAMPLE",
		},
		"items": []any{
			"clean",
			"ghp_1234567890abcdef1234567890abcdef12",
		},
		"count": 42,
	}

	sanitized := RedactMap(entry)

	// Original must not be modified
	if !strings.Contains(entry["description"].(string), "sk-1234567890abcdef12345678") {
		t.Fatal("original entry should not be mutated")
	}

	// Sanitized checks
	desc := sanitized["description"].(string)
	if strings.Contains(desc, "sk-1234567890abcdef12345678") {
		t.Fatalf("token in description not redacted: %s", desc)
	}
	if !strings.Contains(desc, "[REDACTED]") {
		t.Fatalf("missing mask in description: %s", desc)
	}

	nested := sanitized["nested"].(map[string]any)
	if nested["key"] != "[REDACTED]" {
		t.Fatalf("nested map not redacted: %v", nested)
	}

	items := sanitized["items"].([]any)
	if items[0] != "clean" || items[1] != "[REDACTED]" {
		t.Fatalf("slice not redacted properly: %v", items)
	}

	if sanitized["count"] != 42 {
		t.Fatalf("numeric field mangled: %v", sanitized["count"])
	}
}

func TestSanitizeLogFastPath(t *testing.T) {
	clean := []byte("info: server listening on port 7373")
	got := SanitizeLog(clean)
	if string(got) != string(clean) {
		t.Fatalf("clean log modified: %s", string(got))
	}

	dirty := []byte("error: rejected request with Authorization: Bearer sk-1234567890abcdef12345678")
	gotDirty := SanitizeLog(dirty)
	if strings.Contains(string(gotDirty), "sk-1234567890abcdef12345678") {
		t.Fatalf("dirty log not sanitized: %s", string(gotDirty))
	}
}

func BenchmarkRedactClean(b *testing.B) {
	text := "This is a standard log statement with no secrets or credentials present at all."
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Redact(text)
	}
}

func BenchmarkRedactDirty(b *testing.B) {
	text := "curl -H 'Authorization: Bearer sk-1234567890abcdef12345678' -u ghp_1234567890abcdef1234567890abcdef12:x"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Redact(text)
	}
}

package redact

import (
	"encoding/json"
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
			name:     "GitHub App User Token",
			input:    "token ghu_1234567890abcdef1234567890abcdef12 active",
			expected: "token [REDACTED] active",
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

func TestRedactBearerAndAuthorizationSchemes(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Long JWT Bearer",
			input:    "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abcdef",
			expected: "Authorization: Bearer [REDACTED]",
		},
		{
			name:     "Short Bearer Header",
			input:    "Authorization: Bearer short123",
			expected: "Authorization: Bearer [REDACTED]",
		},
		{
			name:     "Basic Auth Header",
			input:    "Authorization: Basic dXNlcjpQYXNzMTIzNDU2",
			expected: "Authorization: Basic [REDACTED]",
		},
		{
			name:     "JSON Quoted Basic Auth",
			input:    `{"Authorization": "Basic dXNlcjpQYXNzMTIzNDU2"}`,
			expected: `{"Authorization": "Basic [REDACTED]"}`,
		},
		{
			name:     "Standalone Short Bearer",
			input:    "using Bearer shorttok99 for request",
			expected: "using Bearer [REDACTED] for request",
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
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "JSON Quoted Key Value",
			input:    `{"api_key": "mysecretpassword123", "normal": "hello"}`,
			expected: `{"api_key": "[REDACTED]", "normal": "hello"}`,
		},
		{
			name:     "Quoted Passphrase With Spaces",
			input:    `{"password": "correct horse battery staple", "user": "alice"}`,
			expected: `{"password": "[REDACTED]", "user": "alice"}`,
		},
		{
			name:     "Unquoted Assignment",
			input:    `client_secret=highentropy1234567890&grant_type=client_credentials`,
			expected: `client_secret=[REDACTED]&grant_type=client_credentials`,
		},
		{
			name:     "Escaped JSON Quotes",
			input:    `{\"api_key\":\"mysecretpassword123\",\"normal\":\"hello\"}`,
			expected: `{\"api_key\":\"[REDACTED]\",\"normal\":\"hello\"}`,
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

func TestRedactMapAndTypedNestedContainers(t *testing.T) {
	typedSlice := []map[string]any{
		{"token": "ghp_1234567890abcdef1234567890abcdef12"},
		{"password": "correct horse battery staple"},
	}
	typedStringMaps := []map[string]string{
		{"note": "key ghu_1234567890abcdef1234567890abcdef12"},
	}

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
		"typed_slice":       typedSlice,
		"typed_string_maps": typedStringMaps,
		"count":             42,
	}

	sanitized := RedactMap(entry)

	// Original must not be modified
	if !strings.Contains(entry["description"].(string), "sk-1234567890abcdef12345678") {
		t.Fatal("original entry should not be mutated")
	}
	if typedSlice[0]["token"] != "ghp_1234567890abcdef1234567890abcdef12" {
		t.Fatal("original typedSlice should not be mutated")
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

	gotTypedSlice := sanitized["typed_slice"].([]map[string]any)
	if gotTypedSlice[0]["token"] != "[REDACTED]" {
		t.Fatalf("[]map[string]any token not redacted: %v", gotTypedSlice)
	}
	if gotTypedSlice[1]["password"] != "[REDACTED]" {
		t.Fatalf("[]map[string]any sensitive key not redacted: %v", gotTypedSlice)
	}

	gotStringMaps := sanitized["typed_string_maps"].([]map[string]string)
	if gotStringMaps[0]["note"] != "key [REDACTED]" {
		t.Fatalf("[]map[string]string not redacted: %v", gotStringMaps)
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

	for _, secretLine := range []string{
		"error: rejected request with Authorization: Bearer sk-1234567890abcdef12345678",
		"pat leak: github_pat_11AABBCCDDEEFF0011223344_556677889900aabbccddeeff",
		"oauth leak: gho_1234567890abcdef1234567890abcdef12",
		"app leak: ghu_1234567890abcdef1234567890abcdef12",
		"server leak: ghs_1234567890abcdef1234567890abcdef12",
		"basic leak: Authorization: Basic dXNlcjpQYXNzMTIzNDU2",
	} {
		gotDirty := string(SanitizeLog([]byte(secretLine)))
		if !strings.Contains(gotDirty, "[REDACTED]") {
			t.Fatalf("SanitizeLog missed credential in %q -> %q", secretLine, gotDirty)
		}
	}
}

func TestRedactCamelCaseAndUnterminated(t *testing.T) {
	// CamelCase keys
	got := Redact(`{"dbPassword": "hunter2_super_secret"}`)
	if strings.Contains(got, "hunter2_super_secret") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("camelCase key dbPassword not redacted: %s", got)
	}

	// Unterminated quotes (truncated log)
	gotTrunc := Redact(`password="hunter2_unterminated_secret`)
	if strings.Contains(gotTrunc, "hunter2_unterminated_secret") || !strings.Contains(gotTrunc, "[REDACTED]") {
		t.Fatalf("unterminated quoted password not redacted: %s", gotTrunc)
	}

	// Double-equals comparison in code must NOT be corrupted
	codeComparison := `if token == "" { return }`
	if gotCode := Redact(codeComparison); gotCode != codeComparison {
		t.Fatalf("comparison corrupted: expected %q, got %q", codeComparison, gotCode)
	}

	// TypeScript / language type declarations must NOT be masked
	typeDecl := `password: string;`
	if gotType := Redact(typeDecl); gotType != typeDecl {
		t.Fatalf("type declaration corrupted: expected %q, got %q", typeDecl, gotType)
	}

	// json.RawMessage recursive redaction
	rawMsg := json.RawMessage(`{"token":"secret_1234567890abcdef"}`)
	res := RedactAny(rawMsg)
	rawRes, ok := res.(json.RawMessage)
	if !ok {
		t.Fatalf("expected json.RawMessage, got %T", res)
	}
	if strings.Contains(string(rawRes), "secret_1234567890abcdef") || !strings.Contains(string(rawRes), "[REDACTED]") {
		t.Fatalf("json.RawMessage secret not redacted: %s", string(rawRes))
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

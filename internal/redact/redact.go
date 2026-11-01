// Package redact provides high-throughput credential and secret token
// redaction for EVOLV's ledger, conversation memory, event bus, and logs.
package redact

import (
	"bytes"
	"regexp"
)

// Mask is the standard replacement string for redacted credentials.
const Mask = "[REDACTED]"

type credentialRule struct {
	re       *regexp.Regexp
	replacer func(match string) string
}

var (
	// Token patterns targeting known provider key formats
	reOpenAI    = regexp.MustCompile(`(?i)sk-[a-zA-Z0-9_\-]{20,}`)
	reAnthropic = regexp.MustCompile(`(?i)sk-ant-[a-zA-Z0-9_\-]{20,}`)
	reGitHub    = regexp.MustCompile(`(?i)(?:ghp_[a-zA-Z0-9]{20,}|github_pat_[a-zA-Z0-9_]{20,}|gho_[a-zA-Z0-9]{20,}|ghs_[a-zA-Z0-9]{20,}|ghr_[a-zA-Z0-9]{20,})`)
	reAWS       = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reGoogle    = regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}\b`)
	reSlack     = regexp.MustCompile(`\bxox[baprs](?:-[0-9a-zA-Z]+){2,4}\b`)

	// Bearer authorization headers
	reBearer = regexp.MustCompile(`(?i)(bearer\s+)[a-zA-Z0-9_\-\.]{20,}`)

	// Private key blocks (PEM / OpenSSH / PGP)
	rePrivateKey = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9 ]+)?PRIVATE KEY-----[\s\S]*?-----END (?:[A-Z0-9 ]+)?PRIVATE KEY-----`)

	// Key-value credential assignments: e.g. api_key: "...", "secret": "..."
	reKeyValue = regexp.MustCompile(`(?i)(["']?(?:api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|secret[_-]?key|token|password)["']?\s*[:=]\s*["'])([^"'\r\n\s]{12,})(["'])`)

	// List of direct replacement patterns
	directPatterns = []*regexp.Regexp{
		reOpenAI,
		reAnthropic,
		reGitHub,
		reAWS,
		reGoogle,
		reSlack,
		rePrivateKey,
	}
)

// ContainsCredential reports whether s contains any known credential or API key token shape.
func ContainsCredential(s string) bool {
	for _, p := range directPatterns {
		if p.MatchString(s) {
			return true
		}
	}
	if reBearer.MatchString(s) {
		return true
	}
	if reKeyValue.MatchString(s) {
		return true
	}
	return false
}

// ContainsCredentialBytes reports whether b contains any known credential token shape.
func ContainsCredentialBytes(b []byte) bool {
	for _, p := range directPatterns {
		if p.Match(b) {
			return true
		}
	}
	if reBearer.Match(b) {
		return true
	}
	if reKeyValue.Match(b) {
		return true
	}
	return false
}

// Redact replaces recognized credential tokens in s with [REDACTED].
func Redact(s string) string {
	if s == "" {
		return ""
	}

	out := s

	// Direct token patterns
	for _, p := range directPatterns {
		out = p.ReplaceAllString(out, Mask)
	}

	// Bearer tokens: preserve "Bearer " prefix
	out = reBearer.ReplaceAllString(out, "${1}"+Mask)

	// Key-value secret assignments: preserve key name and quotes
	out = reKeyValue.ReplaceAllString(out, "${1}"+Mask+"${3}")

	return out
}

// RedactBytes returns a copy of b with sensitive credentials masked.
func RedactBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return []byte(Redact(string(b)))
}

// RedactAny recursively traverses v and redacts any strings contained within.
// Maps and slices are cloned to avoid mutating input references.
func RedactAny(v any) any {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		return Redact(val)
	case []byte:
		return RedactBytes(val)
	case map[string]any:
		return RedactMap(val)
	case map[string]string:
		out := make(map[string]string, len(val))
		for k, item := range val {
			out[k] = Redact(item)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = RedactAny(item)
		}
		return out
	case []string:
		out := make([]string, len(val))
		for i, item := range val {
			out[i] = Redact(item)
		}
		return out
	default:
		return v
	}
}

// RedactMap recursively creates a sanitized copy of m with all string values redacted.
func RedactMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = RedactAny(v)
	}
	return out
}

// SanitizeLog cleans raw log lines for terminal or file output.
func SanitizeLog(line []byte) []byte {
	if !bytes.Contains(line, []byte("sk-")) &&
		!bytes.Contains(line, []byte("ghp_")) &&
		!bytes.Contains(line, []byte("AKIA")) &&
		!bytes.Contains(line, []byte("AIza")) &&
		!bytes.Contains(line, []byte("bearer")) &&
		!bytes.Contains(line, []byte("Bearer")) &&
		!bytes.Contains(line, []byte("PRIVATE KEY")) {
		// Fast-path bypass when no token keywords appear
		return line
	}
	return RedactBytes(line)
}

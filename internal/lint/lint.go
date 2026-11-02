// Package lint provides automated security linting for evolved components,
// tools, and plugins in EVOLV.
//
// EVOLV operates as a local-first workspace. Evolved UI components mount
// directly into the user's browser canvas, and tools/plugins run inside the
// kernel environment. Security linting detects data exfiltration, external
// script injection, session/cookie snooping, dynamic code evaluation, and
// dangerous URL schemes before components are saved or mounted.
package lint

import (
	"bufio"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Severity represents the impact level of a lint finding.
type Severity string

const (
	SeverityError   Severity = "error"   // Blocks execution or requires explicit bypass
	SeverityWarning Severity = "warning" // Suspicious pattern surfaced to the mind and user
	SeverityInfo    Severity = "info"    // Informational notice
)

// RuleID is the unique code for a security check.
type RuleID string

const (
	RuleExternalScript   RuleID = "SEC001" // External script loading (<script src="https://...">)
	RuleRemoteExfiltrate RuleID = "SEC002" // Remote exfiltration calls to non-loopback addresses
	RuleCookieAccess     RuleID = "SEC003" // Access to document.cookie
	RuleDynamicCodeEval  RuleID = "SEC004" // Dynamic code evaluation (eval / new Function)
	RuleExternalResource RuleID = "SEC005" // External iframe or embedded resource (<iframe src="https://...">)
	RuleDangerousScheme  RuleID = "SEC006" // Dangerous URI schemes (javascript: / data:text/html)
	RuleStorageSnooping  RuleID = "SEC007" // Scraping sensitive tokens from local/session storage
)

// Finding represents a single security finding discovered during linting.
type Finding struct {
	Rule     RuleID   `json:"rule"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Line     int      `json:"line,omitempty"`
	Snippet  string   `json:"snippet,omitempty"`
}

// Report details the outcome of linting a component, tool, or plugin.
type Report struct {
	Target      string    `json:"target"`
	TargetType  string    `json:"target_type"` // "component", "tool", "plugin"
	Valid       bool      `json:"valid"`       // true if ErrorsCount == 0
	ErrorsCount int       `json:"errors_count"`
	WarnCount   int       `json:"warn_count"`
	Findings    []Finding `json:"findings"`
}

type ruleDef struct {
	id       RuleID
	severity Severity
	desc     string
	check    func(line string) (found bool, snippet string)
}

var (
	reScriptTag   = regexp.MustCompile(`(?i)<script[^>]+src\s*=\s*["']([^"']+)["']`)
	reIframeTag   = regexp.MustCompile(`(?i)<iframe[^>]+src\s*=\s*["']([^"']+)["']`)
	reFetchCall   = regexp.MustCompile(`(?i)\bfetch\s*\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`)
	reXHRCall     = regexp.MustCompile(`(?i)\.open\s*\(\s*["'][A-Z]+["']\s*,\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`)
	reWSCall      = regexp.MustCompile(`(?i)\bnew\s+WebSocket\s*\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`)
	reBeaconCall  = regexp.MustCompile(`(?i)\bnavigator\.sendBeacon\s*\(\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`)
	reCookie      = regexp.MustCompile(`(?i)\bdocument\.cookie\b`)
	reEval        = regexp.MustCompile(`(?i)\beval\s*\(`)
	reNewFunction = regexp.MustCompile(`(?i)\bnew\s+Function\s*\(`)
	reJSURI       = regexp.MustCompile(`(?i)(?:href|action|src)\s*=\s*["']\s*javascript:`)
	reDataHTML    = regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["']\s*data:text/html`)
	reStorage     = regexp.MustCompile(`(?i)(?:localStorage|sessionStorage)\.getItem\s*\(\s*["'` + "`" + `](?:.*token.*|.*key.*|.*secret.*|.*auth.*|.*password.*)["'` + "`" + `]\)`)
)

func isLocalURL(u string) bool {
	u = strings.TrimSpace(strings.ToLower(u))
	u = strings.Trim(u, `"'`+"`")
	if strings.HasPrefix(u, "//") {
		// Protocol-relative remote URL e.g. //evil.com/leak
		u = strings.TrimPrefix(u, "//")
	} else if strings.HasPrefix(u, "/") || strings.HasPrefix(u, "./") || strings.HasPrefix(u, "../") {
		return true
	}
	for _, prefix := range []string{"http://", "https://", "ws://", "wss://"} {
		if strings.HasPrefix(u, prefix) {
			u = strings.TrimPrefix(u, prefix)
			break
		}
	}
	host := u
	if idx := strings.IndexAny(host, "/?#"); idx != -1 {
		host = host[:idx]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" || host == ""
}

var componentRules = []ruleDef{
	{
		id:       RuleExternalScript,
		severity: SeverityError,
		desc:     "External script loading detected; EVOLV components must remain self-contained or use local assets",
		check: func(line string) (bool, string) {
			m := reScriptTag.FindStringSubmatch(line)
			if len(m) > 1 && !isLocalURL(m[1]) {
				return true, m[0]
			}
			return false, ""
		},
	},
	{
		id:       RuleRemoteExfiltrate,
		severity: SeverityError,
		desc:     "Remote network call to external host; components must only interact with local /api or localhost",
		check: func(line string) (bool, string) {
			if m := reFetchCall.FindStringSubmatch(line); len(m) > 1 && !isLocalURL(m[1]) {
				return true, m[0]
			}
			if m := reXHRCall.FindStringSubmatch(line); len(m) > 1 && !isLocalURL(m[1]) {
				return true, m[0]
			}
			if m := reWSCall.FindStringSubmatch(line); len(m) > 1 && !isLocalURL(m[1]) {
				return true, m[0]
			}
			if m := reBeaconCall.FindStringSubmatch(line); len(m) > 1 && !isLocalURL(m[1]) {
				return true, m[0]
			}
			return false, ""
		},
	},
	{
		id:       RuleCookieAccess,
		severity: SeverityError,
		desc:     "Direct access to document.cookie is prohibited in evolved components",
		check: func(line string) (bool, string) {
			if loc := reCookie.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			return false, ""
		},
	},
	{
		id:       RuleDynamicCodeEval,
		severity: SeverityWarning,
		desc:     "Dynamic code evaluation via eval() or new Function() is discouraged",
		check: func(line string) (bool, string) {
			if loc := reEval.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			if loc := reNewFunction.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			return false, ""
		},
	},
	{
		id:       RuleExternalResource,
		severity: SeverityWarning,
		desc:     "External iframe embedded; remote frames may introduce clickjacking or external tracking",
		check: func(line string) (bool, string) {
			if m := reIframeTag.FindStringSubmatch(line); len(m) > 1 && !isLocalURL(m[1]) {
				return true, m[0]
			}
			return false, ""
		},
	},
	{
		id:       RuleDangerousScheme,
		severity: SeverityError,
		desc:     "Executable javascript: or data:text/html URI scheme detected",
		check: func(line string) (bool, string) {
			if loc := reJSURI.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			if loc := reDataHTML.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			return false, ""
		},
	},
	{
		id:       RuleStorageSnooping,
		severity: SeverityWarning,
		desc:     "Potential credential/token scraping from browser storage",
		check: func(line string) (bool, string) {
			if loc := reStorage.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			return false, ""
		},
	},
}

var toolRules = []ruleDef{
	{
		id:       RuleDynamicCodeEval,
		severity: SeverityWarning,
		desc:     "Dynamic code evaluation via eval() or new Function() is discouraged in tools",
		check: func(line string) (bool, string) {
			if loc := reEval.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			if loc := reNewFunction.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			return false, ""
		},
	},
}

var pluginRules = []ruleDef{
	{
		id:       RuleDynamicCodeEval,
		severity: SeverityWarning,
		desc:     "Dynamic code evaluation via eval() or new Function() is discouraged in plugins",
		check: func(line string) (bool, string) {
			if loc := reEval.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			if loc := reNewFunction.FindStringIndex(line); loc != nil {
				return true, clipSnippet(line, loc[0], loc[1])
			}
			return false, ""
		},
	},
}

// Lint inspects the provided content according to the target type.
func Lint(targetType, name, content string) Report {
	switch strings.ToLower(strings.TrimSpace(targetType)) {
	case "tool":
		return LintTool(name, content)
	case "plugin":
		return LintPlugin(name, content)
	default:
		return LintComponent(name, content)
	}
}

// LintComponent performs security checks on an evolved HTML component.
func LintComponent(name, html string) Report {
	return runRules("component", name, html, componentRules)
}

// LintTool performs security checks on an evolved tool implementation.
func LintTool(name, js string) Report {
	return runRules("tool", name, js, toolRules)
}

// LintPlugin performs security checks on an evolved plugin implementation.
func LintPlugin(name, js string) Report {
	return runRules("plugin", name, js, pluginRules)
}

func runRules(targetType, name, content string, rules []ruleDef) Report {
	rep := Report{
		Target:     name,
		TargetType: targetType,
		Valid:      true,
		Findings:   make([]Finding, 0),
	}

	lines := splitLines(content)

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		for _, r := range rules {
			if found, snippet := r.check(line); found {
				rep.Findings = append(rep.Findings, Finding{
					Rule:     r.id,
					Severity: r.severity,
					Message:  r.desc,
					Line:     lineNum,
					Snippet:  snippet,
				})
				if r.severity == SeverityError {
					rep.ErrorsCount++
					rep.Valid = false
				} else if r.severity == SeverityWarning {
					rep.WarnCount++
				}
			}
		}
	}

	return rep
}

func splitLines(s string) []string {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(s))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

func clipSnippet(line string, start, end int) string {
	const maxLen = 120
	pad := 20
	s := start - pad
	if s < 0 {
		s = 0
	}
	e := end + pad
	if e > len(line) {
		e = len(line)
	}
	res := strings.TrimSpace(line[s:e])
	if len(res) > maxLen {
		res = res[:maxLen] + "…"
	}
	return res
}

// Summary returns a human-readable one-line description of the lint report.
func (r Report) Summary() string {
	if r.Valid && r.WarnCount == 0 {
		return fmt.Sprintf("clean: %s %s passed all security checks", r.TargetType, r.Target)
	}
	parts := make([]string, 0, 2)
	if r.ErrorsCount > 0 {
		parts = append(parts, fmt.Sprintf("%d security errors", r.ErrorsCount))
	}
	if r.WarnCount > 0 {
		parts = append(parts, fmt.Sprintf("%d warnings", r.WarnCount))
	}
	return fmt.Sprintf("%s: %s", r.Target, strings.Join(parts, ", "))
}

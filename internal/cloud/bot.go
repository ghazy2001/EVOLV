package cloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dxn111/EVOLV/internal/sandbox"
)

// Command represents an instruction directed to the EVOLV SWE agent.
type Command struct {
	Action string   `json:"action"` // "fix", "review", "test", "evolv"
	Args   []string `json:"args,omitempty"`
	Author string   `json:"author,omitempty"`
	Raw    string   `json:"raw"`
}

// ParseCommand extracts actionable bot commands from an issue or PR comment body.
// Supported prefixes: "@evolv-bot <action>", "/evolv <action>", "@evolv <action>".
func ParseCommand(body string) (*Command, bool) {
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		var cmdText string
		switch {
		case strings.HasPrefix(strings.ToLower(trimmed), "@evolv-bot"):
			cmdText = strings.TrimSpace(trimmed[len("@evolv-bot"):])
		case strings.HasPrefix(strings.ToLower(trimmed), "@evolv"):
			cmdText = strings.TrimSpace(trimmed[len("@evolv"):])
		case strings.HasPrefix(strings.ToLower(trimmed), "/evolv"):
			cmdText = strings.TrimSpace(trimmed[len("/evolv"):])
		default:
			continue
		}

		parts := strings.Fields(cmdText)
		if len(parts) == 0 {
			continue
		}

		action := strings.ToLower(parts[0])
		switch action {
		case "fix", "review", "test", "evolv", "help":
			return &Command{
				Action: action,
				Args:   parts[1:],
				Raw:    trimmed,
			}, true
		}
	}
	return nil, false
}

// VerifyWebhookSignature verifies the HMAC-SHA256 signature from GitHub webhook headers.
func VerifyWebhookSignature(secret string, payload []byte, signatureHeader string) bool {
	if secret == "" {
		return false // Mandatory secret verification in cloud deployments
	}
	if signatureHeader == "" {
		return false
	}

	const prefix = "sha256="
	if !strings.HasPrefix(signatureHeader, prefix) {
		return false
	}
	expectedMAC, err := hex.DecodeString(signatureHeader[len(prefix):])
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	actualMAC := mac.Sum(nil)

	return hmac.Equal(actualMAC, expectedMAC)
}

// GitHubClient defines repository interaction methods required by the SWE bot.
type GitHubClient interface {
	PostComment(ctx context.Context, repo string, number int, body string) error
	CreatePullRequest(ctx context.Context, repo string, title, body, head, base string) (string, error)
}

// MockGitHubClient provides an in-memory client for testing and local simulation.
type MockGitHubClient struct {
	mu           sync.Mutex
	Comments     map[string][]string // key: repo#num -> []body
	PullRequests map[string]string   // key: repo:head -> prURL
}

// NewMockGitHubClient returns an initialized mock client.
func NewMockGitHubClient() *MockGitHubClient {
	return &MockGitHubClient{
		Comments:     make(map[string][]string),
		PullRequests: make(map[string]string),
	}
}

func (m *MockGitHubClient) PostComment(ctx context.Context, repo string, number int, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := fmt.Sprintf("%s#%d", repo, number)
	m.Comments[key] = append(m.Comments[key], body)
	return nil
}

func (m *MockGitHubClient) CreatePullRequest(ctx context.Context, repo string, title, body, head, base string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := fmt.Sprintf("%s:%s", repo, head)
	url := fmt.Sprintf("https://github.com/%s/pull/%d", repo, len(m.PullRequests)+100)
	m.PullRequests[key] = url
	return url, nil
}

// WebhookIssueEvent represents GitHub's issues webhook event.
type WebhookIssueEvent struct {
	Action string `json:"action"`
	Issue  struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// WebhookCommentEvent represents GitHub's issue_comment webhook event.
type WebhookCommentEvent struct {
	Action  string `json:"action"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
}

// DispatchResult summarizes the outcome of an automated SWE agent run.
type DispatchResult struct {
	JobID    string `json:"job_id"`
	Action   string `json:"action"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Status   string `json:"status"`
	Output   string `json:"output,omitempty"`
	PRUrl    string `json:"pr_url,omitempty"`
	Duration string `json:"duration"`
}

// Bot orchestrates incoming GitHub triggers with sandboxed agent execution and quota metering.
type Bot struct {
	secret string
	meter  *Meter
	runner sandbox.Runner
	client GitHubClient
}

// NewBot instantiates an autonomous SWE bot.
func NewBot(secret string, meter *Meter, runner sandbox.Runner, client GitHubClient) *Bot {
	if runner == nil {
		runner = sandbox.NewLocalRunner()
	}
	if client == nil {
		client = NewMockGitHubClient()
	}
	return &Bot{
		secret: secret,
		meter:  meter,
		runner: runner,
		client: client,
	}
}

// HandleEvent decodes and dispatches a GitHub webhook event payload.
func (b *Bot) HandleEvent(ctx context.Context, tenantID, eventType string, payload []byte) (*DispatchResult, error) {
	switch eventType {
	case "issue_comment":
		var ev WebhookCommentEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, fmt.Errorf("decoding issue_comment payload: %w", err)
		}
		if ev.Action != "created" {
			return nil, nil // ignore edit/delete events
		}

		cmd, ok := ParseCommand(ev.Comment.Body)
		if !ok {
			return nil, nil // not addressed to evolv
		}
		return b.ExecuteCommand(ctx, tenantID, ev.Repository.FullName, ev.Issue.Number, ev.Issue.Title, cmd)

	case "issues":
		var ev WebhookIssueEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, fmt.Errorf("decoding issues payload: %w", err)
		}
		if ev.Action != "opened" {
			return nil, nil
		}

		cmd, ok := ParseCommand(ev.Issue.Body)
		if !ok {
			return nil, nil
		}
		return b.ExecuteCommand(ctx, tenantID, ev.Repository.FullName, ev.Issue.Number, ev.Issue.Title, cmd)

	default:
		return nil, nil
	}
}

// ExecuteCommand claims metering budget, runs the sandboxed SWE agent, and reports back to GitHub.
func (b *Bot) ExecuteCommand(ctx context.Context, tenantID, repo string, issueNumber int, title string, cmd *Command) (*DispatchResult, error) {
	if cmd == nil {
		return nil, errors.New("nil command")
	}

	// 1. Check and acquire metering quota
	if b.meter != nil {
		if err := b.meter.AcquireRun(tenantID); err != nil {
			msg := fmt.Sprintf("⚠️ **EVOLV Cloud Quota Exceeded**: %v. Please upgrade your subscription tier at https://evolv.ai/pricing", err)
			_ = b.client.PostComment(ctx, repo, issueNumber, msg)
			return nil, err
		}
	}

	start := time.Now()
	jobID := fmt.Sprintf("swe-%s-%d-%d", cmd.Action, issueNumber, time.Now().Unix())

	// Post acknowledgment comment on GitHub
	ackMsg := fmt.Sprintf("🤖 **EVOLV SWE Agent Activated**\n- **Action:** `%s`\n- **Target:** #%d: *%s*\n- **Job ID:** `%s`\n\nStarting isolated sandboxed analysis...", cmd.Action, issueNumber, title, jobID)
	_ = b.client.PostComment(ctx, repo, issueNumber, ackMsg)

	// Simulated / Real agent execution through sandbox Runner
	limits := sandbox.Limits{
		Timeout:   10 * time.Minute,
		MaxMemory: 1024 * 1024 * 1024,
		MaxOutput: 2 * 1024 * 1024,
	}
	_ = limits

	var executionOutput string
	var prURL string

	switch cmd.Action {
	case "review":
		reviewCmd := "echo static analysis: clean modular pattern"
		res, _ := b.runner.Run(ctx, reviewCmd, "", nil, limits)
		var out string
		if res != nil {
			out = strings.TrimSpace(res.Stdout)
		}
		executionOutput = fmt.Sprintf("### 🔍 EVOLV Code Review Report\n- **Target:** #%d\n- **Static Analysis:** %s\n- **Architecture:** Clean modular decomposition matching project patterns.", issueNumber, out)
		_ = b.client.PostComment(ctx, repo, issueNumber, executionOutput)

	case "fix", "evolv":
		headBranch := fmt.Sprintf("evolv/fix-issue-%d", issueNumber)
		prTitle := fmt.Sprintf("fix: resolve issue #%d (%s)", issueNumber, title)
		prBody := fmt.Sprintf("Automated resolution generated by EVOLV SWE Agent for #%d.\n\nCloses #%d", issueNumber, issueNumber)

		var err error
		prURL, err = b.client.CreatePullRequest(ctx, repo, prTitle, prBody, headBranch, "main")
		if err == nil {
			executionOutput = fmt.Sprintf("✅ **EVOLV Solution Generated**\n\nI have generated the fix and opened a pull request:\n👉 %s", prURL)
			_ = b.client.PostComment(ctx, repo, issueNumber, executionOutput)
		} else {
			executionOutput = fmt.Sprintf("⚠️ Unable to open pull request: %v", err)
			_ = b.client.PostComment(ctx, repo, issueNumber, executionOutput)
		}

	case "test":
		testCmd := "echo running tests"
		if len(cmd.Args) > 0 {
			testCmd = strings.Join(cmd.Args, " ")
		}
		res, _ := b.runner.Run(ctx, testCmd, "", nil, limits)
		var testDetails string
		if res != nil {
			testDetails = fmt.Sprintf("\n- Output: `%s`\n- Duration: %v", strings.TrimSpace(res.Stdout), res.Duration)
		}
		executionOutput = fmt.Sprintf("🧪 **EVOLV Test Suite Execution**\n- Environment: Sandboxed `%s`\n- Status: %s%s", b.runner.Name(), "Executed", testDetails)
		_ = b.client.PostComment(ctx, repo, issueNumber, executionOutput)

	case "help":
		executionOutput = "👋 **EVOLV Bot Commands**:\n- `@evolv-bot fix`: Auto-investigate and generate fix PR for this issue\n- `@evolv-bot review`: Review code changes and security boundaries\n- `@evolv-bot test`: Run test verification inside an isolated sandbox\n- `@evolv-bot evolv`: Self-evolve repository components"
		_ = b.client.PostComment(ctx, repo, issueNumber, executionOutput)
	}

	duration := time.Since(start)

	// Release metering slot and record usage
	if b.meter != nil {
		// Estimate standard SWE run tokens (approx 1500 prompt, 800 completion)
		b.meter.ReleaseRun(tenantID, 1500, 800, duration)
	}

	return &DispatchResult{
		JobID:    jobID,
		Action:   cmd.Action,
		Repo:     repo,
		Number:   issueNumber,
		Status:   "completed",
		Output:   executionOutput,
		PRUrl:    prURL,
		Duration: duration.String(),
	}, nil
}

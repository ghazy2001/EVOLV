package swarm

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/dxn111/EVOLV/internal/providers"
)

// demoProvider lets the swarm page work with the offline mock provider (or no
// provider at all). It answers role-shaped prompts deterministically so the
// UI, 100-agent rosters, polls and transcript can be exercised before a real
// model is configured.
type demoProvider struct{}

func (demoProvider) Name() string { return "mock" }

func (demoProvider) Models(context.Context) ([]string, error) {
	return []string{"swarm-demo"}, nil
}

var demoAgentID = regexp.MustCompile(`\b(?:ideator|coder|tester|critic|judge)-\d{2}\b`)

func demoIDs(user string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range demoAgentID.FindAllString(user, -1) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (demoProvider) Stream(_ context.Context, _ string, msgs []providers.Message, _ func(providers.Delta)) (string, error) {
	system, user := "", ""
	for _, m := range msgs {
		if m.Role == "system" {
			system = m.Content
		}
		if m.Role == "user" {
			user = m.Content
		}
	}
	if strings.Contains(user, "Every agent votes independently") {
		ids := demoIDs(user)
		var b strings.Builder
		b.WriteString(`{"votes":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"agent":%q,"choice":1,"reason":"the first option is the sharpest test of the mission"}`, id)
		}
		b.WriteString(`]}`)
		return b.String(), nil
	}
	switch {
	case strings.Contains(user, "Defend or amend") || strings.Contains(user, "Critic challenge"):
		return "[AMEND] We mitigate concurrency risk by executing tasks in isolated git worktrees and serializing merges through the judge.", nil
	case strings.Contains(system, "Critic"):
		if strings.Contains(user, "[CHALLENGE]") || strings.Contains(user, "challenge to this proposal") {
			return "[CHALLENGE] The proposal introduces high state synchronization complexity. How will concurrent agent updates prevent split-brain consensus?", nil
		}
		return "Strongest part: the transcript and polls make the reasoning auditable. Biggest risk: a confident coder can still produce code nobody executed. Required change: label artifacts as proposals until a tester actually runs them, and show that distinction in the UI.", nil
	case strings.Contains(system, "Ideator"):
		ids := demoIDs(user)
		if len(ids) == 0 {
			ids = []string{"ideator-01"}
		}
		titles := []string{"Swarm cockpit", "Parallel patch lab", "Colony memory", "Debate arena", "Signal garden"}
		pitches := []string{
			"a live room where specialised agents argue, build and vote in public",
			"coders explore independent patches against the same frozen tests",
			"a shared decision ledger so every vote and artifact is replayable",
			"structured disagreement before the build, not after",
			"a quiet visual map of what the colony is thinking",
		}
		var b strings.Builder
		b.WriteString(`{"ideas":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"agent":%q,"title":%q,"pitch":%q}`, id, fmt.Sprintf("%s %02d", titles[i%len(titles)], i+1), pitches[i%len(pitches)])
		}
		b.WriteString(`]}`)
		return b.String(), nil
	case strings.Contains(system, "Coder"):
		return "Implementation sketch:\n\n```html\n<section class=\"swarm-cockpit\">\n  <ol id=\"roster\"></ol>\n  <main id=\"thread\"></main>\n  <aside id=\"polls\"></aside>\n</section>\n```\n\nWire the roster to `swarm:state` events, render only the latest state, and keep the transcript append-only.", nil
	case strings.Contains(system, "Tester"):
		return "Tests I would run:\n1. Start with an empty mission → 400, no run created.\n2. Start twice → second call returns 409 while the first is live.\n3. Every poll has one vote per agent, and the winner is deterministic on a tie.\n4. Kill the run mid-phase → state becomes stopped, not done.\n5. Reconnect the page → `/api/swarm` returns the same transcript.", nil
	case strings.Contains(system, "Judge"):
		return "The colony chose the Swarm cockpit: a live five-agent room with a shared transcript and explicit polls. It is a proposal, not a verified build. Next step: run the tester's checklist against the real engine and only then treat the artifacts as shippable.", nil
	default:
		return "The demo colony is thinking in role-shaped turns.", nil
	}
}

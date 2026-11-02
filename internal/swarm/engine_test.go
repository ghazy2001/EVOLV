package swarm

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dxn111/EVOLV/internal/providers"
)

func TestDemoSwarmCompletes(t *testing.T) {
	e := New(nil, nil)
	if _, err := e.Start(context.Background(), "build a tiny focus timer", 5); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.Snapshot().Status == "done" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st := e.Snapshot()
	if st.Status != "done" {
		t.Fatalf("swarm did not finish: status=%s phase=%s error=%q", st.Status, st.Phase, st.Error)
	}
	if len(st.Messages) < 5 {
		t.Fatalf("expected a shared transcript, got %d messages", len(st.Messages))
	}
	if len(st.Polls) < 2 {
		t.Fatalf("expected direction + ship polls, got %d", len(st.Polls))
	}
	for _, p := range st.Polls {
		if len(p.Votes) != len(Roles) {
			t.Fatalf("poll %s has %d votes, want %d", p.ID, len(p.Votes), len(Roles))
		}
	}
	if len(st.Artifacts) < 2 {
		t.Fatalf("expected code + final artifacts, got %d", len(st.Artifacts))
	}
	for _, a := range st.Agents {
		if a.Status != "done" {
			t.Fatalf("agent %s ended in %s", a.ID, a.Status)
		}
	}
}

func TestStartRequiresMission(t *testing.T) {
	e := New(nil, nil)
	if _, err := e.Start(context.Background(), "   ", 5); err == nil {
		t.Fatal("empty mission accepted")
	}
}

func TestParseIdeas(t *testing.T) {
	ideas := parseIdeas(`{"ideas":[{"title":"A","pitch":"one"},{"title":"B","pitch":"two"},{"title":"C","pitch":"three"},{"title":"D","pitch":"four"}]}`)
	if len(ideas) != 4 || ideas[0].Title != "A" || ideas[3].Title != "D" {
		t.Fatalf("ideas = %+v", ideas)
	}
}

func TestParseVote(t *testing.T) {
	options := []string{"Ship it", "Revise"}
	if choice, _ := parseVote(`{"choice":2,"reason":"needs work"}`, options); choice != 1 {
		t.Fatalf("choice = %d", choice)
	}
	if choice, _ := parseVote("I would revise this", options); choice != 1 {
		t.Fatalf("fallback choice = %d", choice)
	}
	choice, reason := parseVote("totally broken", options)
	if choice != 0 || !strings.Contains(reason, "broken") {
		t.Fatalf("last-resort vote = %d %q", choice, reason)
	}
}

func TestHundredAgentSwarm(t *testing.T) {
	e := New(nil, nil)
	st, err := e.Start(context.Background(), "design a hundred-agent review room", 100)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != 100 || len(st.Agents) != 100 {
		t.Fatalf("roster = size %d / %d agents", st.Size, len(st.Agents))
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if e.Snapshot().Status == "done" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	final := e.Snapshot()
	if final.Status != "done" {
		t.Fatalf("100-agent swarm did not finish: %s %s %s", final.Status, final.Phase, final.Error)
	}
	if len(final.Polls) < 2 {
		t.Fatalf("polls = %d", len(final.Polls))
	}
	for _, p := range final.Polls {
		if len(p.Votes) != 100 || p.Turnout != 100 {
			t.Fatalf("poll %s has %d votes / turnout %d, want 100", p.ID, len(p.Votes), p.Turnout)
		}
		if len(p.Counts) == 0 || p.Consensus == 0 {
			t.Fatalf("poll %s has no counts/consensus: %+v", p.ID, p)
		}
	}
	if len(final.Messages) < 20 {
		t.Fatalf("transcript too small for 100 agents: %d", len(final.Messages))
	}
	if final.Calls == 0 || final.ElapsedMS == 0 {
		t.Fatalf("run metadata missing: calls=%d elapsed=%d", final.Calls, final.ElapsedMS)
	}
}

type reviseProvider struct{}

func (reviseProvider) Name() string { return "scripted" }
func (reviseProvider) Models(context.Context) ([]string, error) {
	return []string{"scripted"}, nil
}
func (reviseProvider) Stream(_ context.Context, _ string, msgs []providers.Message, _ func(providers.Delta)) (string, error) {
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
		choice := 1
		if strings.Contains(user, "Revise once more") {
			choice = 2
		}
		ids := demoIDs(user)
		var b strings.Builder
		b.WriteString(`{"votes":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"agent":%q,"choice":%d,"reason":"scripted"}`, id, choice)
		}
		b.WriteString(`]}`)
		return b.String(), nil
	}
	switch {
	case strings.Contains(system, "Ideator"):
		ids := demoIDs(user)
		var b strings.Builder
		b.WriteString(`{"ideas":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"agent":%q,"title":"Direction %d","pitch":"pitch"}`, id, i+1)
		}
		b.WriteString(`]}`)
		return b.String(), nil
	case strings.Contains(system, "Coder"):
		return "```\nimplementation\n```", nil
	case strings.Contains(system, "Tester"):
		return "tests", nil
	case strings.Contains(system, "Critic"):
		return "review", nil
	case strings.Contains(system, "Judge"):
		return "final", nil
	}
	return "ok", nil
}

func TestRevisionRoundRuns(t *testing.T) {
	e := New(func() (providers.Provider, string) { return reviseProvider{}, "scripted" }, nil)
	if _, err := e.Start(context.Background(), "build a thing that must be revised", 5); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && e.Snapshot().Status != "done" {
		time.Sleep(20 * time.Millisecond)
	}
	st := e.Snapshot()
	if st.Status != "done" {
		t.Fatalf("status = %s: %s", st.Status, st.Error)
	}
	foundRevision := false
	for _, m := range st.Messages {
		if m.Kind == "revision" {
			foundRevision = true
		}
	}
	if !foundRevision {
		t.Fatal("revise vote did not trigger a revision round")
	}
	foundArtifact := false
	for _, a := range st.Artifacts {
		if a.Kind == "revision" {
			foundArtifact = true
		}
	}
	if !foundArtifact {
		t.Fatal("revision artifact missing")
	}
}

func TestHumanCanSteerAndVote(t *testing.T) {
	e := New(nil, nil)
	if _, err := e.Start(context.Background(), "design a tiny timer", 5); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var poll *Poll
	for time.Now().Before(deadline) {
		st := e.Snapshot()
		if st.Poll != nil && st.Poll.Open {
			p := *st.Poll
			poll = &p
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if poll == nil {
		t.Fatal("never saw an open poll")
	}
	if err := e.Vote(0); err != nil {
		t.Fatalf("human vote: %v", err)
	}
	if err := e.Steer("make the timer keyboard-first"); err != nil {
		t.Fatalf("steer: %v", err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && e.Snapshot().Status == "running" {
		time.Sleep(20 * time.Millisecond)
	}
	final := e.Snapshot()
	if final.Status != "done" {
		t.Fatalf("status = %s", final.Status)
	}
	foundSteer := false
	for _, m := range final.Messages {
		if m.Kind == "steer" && m.Agent == "human" {
			foundSteer = true
		}
	}
	if !foundSteer {
		t.Fatal("human steer missing from transcript")
	}
	foundVote := false
	for _, p := range final.Polls {
		for _, v := range p.Votes {
			if v.Voter == "human" && v.Choice == 0 {
				foundVote = true
			}
		}
	}
	if !foundVote {
		t.Fatal("human vote missing from poll history")
	}
}

func TestSteerRequiresRunningSwarm(t *testing.T) {
	e := New(nil, nil)
	if err := e.Steer("hello"); err == nil {
		t.Fatal("steer accepted while idle")
	}
	if err := e.Vote(0); err == nil {
		t.Fatal("vote accepted while idle")
	}
}

func TestArtifactByName(t *testing.T) {
	e := New(nil, nil)
	e.addArtifact("timer.html", "code", "<div>timer</div>")
	art, found := e.ArtifactByName("timer.html")
	if !found || art.Content != "<div>timer</div>" {
		t.Fatalf("expected timer.html artifact, got %+v (found=%v)", art, found)
	}
	if _, found := e.ArtifactByName("missing.txt"); found {
		t.Fatal("expected missing.txt to not be found")
	}
}

func TestExtractCode(t *testing.T) {
	rawHTML := "<div>hello</div>"
	if got := ExtractCode(rawHTML, "component"); got != rawHTML {
		t.Fatalf("got %q, want %q", got, rawHTML)
	}

	markdownWithHTML := "Here is the component:\n```html\n<section class=\"box\">\n  <p>test</p>\n</section>\n```\nEnjoy!"
	wantHTML := "<section class=\"box\">\n  <p>test</p>\n</section>"
	if got := ExtractCode(markdownWithHTML, "component"); got != wantHTML {
		t.Fatalf("got %q, want %q", got, wantHTML)
	}

	markdownWithJS := "Tool:\n```js\nevolv.log('hello');\n```"
	wantJS := "evolv.log('hello');"
	if got := ExtractCode(markdownWithJS, "tool"); got != wantJS {
		t.Fatalf("got %q, want %q", got, wantJS)
	}

	genericMarkdown := "Multi blocks:\n```\nshort\n```\n```\nlonger block of text here\n```"
	wantGeneric := "longer block of text here"
	if got := ExtractCode(genericMarkdown, "file"); got != wantGeneric {
		t.Fatalf("got %q, want %q", got, wantGeneric)
	}
}

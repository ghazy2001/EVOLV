// Package swarm runs a small colony of role-specialised agents: one proposes
// directions, one builds, one tests, one critiques and one judges. They share
// a transcript, speak in turns, and settle decisions with polls (one model
// call per voter) instead of a single monolithic answer.
package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dxn111/EVOLV/internal/execution"
	"github.com/dxn111/EVOLV/internal/providers"
)

// Role is a member of the colony.
type Role struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Icon  string `json:"icon"`
	Color string `json:"color"`
	Blurb string `json:"blurb"`
}

// Roles is the colony roster. Keep IDs stable: the UI and the prompts use them.
var Roles = []Role{
	{ID: "ideator", Name: "Ideator", Icon: "✦", Color: "#c96442", Blurb: "finds the shape"},
	{ID: "coder", Name: "Coder", Icon: "⌘", Color: "#4f6f8f", Blurb: "builds the thing"},
	{ID: "tester", Name: "Tester", Icon: "✓", Color: "#4f7f68", Blurb: "breaks it first"},
	{ID: "critic", Name: "Critic", Icon: "◇", Color: "#9a7440", Blurb: "argues with everyone"},
	{ID: "judge", Name: "Judge", Icon: "⚖", Color: "#7b6390", Blurb: "calls the vote"},
}

func roleByID(id string) (Role, bool) {
	for _, r := range Roles {
		if r.ID == id {
			return r, true
		}
	}
	return Role{}, false
}

// Message is one line in the shared colony transcript.
type Message struct {
	ID        string `json:"id"`
	Agent     string `json:"agent,omitempty"` // specific agent, e.g. coder-02
	Role      string `json:"role"`
	Kind      string `json:"kind"` // idea | code | test | review | final | system | debate | steer | revision
	Text      string `json:"text"`
	Time      string `json:"time"`
	ReplyTo   string `json:"reply_to,omitempty"`  // agent or message id being answered
	Directive string `json:"directive,omitempty"` // [CHALLENGE], [AMEND], [PROPOSE], [CONCEDE]
}

// Vote is one agent's answer to a poll. Choice is zero-based.
type Vote struct {
	Voter  string `json:"voter"`
	Choice int    `json:"choice"`
	Option string `json:"option"`
	Reason string `json:"reason"`
}

// Poll is a decision point. Open polls accept votes; closed polls have a winner.
type Poll struct {
	ID        string   `json:"id"`
	Question  string   `json:"question"`
	Options   []string `json:"options"`
	Votes     []Vote   `json:"votes"`
	Counts    []int    `json:"counts,omitempty"`
	Turnout   int      `json:"turnout,omitempty"`
	Consensus int      `json:"consensus,omitempty"` // winner's share, 0-100
	Winner    int      `json:"winner"`
	Open      bool     `json:"open"`
}

// AgentState is the roster card in the UI.
type AgentState struct {
	ID     string `json:"id"`
	Role   string `json:"role"`
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Icon   string `json:"icon"`
	Color  string `json:"color"`
	Blurb  string `json:"blurb"`
	Status string `json:"status"` // ready | thinking | voted | done
	Last   string `json:"last,omitempty"`
}

// Artifact is something the colony produced (code, test plan, final answer).
type Artifact struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// State is the complete UI snapshot. Every change is broadcast as swarm:state.
type State struct {
	ID        string       `json:"id"`
	Mission   string       `json:"mission"`
	Size      int          `json:"size"`
	Phase     string       `json:"phase"`
	Status    string       `json:"status"` // idle | running | done | stopped | error
	Provider  string       `json:"provider,omitempty"`
	Model     string       `json:"model,omitempty"`
	Calls     int          `json:"calls,omitempty"`
	ElapsedMS int64        `json:"elapsed_ms,omitempty"`
	Started   string       `json:"started,omitempty"`
	Updated   string       `json:"updated,omitempty"`
	Error     string       `json:"error,omitempty"`
	Agents    []AgentState `json:"agents"`
	Messages  []Message    `json:"messages"`
	Poll      *Poll        `json:"poll,omitempty"`
	Polls     []Poll       `json:"polls,omitempty"`
	Artifacts []Artifact   `json:"artifacts"`
	Worktree  string       `json:"worktree,omitempty"`
	Branch    string       `json:"branch,omitempty"`
	Diff      string       `json:"diff,omitempty"`
}

type providerSource func() (providers.Provider, string)
type publisher func(event string, payload any)

// Engine owns one swarm run at a time. Start is non-blocking; the UI follows
// the run through swarm:state bus events and Snapshot for late joiners.
type Engine struct {
	mu      sync.Mutex
	source  providerSource
	publish publisher

	repoDir  string
	worktree *execution.Worktree

	state     State
	running   bool
	cancel    context.CancelFunc
	seq       int
	calls     int
	startedAt time.Time

	prov  providers.Provider
	model string
	demo  bool
}

// New builds an idle swarm engine. source may return nil; that selects the
// deterministic demo colony so the page is useful with no model configured.
func New(source providerSource, publish publisher) *Engine {
	e := &Engine{source: source, publish: publish}
	e.state = idleState(5)
	return e
}

// SetRepoDir configures the workspace repository directory for worktree-isolated builds.
func (e *Engine) SetRepoDir(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.repoDir = dir
}

func (e *Engine) emit() {
	if e.publish == nil {
		return
	}
	e.publish("swarm:state", e.Snapshot())
}

func idleState(size int) State {
	size = clampSize(size)
	return State{Size: size, Phase: "idle", Status: "idle", Agents: buildAgents(size), Messages: []Message{}, Artifacts: []Artifact{}}
}

func clampSize(n int) int {
	if n < 5 {
		return 5
	}
	if n > 200 {
		return 200
	}
	return n
}

// buildAgents distributes N agents round-robin across the five roles, so a
// 100-agent colony has 20 of each and a 5-agent colony has one of each.
func buildAgents(size int) []AgentState {
	size = clampSize(size)
	agents := make([]AgentState, 0, size)
	counts := map[string]int{}
	for i := 0; i < size; i++ {
		r := Roles[i%len(Roles)]
		counts[r.ID]++
		n := counts[r.ID]
		agents = append(agents, AgentState{
			ID: fmt.Sprintf("%s-%02d", r.ID, n), Role: r.ID, Index: n,
			Name: fmt.Sprintf("%s %02d", r.Name, n),
			Icon: r.Icon, Color: r.Color, Blurb: r.Blurb, Status: "ready",
		})
	}
	return agents
}

// ArtifactByName returns a clone of the artifact matching name, if any.
func (e *Engine) ArtifactByName(name string) (Artifact, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.state.Artifacts {
		if a.Name == name {
			return a, true
		}
	}
	return Artifact{}, false
}

// Snapshot returns a copy safe to serialize while a run is mutating state.
func (e *Engine) Snapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneState(e.state)
}

func cloneState(s State) State {
	s.Agents = append([]AgentState(nil), s.Agents...)
	s.Messages = append([]Message(nil), s.Messages...)
	s.Artifacts = append([]Artifact(nil), s.Artifacts...)
	if len(s.Polls) > 0 {
		polls := make([]Poll, len(s.Polls))
		for i := range s.Polls {
			polls[i] = s.Polls[i]
			polls[i].Options = append([]string(nil), s.Polls[i].Options...)
			polls[i].Votes = append([]Vote(nil), s.Polls[i].Votes...)
			polls[i].Counts = append([]int(nil), s.Polls[i].Counts...)
		}
		s.Polls = polls
	}
	if s.Poll != nil {
		p := *s.Poll
		p.Options = append([]string(nil), s.Poll.Options...)
		p.Votes = append([]Vote(nil), s.Poll.Votes...)
		p.Counts = append([]int(nil), s.Poll.Counts...)
		s.Poll = &p
	}
	return s
}

// Start begins a run and returns the initial snapshot.
func (e *Engine) Start(ctx context.Context, mission string, size int) (State, error) {
	mission = strings.TrimSpace(mission)
	if mission == "" {
		return State{}, fmt.Errorf("mission is required")
	}
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return State{}, fmt.Errorf("a swarm is already running")
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	e.state = idleState(size)
	e.state.ID = fmt.Sprintf("swarm-%d", time.Now().UnixNano())
	e.state.Mission = mission
	e.state.Status = "running"
	e.state.Phase = "recruiting"
	e.state.Started = now()
	e.state.Updated = e.state.Started
	e.running = true
	e.cancel = cancel
	e.seq = 0
	e.calls = 0
	e.startedAt = time.Now()
	e.mu.Unlock()
	e.emit()

	go func() {
		defer func() {
			e.mu.Lock()
			e.running = false
			e.cancel = nil
			// A run that forgets to set a terminal state should not look alive.
			if e.state.Status == "running" {
				e.state.Status = "done"
				e.state.Phase = "done"
			}
			e.state.Updated = now()
			e.mu.Unlock()
			e.emit()
			cancel()
		}()
		e.run(runCtx)
	}()
	return e.Snapshot(), nil
}

// Stop cancels the in-flight run. The run goroutine observes ctx and posts a
// stopped state; the UI does not need a second event.
func (e *Engine) Stop() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Restore installs a persisted snapshot. A run that was interrupted by a
// process exit becomes stopped; it never reappears as a live spinner.
func (e *Engine) Restore(s State) {
	if s.Size == 0 {
		s.Size = 5
	}
	if len(s.Agents) == 0 {
		s.Agents = buildAgents(s.Size)
	}
	if s.Status == "running" {
		s.Status = "stopped"
		s.Phase = "stopped"
	}
	if s.Messages == nil {
		s.Messages = []Message{}
	}
	if s.Artifacts == nil {
		s.Artifacts = []Artifact{}
	}
	e.mu.Lock()
	e.state = s
	e.mu.Unlock()
}

// Steer appends a human message to the live transcript. The next model call
// sees it; there is no restart and no separate chat channel.
func (e *Engine) Steer(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("steer message is required")
	}
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return fmt.Errorf("no swarm is running")
	}
	e.seq++
	e.state.Messages = append(e.state.Messages, Message{
		ID: fmt.Sprintf("m%d", e.seq), Agent: "human", Role: "human", Kind: "steer", Text: text, Time: now(),
	})
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
	return nil
}

// Vote lets the human join the current open poll. A second human vote replaces
// the first instead of stuffing the ballot.
func (e *Engine) Vote(choice int) error {
	e.mu.Lock()
	if !e.running || e.state.Poll == nil || !e.state.Poll.Open {
		e.mu.Unlock()
		return fmt.Errorf("no open poll")
	}
	if choice < 0 || choice >= len(e.state.Poll.Options) {
		e.mu.Unlock()
		return fmt.Errorf("choice out of range")
	}
	votes := e.state.Poll.Votes[:0]
	for _, v := range e.state.Poll.Votes {
		if v.Voter != "human" {
			votes = append(votes, v)
		}
	}
	e.state.Poll.Votes = append(votes, Vote{
		Voter: "human", Choice: choice, Option: e.state.Poll.Options[choice], Reason: "your vote",
	})
	e.state.Poll.Turnout = len(e.state.Poll.Votes)
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
	return nil
}

func (e *Engine) setStatus(status, phase string) {
	e.mu.Lock()
	e.state.Status = status
	e.state.Phase = phase
	e.state.Updated = now()
	if status == "done" || status == "error" || status == "stopped" {
		if !e.startedAt.IsZero() {
			e.state.ElapsedMS = time.Since(e.startedAt).Milliseconds()
		}
	}
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) setPhase(phase string) {
	e.mu.Lock()
	e.state.Phase = phase
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) setAgent(id, status, last string) {
	e.mu.Lock()
	for i := range e.state.Agents {
		if e.state.Agents[i].ID == id {
			e.state.Agents[i].Status = status
			if last != "" {
				e.state.Agents[i].Last = last
			}
			break
		}
	}
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) addMessage(agentID, kind, text string) {
	e.addMessageWithReply(agentID, kind, text, "", "")
}

func (e *Engine) addMessageWithReply(agentID, kind, text, replyTo, directive string) {
	e.mu.Lock()
	roleID := agentID
	for _, a := range e.state.Agents {
		if a.ID == agentID {
			roleID = a.Role
			break
		}
	}
	e.seq++
	e.state.Messages = append(e.state.Messages, Message{
		ID:        fmt.Sprintf("m%d", e.seq),
		Agent:     agentID,
		Role:      roleID,
		Kind:      kind,
		Text:      text,
		Time:      now(),
		ReplyTo:   replyTo,
		Directive: directive,
	})
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) addArtifact(name, kind, content string) {
	e.mu.Lock()
	e.state.Artifacts = append(e.state.Artifacts, Artifact{Name: name, Kind: kind, Content: content})
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) setPoll(p *Poll) {
	e.mu.Lock()
	e.state.Poll = p
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) addVote(v Vote) {
	e.mu.Lock()
	if e.state.Poll != nil {
		e.state.Poll.Votes = append(e.state.Poll.Votes, v)
	}
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) closePoll() *Poll {
	e.mu.Lock()
	p := e.state.Poll
	if p != nil {
		counts := make([]int, len(p.Options))
		for _, v := range p.Votes {
			if v.Choice >= 0 && v.Choice < len(counts) {
				counts[v.Choice]++
			}
		}
		best := 0
		for i := 1; i < len(counts); i++ {
			if counts[i] > counts[best] {
				best = i
			}
		}
		p.Counts = append([]int(nil), counts...)
		p.Turnout = len(p.Votes)
		if p.Turnout > 0 {
			p.Consensus = counts[best] * 100 / p.Turnout
		}
		p.Winner = best
		p.Open = false
		closed := *p
		closed.Options = append([]string(nil), p.Options...)
		closed.Votes = append([]Vote(nil), p.Votes...)
		closed.Counts = append([]int(nil), p.Counts...)
		e.state.Polls = append(e.state.Polls, closed)
	}
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
	if p == nil {
		return &Poll{}
	}
	cp := *p
	return &cp
}

func (e *Engine) fail(ctx context.Context, err error) {
	e.mu.Lock()
	if ctx.Err() != nil {
		e.state.Status = "stopped"
		e.state.Phase = "stopped"
	} else {
		e.state.Status = "error"
		e.state.Phase = "error"
		e.state.Error = err.Error()
	}
	if !e.startedAt.IsZero() {
		e.state.ElapsedMS = time.Since(e.startedAt).Milliseconds()
	}
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) run(ctx context.Context) {
	prov, model := (providers.Provider)(nil), ""
	if e.source != nil {
		prov, model = e.source()
	}
	demo := false
	if prov == nil || strings.EqualFold(prov.Name(), "mock") {
		prov = demoProvider{}
		model = "swarm-demo"
		demo = true
	}
	providerName := prov.Name()
	if demo {
		providerName = "demo"
	}
	e.mu.Lock()
	e.prov, e.model, e.demo = prov, model, demo
	e.state.Provider, e.state.Model = providerName, model
	e.mu.Unlock()

	e.setPhase("ideating")
	ideators := e.roleAgents("ideator")
	e.setRole("ideator", "thinking", "reading the mission")
	ideasText, err := e.complete(ctx, mustRole("ideator"), e.ideaPrompt(ideators))
	if err != nil {
		e.fail(ctx, err)
		return
	}
	ideas := parseIdeas(ideasText)
	if len(ideas) == 0 {
		ideas = []idea{{Agent: ideators[0].ID, Title: "The obvious first move", Pitch: clip(ideasText, 400)}}
	}
	for _, idea := range ideas {
		agent := idea.Agent
		if agent == "" || !e.hasAgent(agent) {
			agent = ideators[0].ID
		}
		e.addMessage(agent, "idea", formatIdea(idea))
	}
	e.setRole("ideator", "ready", "ideas on the table")

	// Keep the first poll readable even when 20 ideators speak.
	if len(ideas) > 6 {
		ideas = ideas[:6]
	}

	// ── Debating phase: inter-agent critique and defense ──────────
	e.setPhase("debating")
	critics := e.roleAgents("critic")
	if len(critics) > 0 && len(ideas) > 0 {
		e.setRole("critic", "thinking", "debating proposals")
		numDebaters := 2
		if len(critics) < numDebaters {
			numDebaters = len(critics)
		}
		for i := 0; i < numDebaters; i++ {
			criticAgent := critics[i]
			targetIdea := ideas[i%len(ideas)]
			debatePrompt := e.debateChallengePrompt(criticAgent, targetIdea)
			challenge, err := e.callAgent(ctx, criticAgent.ID, debatePrompt)
			if err != nil {
				e.fail(ctx, err)
				return
			}
			directive, cleanText := extractDirective(challenge, "[CHALLENGE]")

			responderAgent := targetIdea.Agent
			if !e.hasAgent(responderAgent) {
				ideatorList := e.roleAgents("ideator")
				if len(ideatorList) > 0 {
					responderAgent = ideatorList[0].ID
				}
			}
			e.addMessageWithReply(criticAgent.ID, "debate", cleanText, responderAgent, directive)
			if responderAgent != "" {
				defensePrompt := e.debateDefensePrompt(responderAgent, targetIdea, cleanText)
				defense, err := e.callAgent(ctx, responderAgent, defensePrompt)
				if err != nil {
					e.fail(ctx, err)
					return
				}
				defDirective, defText := extractDirective(defense, "[AMEND]")
				e.addMessageWithReply(responderAgent, "debate", defText, criticAgent.ID, defDirective)
			}
		}
		e.setRole("critic", "ready", "debated")
	}

	options := make([]string, 0, len(ideas))
	for _, idea := range ideas {
		options = append(options, idea.Title)
	}
	e.setPhase("voting")
	poll := &Poll{ID: "direction", Question: "Which direction should the swarm build?", Options: options, Open: true}
	e.setPoll(poll)
	if err := e.collectVotes(ctx, poll); err != nil {
		e.fail(ctx, err)
		return
	}
	closed := e.closePoll()
	winner := 0
	if closed.Winner >= 0 && closed.Winner < len(ideas) {
		winner = closed.Winner
	}
	chosen := ideas[winner]

	e.setPhase("coding")

	// Allocate isolated git worktree if repoDir is a valid git repository
	var wt *execution.Worktree
	e.mu.Lock()
	repoDir := e.repoDir
	e.mu.Unlock()
	if repoDir != "" && execution.IsGitRepo(repoDir) {
		if w, err := execution.CreateWorktree(repoDir, e.state.ID, execution.WorktreeOptions{BranchPrefix: "evolv/swarm-"}); err == nil {
			wt = w
			e.mu.Lock()
			e.worktree = wt
			e.state.Worktree = wt.Path
			e.state.Branch = wt.Branch
			e.mu.Unlock()
			e.emit()
			defer func() {
				if e.state.Status == "error" || e.state.Status == "stopped" {
					_ = execution.RemoveWorktree(repoDir, wt, true)
				}
			}()
		}
	}

	code := make([]string, 0, 3)
	for i, agent := range e.pick("coder", 3) {
		lens := []string{"smallest working implementation", "robust edge cases", "testable seams"}[i%3]
		out, err := e.callAgent(ctx, agent.ID, e.coderPrompt(agent, chosen, lens))
		if err != nil {
			e.fail(ctx, err)
			return
		}
		e.addMessage(agent.ID, "code", out)
		artName := safeName(chosen.Title) + "-" + agent.ID + ".txt"
		e.addArtifact(artName, "code", out)
		code = append(code, agent.Name+":\n"+out)

		if wt != nil && wt.Active {
			clean := ExtractCode(out, "file")
			filePath := filepath.Join(wt.Path, safeName(chosen.Title)+"-"+agent.ID+".txt")
			_ = os.WriteFile(filePath, []byte(clean), 0o644)
		}
	}

	e.setPhase("testing")
	tests := make([]string, 0, 3)
	codeText := strings.Join(code, "\n\n")
	for _, agent := range e.pick("tester", 3) {
		out, err := e.callAgent(ctx, agent.ID, e.testerPrompt(agent, chosen, codeText))
		if err != nil {
			e.fail(ctx, err)
			return
		}
		e.addMessage(agent.ID, "test", out)
		e.addArtifact(safeName(chosen.Title)+"-"+agent.ID+".tests.txt", "test", out)
		tests = append(tests, agent.Name+":\n"+out)
	}
	testText := strings.Join(tests, "\n\n")

	e.setPhase("reviewing")
	if wt != nil && wt.Active {
		if diff, err := execution.DiffWorktree(wt); err == nil && diff != "" {
			e.mu.Lock()
			e.state.Diff = diff
			e.mu.Unlock()
			e.addArtifact("git.diff", "diff", diff)
		}
	}

	reviews := make([]string, 0, 3)
	for _, agent := range e.pick("critic", 3) {
		out, err := e.callAgent(ctx, agent.ID, e.criticPrompt(agent, chosen, codeText, testText))
		if err != nil {
			e.fail(ctx, err)
			return
		}
		e.addMessage(agent.ID, "review", out)
		reviews = append(reviews, agent.Name+":\n"+out)
	}
	reviewText := strings.Join(reviews, "\n\n")

	e.setPhase("shipping")
	shipOptions := []string{"Ship it", "Revise once more"}
	shipPoll := &Poll{ID: "ship", Question: "Ship this direction?", Options: shipOptions, Open: true}
	e.setPoll(shipPoll)
	if err := e.collectVotes(ctx, shipPoll); err != nil {
		e.fail(ctx, err)
		return
	}
	ship := e.closePoll()
	decision := "ship"
	if ship.Winner == 1 {
		decision = "revise"
	}

	if decision == "revise" {
		e.setPhase("revising")
		revised := make([]string, 0, 3)
		for _, agent := range e.pick("coder", 3) {
			out, err := e.callAgent(ctx, agent.ID, e.revisionPrompt(agent, chosen, codeText, testText, reviewText))
			if err != nil {
				e.fail(ctx, err)
				return
			}
			e.addMessage(agent.ID, "revision", out)
			e.addArtifact(safeName(chosen.Title)+"-"+agent.ID+"-revised.txt", "revision", out)
			revised = append(revised, agent.Name+":\n"+out)

			if wt != nil && wt.Active {
				clean := ExtractCode(out, "file")
				filePath := filepath.Join(wt.Path, safeName(chosen.Title)+"-"+agent.ID+"-revised.txt")
				_ = os.WriteFile(filePath, []byte(clean), 0o644)
			}
		}
		codeText = strings.Join(revised, "\n\n")
		retests := make([]string, 0, 3)
		for _, agent := range e.pick("tester", 3) {
			out, err := e.callAgent(ctx, agent.ID, e.testerPrompt(agent, chosen, codeText))
			if err != nil {
				e.fail(ctx, err)
				return
			}
			e.addMessage(agent.ID, "test", out)
			e.addArtifact(safeName(chosen.Title)+"-"+agent.ID+"-retest.txt", "test", out)
			retests = append(retests, agent.Name+":\n"+out)
		}
		testText = strings.Join(retests, "\n\n")

		if wt != nil && wt.Active {
			if diff, err := execution.DiffWorktree(wt); err == nil && diff != "" {
				e.mu.Lock()
				e.state.Diff = diff
				e.mu.Unlock()
				e.addArtifact("git.diff", "diff", diff)
			}
		}
	}

	e.setPhase("judging")
	judge := e.pick("judge", 1)[0]
	final, err := e.callAgent(ctx, judge.ID, e.judgePrompt(judge, chosen, codeText, testText, reviewText, decision))
	if err != nil {
		e.fail(ctx, err)
		return
	}
	e.addMessage(judge.ID, "final", final)
	e.addArtifact("final.md", "final", final)

	if decision == "ship" && wt != nil && wt.Active {
		commitMsg := fmt.Sprintf("feat(swarm): %s", chosen.Title)
		if hash, err := execution.CommitWorktree(wt, commitMsg); err == nil {
			wt.Commit = hash
			e.addMessage(judge.ID, "system", fmt.Sprintf("Committed swarm branch %s (%s)", wt.Branch, hash[:7]))
		}
	}
	for _, role := range Roles {
		e.setRole(role.ID, "done", "done")
	}
	e.setStatus("done", "done")
}

// collectVotes asks one role cohort at a time to return a vote for every one
// of its agents. Five model calls cover a 100-agent poll without pretending
// the other 95 agents did not speak.
func (e *Engine) collectVotes(ctx context.Context, poll *Poll) error {
	type batch struct {
		role   Role
		agents []AgentState
		text   string
		err    error
	}
	batches := make([]batch, len(Roles))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3) // bounded parallel provider calls
	for i, role := range Roles {
		agents := e.roleAgents(role.ID)
		if len(agents) == 0 {
			continue
		}
		batches[i] = batch{role: role, agents: agents}
		e.setRole(role.ID, "thinking", "voting")
		wg.Add(1)
		go func(i int, role Role, agents []AgentState) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			text, err := e.complete(ctx, role, e.batchVotePrompt(role, agents, poll.Question, poll.Options))
			batches[i].text, batches[i].err = text, err
		}(i, role, agents)
	}
	wg.Wait()
	for _, b := range batches {
		if b.err != nil {
			return b.err
		}
		if len(b.agents) == 0 {
			continue
		}
		e.addVotes(parseBatchVotes(b.text, b.agents, poll.Options))
		e.setRole(b.role.ID, "voted", "voted")
	}
	return nil
}

func (e *Engine) callAgent(ctx context.Context, agentID, user string) (string, error) {
	agent, ok := e.agentByID(agentID)
	if !ok {
		return "", fmt.Errorf("unknown agent %q", agentID)
	}
	role, _ := roleByID(agent.Role)
	e.setAgent(agentID, "thinking", "")
	out, err := e.complete(ctx, role, user)
	if err != nil {
		e.setAgent(agentID, "ready", "")
		return "", err
	}
	e.setAgent(agentID, "ready", clip(out, 90))
	return out, nil
}

func (e *Engine) complete(ctx context.Context, role Role, user string) (string, error) {
	if e.demo {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(45 * time.Millisecond):
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	e.mu.Lock()
	prov, model := e.prov, e.model
	e.mu.Unlock()
	var live strings.Builder
	last := time.Time{}
	e.activity(role.ID, "thinking…")
	text, err := prov.Stream(callCtx, model, []providers.Message{
		{Role: "system", Content: rolePrompt(role)},
		{Role: "user", Content: user + "\n\n" + e.transcript()},
	}, func(d providers.Delta) {
		if d.Content == "" {
			return
		}
		live.WriteString(d.Content)
		if now := time.Now(); now.Sub(last) > 120*time.Millisecond {
			last = now
			e.activity(role.ID, clip(live.String(), 180))
		}
	})
	e.mu.Lock()
	e.calls++
	e.state.Calls = e.calls
	e.mu.Unlock()
	e.activity(role.ID, "")
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "(the model returned an empty response)"
	}
	return text, nil
}

func (e *Engine) activity(roleID, text string) {
	if e.publish == nil {
		return
	}
	e.publish("swarm:activity", map[string]string{"role": roleID, "text": text})
}

func (e *Engine) transcript() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	start := len(e.state.Messages) - 10
	if start < 0 {
		start = 0
	}
	var b strings.Builder
	b.WriteString("Shared transcript (most recent last):\n")
	for _, m := range e.state.Messages[start:] {
		b.WriteString(e.agentNameLocked(m.Agent) + ": " + clip(m.Text, 700) + "\n")
	}
	return b.String()
}

func (e *Engine) hasAgent(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.state.Agents {
		if a.ID == id {
			return true
		}
	}
	return false
}

func (e *Engine) agentByID(id string) (AgentState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.state.Agents {
		if a.ID == id {
			return a, true
		}
	}
	return AgentState{}, false
}

func (e *Engine) agentNameLocked(id string) string {
	for _, a := range e.state.Agents {
		if a.ID == id {
			return a.Name
		}
	}
	return id
}

func (e *Engine) roleAgents(roleID string) []AgentState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []AgentState{}
	for _, a := range e.state.Agents {
		if a.Role == roleID {
			out = append(out, a)
		}
	}
	return out
}

func (e *Engine) pick(roleID string, max int) []AgentState {
	agents := e.roleAgents(roleID)
	if len(agents) > max {
		agents = agents[:max]
	}
	return agents
}

func (e *Engine) setRole(roleID, status, last string) {
	e.mu.Lock()
	for i := range e.state.Agents {
		if e.state.Agents[i].Role == roleID {
			e.state.Agents[i].Status = status
			if last != "" {
				e.state.Agents[i].Last = last
			}
		}
	}
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) addVotes(votes []Vote) {
	e.mu.Lock()
	if e.state.Poll != nil {
		e.state.Poll.Votes = append(e.state.Poll.Votes, votes...)
		for i := range e.state.Agents {
			for _, v := range votes {
				if v.Voter == e.state.Agents[i].ID {
					e.state.Agents[i].Status = "voted"
					e.state.Agents[i].Last = "voted " + v.Option
				}
			}
		}
	}
	e.state.Updated = now()
	e.mu.Unlock()
	e.emit()
}

func (e *Engine) batchVotePrompt(role Role, agents []AgentState, question string, options []string) string {
	var b strings.Builder
	b.WriteString("Poll: " + question + "\nOptions:\n")
	for i, option := range options {
		fmt.Fprintf(&b, "%d. %s\n", i+1, option)
	}
	e.mu.Lock()
	var debateSnippets []string
	for _, m := range e.state.Messages {
		if m.Kind == "debate" {
			prefix := m.Directive
			if prefix == "" {
				prefix = "[DEBATE]"
			}
			debateSnippets = append(debateSnippets, fmt.Sprintf("- %s %s: %s", m.Agent, prefix, clip(m.Text, 140)))
		}
	}
	e.mu.Unlock()
	if len(debateSnippets) > 0 {
		b.WriteString("\nDebate highlights:\n")
		for _, s := range debateSnippets {
			b.WriteString(s + "\n")
		}
	}
	b.WriteString("\nAgents in this cohort:\n")
	for _, agent := range agents {
		b.WriteString("- " + agent.ID + " (" + agent.Name + ")\n")
	}
	b.WriteString("\nEvery agent votes independently. Reply with JSON only: {\"votes\":[{\"agent\":\"<id>\",\"choice\":<number>,\"reason\":\"one short sentence\"}]}. Include exactly one vote per agent above.")
	return b.String()
}

var directiveRegex = regexp.MustCompile(`^(?i)\[(CHALLENGE|AMEND|PROPOSE|CONCEDE|VOTE)\]\s*`)
var inlineDirectiveRegex = regexp.MustCompile(`(?i)\[(CHALLENGE|AMEND|PROPOSE|CONCEDE)\]`)

func extractDirective(text, fallback string) (string, string) {
	text = strings.TrimSpace(text)
	if m := directiveRegex.FindStringSubmatch(text); len(m) > 1 {
		clean := strings.TrimSpace(text[len(m[0]):])
		return "[" + strings.ToUpper(m[1]) + "]", clean
	}
	if loc := inlineDirectiveRegex.FindStringSubmatchIndex(text); loc != nil {
		d := "[" + strings.ToUpper(text[loc[2]:loc[3]]) + "]"
		clean := text[:loc[0]] + text[loc[1]:]
		return d, strings.TrimSpace(clean)
	}
	return fallback, text
}

func (e *Engine) debateChallengePrompt(agent AgentState, target idea) string {
	return fmt.Sprintf("Mission: %s\nTarget proposal by %s: %s — %s\n\nYou are %s (Critic). Provide a sharp, constructive challenge to this proposal: identify potential failure modes, hidden complexity, or missing test/security guarantees. Prepend your reply with [CHALLENGE]. Keep it concise (under 3 sentences).",
		e.mission(), target.Agent, target.Title, target.Pitch, agent.Name)
}

func (e *Engine) debateDefensePrompt(agentID string, target idea, challenge string) string {
	return fmt.Sprintf("Mission: %s\nYour proposal: %s — %s\nCritic challenge: %s\n\nYou are %s. Defend or amend your proposal to directly address the critique. If amending, prepend your reply with [AMEND]. Keep it concise (under 3 sentences).",
		e.mission(), target.Title, target.Pitch, challenge, agentID)
}

func (e *Engine) ideaPrompt(agents []AgentState) string {
	var b strings.Builder
	b.WriteString("Mission: " + e.mission() + "\n\nIdeator cohort:\n")
	for _, a := range agents {
		b.WriteString("- " + a.ID + "\n")
	}
	b.WriteString("\nEach ideator proposes exactly one distinct, buildable direction. Reply with JSON only: {\"ideas\":[{\"agent\":\"<id>\",\"title\":\"...\",\"pitch\":\"...\"}]}. Include one idea per ideator above.")
	return b.String()
}

func (e *Engine) coderPrompt(agent AgentState, chosen idea, lens string) string {
	return "Mission: " + e.mission() + "\nChosen direction: " + chosen.Title + " — " + chosen.Pitch + "\nYou are " + agent.Name + ". Your lens: " + lens + ".\n\nBuild the smallest real implementation from that lens. Return one concrete artifact with code or exact build steps; no preamble."
}

func (e *Engine) testerPrompt(agent AgentState, chosen idea, code string) string {
	return "Mission: " + e.mission() + "\nDirection: " + chosen.Title + "\nImplementation:\n" + clip(code, 5000) + "\n\nYou are " + agent.Name + ". Write the tests and edge cases that would prove or break this. Be adversarial and concrete."
}

func (e *Engine) criticPrompt(agent AgentState, chosen idea, code, tests string) string {
	return "Mission: " + e.mission() + "\nDirection: " + chosen.Title + "\nCode:\n" + clip(code, 3500) + "\nTests:\n" + clip(tests, 3000) + "\n\nYou are " + agent.Name + ". Name the strongest part, the biggest risk, and one change required before shipping."
}

func (e *Engine) revisionPrompt(agent AgentState, chosen idea, code, tests, review string) string {
	return "Mission: " + e.mission() + "\nDirection: " + chosen.Title + "\nCurrent code:\n" + clip(code, 3500) + "\nTests:\n" + clip(tests, 2000) + "\nCritique:\n" + clip(review, 2200) + "\n\nYou are " + agent.Name + ". The colony voted to revise. Apply the critique with the smallest coherent change and return the revised implementation. No preamble."
}

func (e *Engine) judgePrompt(agent AgentState, chosen idea, code, tests, review, decision string) string {
	return "Mission: " + e.mission() + "\nDirection: " + chosen.Title + "\nCode:\n" + clip(code, 3000) + "\nTests:\n" + clip(tests, 2500) + "\nCritique:\n" + clip(review, 2500) + "\nThe colony voted: " + decision + ".\n\nYou are " + agent.Name + ". Write the final answer: what the swarm decided, what it built, how it was verified, and the next concrete step. Be concise; do not repeat the whole code."
}

func (e *Engine) mission() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Mission
}

func mustRole(id string) Role {
	r, _ := roleByID(id)
	return r
}

func rolePrompt(r Role) string {
	switch r.ID {
	case "ideator":
		return "You are the Ideator of a five-agent swarm. You find the shape of the problem and propose three distinct, buildable directions. Prefer sharp, specific ideas over generic ones. Do not write code."
	case "coder":
		return "You are the Coder of a five-agent swarm. You turn the chosen direction into the smallest real implementation. Be concrete, self-contained and honest about tradeoffs."
	case "tester":
		return "You are the Tester of a five-agent swarm. You break what the Coder built. Name concrete tests, edge cases, failure modes and the exact check that would catch them."
	case "critic":
		return "You are the Critic of a five-agent swarm. You are constructive but hard to impress. Name the strongest part, the biggest risk, and one change required before shipping."
	case "judge":
		return "You are the Judge of a five-agent swarm. You synthesize the colony's work into one decision. Be concise, truthful and actionable. Do not invent verification that did not happen."
	default:
		return "You are a member of a five-agent swarm. Be concrete."
	}
}

type idea struct {
	Agent string `json:"agent,omitempty"`
	Title string `json:"title"`
	Pitch string `json:"pitch"`
}

type ideaEnvelope struct {
	Ideas []idea `json:"ideas"`
}

func parseIdeas(text string) []idea {
	var env ideaEnvelope
	if decodeJSON(text, &env) == nil && len(env.Ideas) > 0 {
		out := make([]idea, 0, len(env.Ideas))
		for _, i := range env.Ideas {
			i.Agent = strings.TrimSpace(i.Agent)
			i.Title = strings.TrimSpace(i.Title)
			i.Pitch = strings.TrimSpace(i.Pitch)
			if i.Title != "" {
				out = append(out, i)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	var out []idea
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "-*•0123456789.) "))
		if line == "" || len(out) == 3 {
			continue
		}
		title, pitch := line, ""
		if a, b, ok := strings.Cut(line, ":"); ok {
			title, pitch = strings.TrimSpace(a), strings.TrimSpace(b)
		} else if a, b, ok := strings.Cut(line, "—"); ok {
			title, pitch = strings.TrimSpace(a), strings.TrimSpace(b)
		}
		if title != "" {
			out = append(out, idea{Title: title, Pitch: pitch})
		}
	}
	return out
}

func parseBatchVotes(text string, agents []AgentState, options []string) []Vote {
	var env struct {
		Votes []struct {
			Agent  string `json:"agent"`
			Choice any    `json:"choice"`
			Reason string `json:"reason"`
		} `json:"votes"`
	}
	if decodeJSON(text, &env) == nil && len(env.Votes) > 0 {
		byAgent := map[string]Vote{}
		for _, v := range env.Votes {
			choice := choiceNumber(v.Choice) - 1
			if choice < 0 || choice >= len(options) {
				choice = 0
			}
			byAgent[v.Agent] = Vote{Voter: v.Agent, Choice: choice, Option: options[choice], Reason: strings.TrimSpace(v.Reason)}
		}
		out := make([]Vote, 0, len(agents))
		for i, agent := range agents {
			if v, ok := byAgent[agent.ID]; ok {
				out = append(out, v)
				continue
			}
			choice := i % len(options)
			out = append(out, Vote{Voter: agent.ID, Choice: choice, Option: options[choice], Reason: "inferred from the cohort vote"})
		}
		return out
	}
	// A sloppy model gets a deterministic fallback, never a missing agent.
	out := make([]Vote, 0, len(agents))
	for i, agent := range agents {
		choice := i % len(options)
		out = append(out, Vote{Voter: agent.ID, Choice: choice, Option: options[choice], Reason: clip(text, 140)})
	}
	return out
}

func choiceNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		var out int
		fmt.Sscanf(n, "%d", &out)
		return out
	}
	return 1
}

func parseVote(text string, options []string) (int, string) {
	var env struct {
		Choice any    `json:"choice"`
		Reason string `json:"reason"`
	}
	if decodeJSON(text, &env) == nil && len(options) > 0 {
		choice := 0
		switch v := env.Choice.(type) {
		case float64:
			choice = int(v) - 1
		case string:
			fmt.Sscanf(v, "%d", &choice)
			choice--
		}
		if choice < 0 || choice >= len(options) {
			choice = 0
		}
		return choice, strings.TrimSpace(env.Reason)
	}
	lower := strings.ToLower(text)
	for i, option := range options {
		if strings.Contains(lower, strings.ToLower(option)) {
			return i, clip(text, 180)
		}
	}
	return 0, clip(text, 180)
}

func decodeJSON(text string, v any) error {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return fmt.Errorf("no json object")
	}
	return json.NewDecoder(strings.NewReader(text[start:])).Decode(v)
}

func formatIdea(i idea) string {
	if i.Pitch == "" {
		return i.Title
	}
	return i.Title + " — " + i.Pitch
}

func safeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "swarm"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ExtractCode isolates clean code from markdown code fences (e.g. ```html ... ```)
// or returns the original content if no fences exist.
func ExtractCode(content, target string) string {
	content = strings.TrimSpace(content)
	if !strings.Contains(content, "```") {
		return content
	}
	lines := strings.Split(content, "\n")
	var inFence bool
	var fenceLang string
	var current strings.Builder
	type block struct {
		lang string
		code string
	}
	var blocks []block

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				blocks = append(blocks, block{lang: fenceLang, code: strings.TrimSpace(current.String())})
				current.Reset()
				inFence = false
				fenceLang = ""
			} else {
				inFence = true
				fenceLang = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "```")))
			}
			continue
		}
		if inFence {
			current.WriteString(line)
			current.WriteByte('\n')
		}
	}

	if len(blocks) == 0 {
		return content
	}

	target = strings.ToLower(strings.TrimSpace(target))
	if target == "component" || target == "organ" {
		for _, b := range blocks {
			if b.lang == "html" || b.lang == "htm" {
				return b.code
			}
		}
	}
	if target == "tool" {
		for _, b := range blocks {
			if b.lang == "js" || b.lang == "javascript" {
				return b.code
			}
		}
	}

	best := blocks[0]
	for _, b := range blocks[1:] {
		if len(b.code) > len(best.code) {
			best = b
		}
	}
	return best.code
}

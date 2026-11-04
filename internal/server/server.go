// Package server wires the organism to the browser: static serving of the
// living UI, a WebSocket hub mirroring the bus, and a small JSON API
// (chat, settings, models, files).
package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dxn111/EVOLV/internal/agent"
	"github.com/dxn111/EVOLV/internal/bus"
	"github.com/dxn111/EVOLV/internal/candidate"
	"github.com/dxn111/EVOLV/internal/execution"
	"github.com/dxn111/EVOLV/internal/lint"
	"github.com/dxn111/EVOLV/internal/providers"
	"github.com/dxn111/EVOLV/internal/swarm"
	"github.com/dxn111/EVOLV/internal/update"
	"github.com/dxn111/EVOLV/internal/workspace"
)

// Server is the organism's interface to the world.
type Server struct {
	ws            *workspace.W
	bus           *bus.Bus
	ag            *agent.Agent
	reg           *providers.Registry
	mux           *http.ServeMux
	upgr          websocket.Upgrader
	mu            sync.Mutex
	live          int
	update        *update.Info // newer release, if any
	genMu         sync.Mutex
	genCancel     context.CancelFunc // cancels the in-flight generation (stop button)
	agentMu       sync.RWMutex
	agents        map[string]*agent.Agent
	profiles      map[string]agent.Profile
	activeAgent   string
	startedAt     time.Time
	requests      atomic.Int64
	errors        atomic.Int64
	wsMu          sync.Mutex
	conns         map[*websocket.Conn]struct{}
	swarm         *swarm.Engine
	swarmMu       sync.Mutex
	swarmArchived map[string]bool
	done          chan struct{}
	closeOnce     sync.Once
	latency       latencyHistogram
	runStats      *runMetrics
	execStore     *execution.Store
	execMu        sync.Mutex
}

// New builds the server.
func New(ws *workspace.W, b *bus.Bus, ag *agent.Agent, reg *providers.Registry) *Server {
	s := &Server{
		ws: ws, bus: b, ag: ag, reg: reg, mux: http.NewServeMux(), agents: map[string]*agent.Agent{}, profiles: map[string]agent.Profile{}, activeAgent: "main",
		startedAt: time.Now(),
		conns:     map[*websocket.Conn]struct{}{},
		done:      make(chan struct{}),
		upgr: websocket.Upgrader{
			CheckOrigin: allowedOrigin, // same-origin browser sockets only
		},
	}
	s.swarmArchived = map[string]bool{}
	s.runStats = newRunMetrics()
	s.swarm = swarm.New(func() (providers.Provider, string) {
		if s.ag == nil {
			return nil, ""
		}
		return s.ag.ProviderSnapshot()
	}, func(event string, payload any) {
		s.bus.Publish(event, payload)
		if state, ok := payload.(swarm.State); ok && event == "swarm:state" {
			s.saveSwarmState(state)
		}
	})
	if s.ws != nil {
		s.swarm.SetRepoDir(s.ws.Root)
	}
	s.loadSwarmState()
	s.loadAgentProfiles()
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleUI)
	s.mux.HandleFunc("/ws", s.handleWS)
	s.mux.HandleFunc("/api/chat", s.handleChat)
	s.mux.HandleFunc("/api/swarm", s.handleSwarm)
	s.mux.HandleFunc("/api/swarm/stop", s.handleSwarmStop)
	s.mux.HandleFunc("/api/swarm/steer", s.handleSwarmSteer)
	s.mux.HandleFunc("/api/swarm/vote", s.handleSwarmVote)
	s.mux.HandleFunc("/api/swarm/runs", s.handleSwarmRuns)
	s.mux.HandleFunc("/api/swarm/history", s.handleSwarmHistory)
	s.mux.HandleFunc("/api/runs", s.handleRuns)
	s.mux.HandleFunc("/api/runs/detail", s.handleRunDetail)
	s.mux.HandleFunc("/api/runs/events", s.handleRunEvents)
	s.mux.HandleFunc("/api/runs/create", s.handleRunCreate)
	s.mux.HandleFunc("/api/runs/transition", s.handleRunTransition)
	s.mux.HandleFunc("/api/runs/cancel", s.handleRunCancel)
	s.mux.HandleFunc("/api/runs/retry", s.handleRunRetry)
	s.mux.HandleFunc("/api/runs/worktree", s.handleRunWorktree)
	s.mux.HandleFunc("/api/plan/compile", s.handlePlanCompile)
	s.mux.HandleFunc("/api/plan/start", s.handlePlanStart)
	s.mux.HandleFunc("/api/runs/insights", s.handleRunInsights)
	s.mux.HandleFunc("/api/organs", s.handleOrgans)
	s.mux.HandleFunc("/api/organs/versions", s.handleGenomeVersions)
	s.mux.HandleFunc("/api/organs/rollback", s.handleGenomeRollback)
	s.mux.HandleFunc("/api/organs/status", s.handleGenomeStatus)
	s.mux.HandleFunc("/api/organs/backfill", s.handleGenomeBackfill)
	s.mux.HandleFunc("/api/lessons", s.handleLessons)
	s.mux.HandleFunc("/api/swarm/apply", s.handleSwarmApply)
	s.mux.HandleFunc("/api/history", s.handleHistory)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
	s.mux.HandleFunc("/api/models", s.handleModels)
	s.mux.HandleFunc("/api/evolution", s.handleEvolution)
	s.mux.HandleFunc("/api/plan", s.handlePlan)
	s.mux.HandleFunc("/api/replay", s.handleReplay)
	s.mux.HandleFunc("/api/mind", s.handleMind)
	s.mux.HandleFunc("/api/widgets", s.handleWidgets)
	s.mux.HandleFunc("/api/creations", s.handleCreations)
	s.mux.HandleFunc("/api/lint", s.handleLint)
	s.mux.HandleFunc("/api/pack/export", s.handlePackExport)
	s.mux.HandleFunc("/api/pack/inspect", s.handlePackInspect)
	s.mux.HandleFunc("/api/pack/import", s.handlePackImport)
	s.mux.HandleFunc("/api/agents", s.handleAgents)
	s.mux.HandleFunc("/api/candidates", s.handleCandidates)
	s.mux.HandleFunc("/api/genome", s.handleGenome)
	s.mux.HandleFunc("/api/files", s.handleFiles)
	s.mux.HandleFunc("/api/reset", s.handleReset)
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/metrics", s.handleMetrics)
	s.mux.HandleFunc("/api/update", s.handleUpdate)
	s.mux.HandleFunc("/api/update/run", s.handleUpdateRun)
	s.mux.HandleFunc("/api/snapshots", s.handleSnapshots)
	s.mux.HandleFunc("/api/snapshots/restore", s.handleSnapshotRestore)
	s.mux.HandleFunc("/api/snapshots/diff", s.handleSnapshotDiff)
	s.mux.HandleFunc("/api/dream", s.handleDream)
	s.mux.HandleFunc("/api/stop", s.handleStop)
	s.mux.HandleFunc("/api/export", s.handleExport)
	s.mux.HandleFunc("/api/wb/root", s.handleWbRoot)
	s.mux.HandleFunc("/api/wb/tree", s.handleWbTree)
	s.mux.HandleFunc("/api/wb/read", s.handleWbRead)
	s.mux.HandleFunc("/api/wb/write", s.handleWbWrite)
	s.mux.HandleFunc("/api/wb/mkdir", s.handleWbMkdir)
	s.mux.HandleFunc("/api/wb/delete", s.handleWbDelete)
	s.mux.HandleFunc("/api/wb/rename", s.handleWbRename)
	s.mux.HandleFunc("/api/wb/search", s.handleWbSearch)
	s.WatchReleases()
	if s.ag != nil {
		s.ag.StartDreaming() // the organism wakes itself when idle
	}
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.withMiddleware(s.mux) }

// Close retires live WebSocket sessions and cancels an in-flight generation.
// HTTP shutdown itself is owned by the caller so it can drain requests.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.done) })
	s.wsMu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.wsMu.Unlock()

	s.genMu.Lock()
	if s.genCancel != nil {
		s.genCancel()
		s.genCancel = nil
	}
	s.genMu.Unlock()

	if s.swarm != nil {
		s.swarm.Stop()
	}

	s.execMu.Lock()
	if s.execStore != nil {
		_ = s.execStore.CloseAll()
		s.execStore = nil
	}
	s.execMu.Unlock()
}

// liveCount tracks WS clients (delta +1/-1, returns new total).
func (s *Server) liveCount(delta int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live += delta
	return s.live
}

// Listen binds and returns the chosen port.
func (s *Server) Listen(prefer int) (int, net.Listener, error) {
	port := workspace.PickPort(prefer)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return 0, nil, err
	}
	return port, ln, nil
}

// ---------- UI (the living frontend, served straight from disk) ----------

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	rel := ""
	switch {
	case r.URL.Path == "/":
		rel = workspace.UIRel() + "/index.html"
	case r.URL.Path == "/swarm" || r.URL.Path == "/swarm/" || r.URL.Path == "/swarm.html":
		rel = workspace.UIRel() + "/swarm.html" // coming-soon preview, a real page not a modal
	case strings.HasPrefix(r.URL.Path, "/ui/"):
		rel = workspace.UIRel() + "/" + strings.TrimPrefix(r.URL.Path, "/ui/")
	case r.URL.Path == "/style.css" || r.URL.Path == "/app.js" || r.URL.Path == "/runs.js" || r.URL.Path == "/organs.js":
		rel = workspace.UIRel() + r.URL.Path // shell assets live beside the genome
	default:
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(s.ws.Root, filepath.Clean(rel))
	if !strings.HasPrefix(p, s.ws.Root) {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		http.Error(w, "genome file missing: "+rel, 404)
		return
	}
	switch filepath.Ext(p) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".json":
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Cache-Control", "no-store") // the body is always changing
	w.Write(data)
}

// ---------- WebSocket hub ----------

// WebSocket house rules: a browser may send small control frames, never a
// multi-megabyte payload; a dead tab is reaped by ping/pong deadlines; a slow
// consumer cannot block the bus because writes have their own deadline.
const (
	wsMaxMessage  = 64 << 10
	wsReadWindow  = 90 * time.Second
	wsWriteWindow = 10 * time.Second
)

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgr.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade failed: %v", err)
		return
	}
	conn.SetReadLimit(wsMaxMessage)
	writeJSON := func(v any) error {
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWindow))
		return conn.WriteJSON(v)
	}
	log.Printf("[ws] client connected (%d live)", s.liveCount(1))
	s.wsMu.Lock()
	s.conns[conn] = struct{}{}
	s.wsMu.Unlock()
	defer func() {
		s.wsMu.Lock()
		delete(s.conns, conn)
		s.wsMu.Unlock()
		log.Printf("[ws] client left (%d live)", s.liveCount(-1))
		conn.Close()
	}()

	sub, replay := s.bus.Subscribe()
	defer s.bus.Unsubscribe(sub)

	for _, ev := range replay {
		if err := writeJSON(ev); err != nil {
			return
		}
	}
	if s.ag != nil {
		provider, model := s.ag.ProviderModel()
		if raw, err := json.Marshal(map[string]any{
			"agent":      map[string]any{"model": model, "provider": provider},
			"evolutions": s.ag.EvolutionCount(),
		}); err == nil {
			_ = writeJSON(bus.Event{Type: "hello", Payload: raw, Time: time.Now().UTC().Format(time.RFC3339Nano)})
		}
	}

	// reader: ping/pong liveness. Browsers answer pings automatically; a
	// client that goes silent for wsReadWindow is disconnected.
	_ = conn.SetReadDeadline(time.Now().Add(wsReadWindow))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsReadWindow))
	})
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(wsReadWindow))
		}
	}()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev := <-sub.Events():
			if err := writeJSON(ev); err != nil {
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWindow))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-s.done:
			return
		case <-sub.Closed():
			return
		}
	}
}

// ---------- API ----------

type chatReq struct {
	Message string `json:"message"`
	Agent   string `json:"agent,omitempty"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		http.Error(w, `{"error":"message required"}`, 400)
		return
	}
	target := s.agentFor(req.Agent)
	if target == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent runtime unavailable")
		return
	}
	// Claim the single generation slot before replying, so two simultaneous
	// chats cannot interleave one history or race the stop button.
	s.genMu.Lock()
	if s.genCancel != nil {
		s.genMu.Unlock()
		writeJSONError(w, http.StatusConflict, "a generation is already running")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	s.genCancel = cancel
	s.genMu.Unlock()

	go func() {
		defer func() {
			s.genMu.Lock()
			s.genCancel = nil
			s.genMu.Unlock()
			cancel()
			if rec := recover(); rec != nil {
				log.Printf("chat panic: %v", rec)
				s.bus.Publish("agent:token", map[string]string{"text": "\n\n⚠️ kernel hiccup — evolution continues."})
			}
		}()
		// detached context: the organism keeps working after the HTTP reply
		s.bus.Publish("agent:active", map[string]string{"agent": target.ProfileID()})
		target.Chat(ctx, req.Message)
		s.bus.Publish("agent:phase", map[string]string{"phase": "idle"})
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	target := s.agentFor(r.URL.Query().Get("agent"))
	if target == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent runtime unavailable")
		return
	}
	limit, offset := 50, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeJSONError(w, http.StatusBadRequest, "limit must be 1..200")
			return
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "offset must be >= 0")
			return
		}
		offset = n
	}
	messages, total := target.HistoryPage(limit, offset)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"messages": messages,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

func (s *Server) swarmStatePath() string {
	return filepath.Join(s.ws.Dir(workspace.WorkspaceDir), "swarm", "last.json")
}

func (s *Server) swarmRunsDir() string {
	return filepath.Join(s.ws.Dir(workspace.WorkspaceDir), "swarm", "runs")
}

func (s *Server) saveSwarmState(st swarm.State) {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	path := s.swarmStatePath()
	s.swarmMu.Lock()
	defer s.swarmMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)

	// Archive each terminal run once, keyed by its id.
	if st.ID == "" || s.swarmArchived[st.ID] {
		return
	}
	if st.Status != "done" && st.Status != "stopped" && st.Status != "error" {
		return
	}
	dir := s.swarmRunsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	runPath := filepath.Join(dir, st.ID+".json")
	runTmp := runPath + ".tmp"
	if os.WriteFile(runTmp, raw, 0o644) == nil && os.Rename(runTmp, runPath) == nil {
		s.swarmArchived[st.ID] = true
	}
}

func (s *Server) loadSwarmState() {
	raw, err := os.ReadFile(s.swarmStatePath())
	if err != nil {
		return
	}
	var st swarm.State
	if json.Unmarshal(raw, &st) != nil || st.ID == "" {
		return
	}
	s.swarm.Restore(st)
}

type swarmRunSummary struct {
	ID        string `json:"id"`
	Mission   string `json:"mission"`
	Status    string `json:"status"`
	Phase     string `json:"phase"`
	Size      int    `json:"size"`
	Calls     int    `json:"calls"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Started   string `json:"started"`
	Updated   string `json:"updated"`
}

func (s *Server) handleSwarmRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	entries, _ := os.ReadDir(s.swarmRunsDir())
	runs := make([]swarmRunSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.swarmRunsDir(), entry.Name()))
		if err != nil {
			continue
		}
		var st swarm.State
		if json.Unmarshal(raw, &st) != nil || st.ID == "" {
			continue
		}
		runs = append(runs, swarmRunSummary{
			ID: st.ID, Mission: st.Mission, Status: st.Status, Phase: st.Phase,
			Size: st.Size, Calls: st.Calls, ElapsedMS: st.ElapsedMS,
			Started: st.Started, Updated: st.Updated,
		})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Started > runs[j].Started })
	if len(runs) > 30 {
		runs = runs[:30]
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"runs": runs})
}

func (s *Server) handleSwarmHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := filepath.Base(strings.TrimSpace(r.URL.Query().Get("id")))
	if !strings.HasPrefix(id, "swarm-") || strings.ContainsAny(id, "/\\") {
		writeJSONError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	raw, err := os.ReadFile(filepath.Join(s.swarmRunsDir(), id+".json"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "run not found")
		return
	}
	var st swarm.State
	if json.Unmarshal(raw, &st) != nil || st.ID == "" {
		writeJSONError(w, http.StatusInternalServerError, "run is unreadable")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(st)
}

func (s *Server) handleSwarm(w http.ResponseWriter, r *http.Request) {
	if s.swarm == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "swarm runtime unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(s.swarm.Snapshot())
	case http.MethodPost:
		var in struct {
			Mission string `json:"mission"`
			Agents  int    `json:"agents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Mission) == "" {
			writeJSONError(w, http.StatusBadRequest, "mission is required")
			return
		}
		st, err := s.swarm.Start(r.Context(), in.Mission, in.Agents)
		if err != nil {
			code := http.StatusBadRequest
			if strings.Contains(err.Error(), "already running") {
				code = http.StatusConflict
			}
			writeJSONError(w, code, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(st)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSwarmSteer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
		writeJSONError(w, http.StatusBadRequest, "message is required")
		return
	}
	if s.swarm == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "swarm runtime unavailable")
		return
	}
	if err := s.swarm.Steer(in.Message); err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleSwarmVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Choice int `json:"choice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "choice is required")
		return
	}
	if s.swarm == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "swarm runtime unavailable")
		return
	}
	if err := s.swarm.Vote(in.Choice); err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleSwarmStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.swarm != nil {
		s.swarm.Stop()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func sanitizeSafeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else if b.Len() > 0 && b.String()[b.Len()-1] != '-' {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func (s *Server) handleSwarmApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Artifact string `json:"artifact"`
		Content  string `json:"content"`
		Target   string `json:"target"` // component | organ | tool | skill | project | file
		Name     string `json:"name"`
		Path     string `json:"path"`
		Desc     string `json:"desc"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	content := in.Content
	if content == "" {
		if in.Artifact == "" {
			writeJSONError(w, http.StatusBadRequest, "artifact name or content is required")
			return
		}
		if s.swarm == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "swarm runtime unavailable")
			return
		}
		art, found := s.swarm.ArtifactByName(in.Artifact)
		if !found {
			for _, a := range s.swarm.Snapshot().Artifacts {
				if a.Name == in.Artifact {
					art = a
					found = true
					break
				}
			}
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "artifact not found in swarm")
			return
		}
		content = art.Content
	}
	if strings.TrimSpace(content) == "" {
		writeJSONError(w, http.StatusBadRequest, "artifact content is empty")
		return
	}

	target := strings.ToLower(strings.TrimSpace(in.Target))
	if target == "" {
		target = "component"
	}

	cleanCode := swarm.ExtractCode(content, target)
	name := strings.TrimSpace(in.Name)
	if name == "" && in.Artifact != "" {
		name = in.Artifact
	}

	switch target {
	case "component", "organ":
		base := sanitizeSafeName(strings.TrimSuffix(name, filepath.Ext(name)))
		if base == "" {
			base = "swarm-organ"
		}
		fileName := base + ".html"
		destPath := filepath.Join(s.ws.Dir(workspace.ComponentsDir), fileName)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(destPath, []byte(cleanCode), 0o644); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		desc := strings.TrimSpace(in.Desc)
		if desc == "" {
			desc = "Swarm organ mounted to canvas"
		}
		rel := "workspace/ui/components/" + fileName
		s.ws.AppendEvolution(map[string]any{
			"kind":        "component",
			"name":        base,
			"path":        rel,
			"description": desc,
			"source":      "swarm",
		})
		if s.bus != nil {
			s.bus.Publish("fs:change", map[string]string{
				"path":   rel,
				"zone":   "components",
				"kind":   "write",
				"source": "swarm",
			})
			s.bus.Publish("evolution", map[string]any{
				"kind":        "component",
				"name":        base,
				"path":        rel,
				"description": desc,
				"source":      "swarm",
			})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"target":  "component",
			"name":    base,
			"path":    rel,
			"bytes":   len(cleanCode),
			"message": "Component mounted to canvas",
		})

	case "tool":
		base := sanitizeSafeName(strings.TrimSuffix(name, filepath.Ext(name)))
		if base == "" {
			base = "swarm-tool"
		}
		fileName := base + ".js"
		destPath := filepath.Join(s.ws.Dir(workspace.ToolsDir), fileName)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(destPath, []byte(cleanCode), 0o644); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		desc := strings.TrimSpace(in.Desc)
		if desc == "" {
			desc = "Swarm tool evolved"
		}
		rel := "workspace/tools/" + fileName
		s.ws.AppendEvolution(map[string]any{
			"kind":        "tool",
			"name":        base,
			"path":        rel,
			"description": desc,
			"source":      "swarm",
		})
		if s.ag != nil {
			s.ag.ReloadGrownCode()
		}
		if s.bus != nil {
			s.bus.Publish("fs:change", map[string]string{
				"path":   rel,
				"zone":   "tools",
				"kind":   "write",
				"source": "swarm",
			})
			s.bus.Publish("evolution", map[string]any{
				"kind":        "tool",
				"name":        base,
				"path":        rel,
				"description": desc,
				"source":      "swarm",
			})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"target":  "tool",
			"name":    base,
			"path":    rel,
			"bytes":   len(cleanCode),
			"message": "Tool compiled into sandbox",
		})

	case "skill":
		base := sanitizeSafeName(strings.TrimSuffix(name, filepath.Ext(name)))
		if base == "" {
			base = "swarm-skill"
		}
		fileName := base + ".md"
		destPath := filepath.Join(s.ws.Dir(workspace.SkillsDir), fileName)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(destPath, []byte(cleanCode), 0o644); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		desc := strings.TrimSpace(in.Desc)
		if desc == "" {
			desc = "Swarm skill learned"
		}
		rel := "workspace/skills/" + fileName
		s.ws.AppendEvolution(map[string]any{
			"kind":        "skill",
			"name":        base,
			"path":        rel,
			"description": desc,
			"source":      "swarm",
		})
		if s.bus != nil {
			s.bus.Publish("fs:change", map[string]string{
				"path":   rel,
				"zone":   "skills",
				"kind":   "write",
				"source": "swarm",
			})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"target":  "skill",
			"name":    base,
			"path":    rel,
			"bytes":   len(cleanCode),
			"message": "Skill added to organism",
		})

	case "project":
		projRoot := ""
		if s.ws != nil && s.ws.Config != nil {
			projRoot = strings.TrimSpace(s.ws.Config.ProjectDir)
		}
		if projRoot == "" {
			writeJSONError(w, http.StatusBadRequest, "no project bound to workspace")
			return
		}
		targetPath := strings.TrimSpace(in.Path)
		if targetPath == "" {
			targetPath = name
		}
		targetPath = strings.TrimSpace(targetPath)
		if strings.HasPrefix(targetPath, "/") || strings.HasPrefix(targetPath, "\\") || filepath.IsAbs(targetPath) {
			writeJSONError(w, http.StatusBadRequest, "path must be relative to project root")
			return
		}
		cleanRel := filepath.Clean(filepath.FromSlash(targetPath))
		if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
			writeJSONError(w, http.StatusBadRequest, "path escapes project root")
			return
		}
		destPath := filepath.Join(projRoot, cleanRel)
		cleanProj := filepath.Clean(projRoot)
		relCheck, err := filepath.Rel(cleanProj, destPath)
		if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
			writeJSONError(w, http.StatusBadRequest, "path escapes project root")
			return
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(destPath, []byte(cleanCode), 0o644); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if s.bus != nil {
			s.bus.Publish("fs:change", map[string]string{
				"path":   filepath.ToSlash(cleanRel),
				"op":     "write",
				"source": "swarm",
			})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"target":  "project",
			"name":    filepath.Base(destPath),
			"path":    filepath.ToSlash(cleanRel),
			"bytes":   len(cleanCode),
			"message": "File written to project",
		})

	case "file":
		targetPath := strings.TrimSpace(in.Path)
		if targetPath == "" {
			targetPath = name
		}
		targetPath = strings.TrimSpace(targetPath)
		if strings.HasPrefix(targetPath, "/") || strings.HasPrefix(targetPath, "\\") || filepath.IsAbs(targetPath) {
			writeJSONError(w, http.StatusBadRequest, "path must be relative to workspace")
			return
		}
		cleanRel := filepath.Clean(filepath.FromSlash(targetPath))
		if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
			writeJSONError(w, http.StatusBadRequest, "path escapes workspace")
			return
		}
		wsRoot := s.ws.Dir(workspace.WorkspaceDir)
		destPath := filepath.Join(wsRoot, cleanRel)
		cleanWS := filepath.Clean(wsRoot)
		relCheck, err := filepath.Rel(cleanWS, destPath)
		if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
			writeJSONError(w, http.StatusBadRequest, "path escapes workspace")
			return
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(destPath, []byte(cleanCode), 0o644); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		rel := filepath.ToSlash(filepath.Join("workspace", cleanRel))
		if s.bus != nil {
			s.bus.Publish("fs:change", map[string]string{
				"path":   rel,
				"op":     "write",
				"source": "swarm",
			})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"target":  "file",
			"name":    filepath.Base(destPath),
			"path":    rel,
			"bytes":   len(cleanCode),
			"message": "File written to workspace",
		})

	default:
		writeJSONError(w, http.StatusBadRequest, "unknown apply target: "+target)
	}
}

func (s *Server) agentFor(id string) *agent.Agent {
	if id == "" || id == "main" {
		return s.ag
	}
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	if a := s.agents[id]; a != nil {
		return a
	}
	return s.ag
}

func agentSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if (r == '-' || r == '_' || r == ' ') && b.Len() > 0 {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Server) profilesDir() string {
	return filepath.Join(s.ws.Dir(workspace.WorkspaceDir), "agents")
}

func (s *Server) saveAgentProfile(p agent.Profile) error {
	dir := filepath.Join(s.profilesDir(), p.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "profile.json"), raw, 0o644)
}

func (s *Server) loadAgentProfiles() {
	// Workbench-only tests and lightweight embedders may intentionally start a
	// server without a chat agent. Profiles remain on disk until one is present.
	if s.ag == nil {
		return
	}
	entries, _ := os.ReadDir(s.profilesDir())
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.profilesDir(), entry.Name(), "profile.json"))
		if err != nil {
			continue
		}
		var p agent.Profile
		if json.Unmarshal(raw, &p) != nil || p.ID == "" {
			continue
		}
		s.profiles[p.ID] = p
		s.agents[p.ID] = s.ag.CloneProfile(p)
	}
}

func (s *Server) agentCapabilityNames(rel, ext string) []string {
	entries, _ := os.ReadDir(s.ws.Dir(rel))
	out := []string{}
	for _, entry := range entries {
		if entry.IsDir() || (ext != "" && filepath.Ext(entry.Name()) != ext) {
			continue
		}
		out = append(out, strings.TrimSuffix(entry.Name(), ext))
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.ag == nil {
		http.Error(w, `{"error":"agent runtime unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		type view struct {
			ID           string   `json:"id"`
			Number       int      `json:"number"`
			Name         string   `json:"name"`
			Identity     string   `json:"identity"`
			SystemPrompt string   `json:"system_prompt"`
			Tools        []string `json:"tools"`
			Skills       []string `json:"skills"`
			History      int      `json:"history"`
			Mutations    int      `json:"mutations"`
			Active       bool     `json:"active"`
		}
		s.agentMu.RLock()
		active := s.activeAgent
		out := []view{{ID: "main", Number: 1, Name: "Prime", Identity: "Original living organism", History: s.ag.HistoryLen(), Mutations: s.ag.EvolutionCount(), Active: active == "main"}}
		for id, p := range s.profiles {
			a := s.agents[id]
			if a == nil {
				continue
			}
			out = append(out, view{ID: id, Number: p.Number, Name: p.Name, Identity: p.Identity, SystemPrompt: p.SystemPrompt, Tools: p.Tools, Skills: p.Skills, History: a.HistoryLen(), Mutations: a.EvolutionCount(), Active: active == id})
		}
		s.agentMu.RUnlock()
		sort.Slice(out[1:], func(i, j int) bool { return out[i+1].Number < out[j+1].Number })
		json.NewEncoder(w).Encode(map[string]any{"agents": out, "active": active, "tools": s.agentCapabilityNames(workspace.ToolsDir, ".js"), "skills": s.agentCapabilityNames(workspace.SkillsDir, ".md")})
	case http.MethodPost:
		var in struct {
			Action       string   `json:"action"`
			ID           string   `json:"id"`
			Name         string   `json:"name"`
			Identity     string   `json:"identity"`
			SystemPrompt string   `json:"system_prompt"`
			Tools        []string `json:"tools"`
			Skills       []string `json:"skills"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if in.Action == "switch" {
			s.agentMu.Lock()
			defer s.agentMu.Unlock()
			if in.ID != "main" {
				_, ok := s.agents[in.ID]
				if !ok {
					http.Error(w, "unknown agent", 404)
					return
				}
			}
			s.activeAgent = in.ID
			s.bus.Publish("agent:active", map[string]string{"agent": in.ID})
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "active": in.ID})
			return
		}
		id := agentSlug(in.ID)
		if id == "" {
			id = agentSlug(in.Name)
		}
		if id == "" || id == "main" {
			http.Error(w, "agent name required", 400)
			return
		}
		tools, err := validateCapabilities(in.Tools, s.agentCapabilityNames(workspace.ToolsDir, ".js"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		skills, err := validateCapabilities(in.Skills, s.agentCapabilityNames(workspace.SkillsDir, ".md"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.agentMu.Lock()
		defer s.agentMu.Unlock()
		if _, exists := s.profiles[id]; exists {
			http.Error(w, "agent id already exists", http.StatusConflict)
			return
		}
		number := 2
		for _, existing := range s.profiles {
			if existing.Number >= number {
				number = existing.Number + 1
			}
		}
		p := agent.Profile{ID: id, Number: number, Name: strings.TrimSpace(in.Name), Identity: strings.TrimSpace(in.Identity), SystemPrompt: strings.TrimSpace(in.SystemPrompt), Tools: tools, Skills: skills}
		if p.Name == "" {
			p.Name = id
		}
		if err := s.saveAgentProfile(p); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.profiles[id] = p
		s.agents[id] = s.ag.CloneProfile(p)
		s.activeAgent = id
		s.bus.Publish("agent:cloned", map[string]string{"agent": id, "name": p.Name})
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "agent": p, "active": id})
	default:
		http.Error(w, "GET or POST only", 405)
	}
}

func validateCapabilities(selected, available []string) ([]string, error) {
	allowed := make(map[string]bool, len(available))
	for _, name := range available {
		allowed[name] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(selected))
	for _, raw := range selected {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		if !allowed[name] {
			return nil, fmt.Errorf("unknown capability %q", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mgr := &candidate.Manager{WS: s.ws, SourceRoot: workspace.DetectSourceRoot()}
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(map[string]any{"candidates": mgr.List(), "source_root": mgr.SourceRoot})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Action string `json:"action"`
		ID     string `json:"id"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Action != "promote" || req.ID == "" {
		http.Error(w, `{"error":"action promote and id are required"}`, http.StatusBadRequest)
		return
	}
	man, err := mgr.Promote(req.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusConflict)
		return
	}
	s.bus.Publish("candidate:updated", man)
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "candidate": man})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := *s.ws.Config
		// never leak keys to the client, only whether they exist
		masked := map[string]bool{}
		for k, v := range cfg.APIKeys {
			masked[k] = v != ""
		}
		cfg.APIKeys = nil
		json.NewEncoder(w).Encode(map[string]any{"config": cfg, "keys_present": masked, "providers": s.reg.Names()})
	case http.MethodPost:
		var in workspace.Config
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		c := s.ws.Config
		if in.Provider != "" {
			c.Provider = in.Provider
		}
		if in.Model != "" {
			c.Model = in.Model
		}
		if in.UserName != "" {
			c.UserName = in.UserName
		}
		if in.ProjectDir != "" || in.Theme != "" {
			// empty project_dir is only sent by the agent's set_project; the
			// settings drawer always resends the current value
			if in.ProjectDir != "" {
				c.ProjectDir = in.ProjectDir
			}
			if in.Theme != "" {
				c.Theme = in.Theme
			}
		}
		if in.Port > 0 {
			c.Port = in.Port
		}
		if in.HeartbeatMins > 0 {
			c.HeartbeatMins = in.HeartbeatMins
		}
		c.OpenBrowser = in.OpenBrowser
		c.AutoEvolve = in.AutoEvolve
		// keys: replace map wholesale if provided (masked GET means client
		// resends only changed keys prefixed with "set:")
		if in.APIKeys != nil {
			for k, v := range in.APIKeys {
				if v == "" {
					continue
				}
				c.APIKeys[k] = v
			}
		}
		if in.BaseURLs != nil {
			for k, v := range in.BaseURLs {
				if v != "" {
					c.BaseURLs[k] = v
				}
			}
		}
		s.ws.SaveConfig()
		s.applyProvider()
		s.bus.Publish("config:updated", map[string]string{"provider": c.Provider, "model": c.Model})
		w.Write([]byte(`{"ok":true}`))
	default:
		http.Error(w, "method", 405)
	}
}

// handleStop aborts the in-flight generation — the organism mid-thought
// simply stops. It does not roll back mutations it already committed.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.genMu.Lock()
	cancel := s.genCancel
	s.genMu.Unlock()
	if cancel != nil {
		cancel()
		s.bus.Publish("agent:token", map[string]string{"text": "\n\n*(stopped — the organism paused mid-thought)*\n"})
		w.Write([]byte(`{"ok":true}`))
		return
	}
	w.Write([]byte(`{"ok":false,"error":"nothing in flight"}`))
}

// applyProvider rebuilds the active provider from config.
func (s *Server) applyProvider() {
	c := s.ws.Config
	p, model := providers.Build(c.Provider, c.Model, c.APIKeys[c.Provider], c.BaseURLs)
	s.ag.SetProvider(p, model)
}

// handleExport turns a conversation into a standalone HTML transcript the
// user can share. The browser owns the conversation, so it POSTs the messages
// here and gets back one self-contained file (no JS, no external assets).
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var in struct {
		Title    string `json:"title"`
		Messages []struct {
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"messages"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if in.Title == "" {
		in.Title = "EVOLV conversation"
	}
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>`)
	b.WriteString(html.EscapeString(in.Title))
	b.WriteString(`</title><style>
body{margin:0;background:#141311;color:#e8e3d8;font:15px/1.65 -apple-system,'Segoe UI',Roboto,sans-serif}
main{max-width:820px;margin:0 auto;padding:48px 20px 80px}
header{border-bottom:1px solid #2e2b27;padding-bottom:24px;margin-bottom:8px}
.logo{display:flex;align-items:center;gap:14px}
.mark{width:44px;height:44px;border-radius:12px;background:#d97757;color:#1f1e1d;font-weight:800;font-size:26px;display:flex;align-items:center;justify-content:center}
h1{font-size:20px;margin:0;color:#f4f1ea}
.sub{color:#8f887c;font-size:13px;margin-top:4px}
.msg{margin:28px 0}
.who{font-size:11px;letter-spacing:.12em;text-transform:uppercase;color:#8f887c;margin-bottom:6px}
.msg.user .who{color:#d97757}
.bubble{white-space:pre-wrap;word-wrap:break-word}
.msg.user .bubble{background:#211f1c;border:1px solid #2e2b27;border-radius:12px;padding:12px 16px}
footer{margin-top:56px;border-top:1px solid #2e2b27;padding-top:18px;color:#6b665c;font-size:12px}
a{color:#d97757}
</style></head><body><main>
<header><div class="logo"><div class="mark">E</div><div>
<h1>`)
	b.WriteString(html.EscapeString(in.Title))
	b.WriteString(`</h1><div class="sub">transcript exported from EVOLV — the self-evolving AI workspace</div>
</div></div></header>
`)
	for _, m := range in.Messages {
		role := "EVOLV"
		if m.Role == "user" {
			role = "YOU"
		}
		b.WriteString(`<div class="msg `)
		if m.Role == "user" {
			b.WriteString("user")
		} else {
			b.WriteString("evolv")
		}
		b.WriteString(`"><div class="who">`)
		b.WriteString(role)
		b.WriteString("</div><div class=\"bubble\">")
		b.WriteString(html.EscapeString(m.Text))
		b.WriteString("</div></div>\n")
	}
	b.WriteString(`<footer>` + fmt.Sprintf("%d messages", len(in.Messages)) + ` · generated by <a href="https://github.com/dxn111/EVOLV">EVOLV</a> · <span id="ts"></span>
<script>document.getElementById('ts').textContent = new Date().toLocaleString();</script>
</footer></main></body></html>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="evolv-transcript.html"`)
	w.Write([]byte(b.String()))
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	c := s.ws.Config
	p, model := providers.Build(c.Provider, c.Model, c.APIKeys[c.Provider], c.BaseURLs)
	models := []string{}
	if p != nil {
		ctx, cancel := contextWithTimeout(r.Context())
		defer cancel()
		if list, err := p.Models(ctx); err == nil {
			models = list
		}
	}
	if c.Provider == "kilo" || c.Provider == "" {
		models = append(models, providers.KiloFreeModels...)
		models = append(models, providers.KiloPaidModels...)
		models = dedupe(models)
	}
	json.NewEncoder(w).Encode(map[string]any{"provider": p.Name(), "model": model, "models": models})
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// handlePlan serves the agent's current task plan (or null when none).
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(s.ws.ReadPlan())
}

func (s *Server) handleEvolution(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"history":    s.ws.ReadEvolution(500), // client virtualizes: renders 200, pages the rest
		"count":      s.ag.EvolutionCount(),
		"components": s.ag.ListComponents(),
	})
}

// handleReplay serves the ENTIRE mutation ledger — every edit the organism
// ever committed, from birth to now. The replay timeline renders it.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"count":   s.ag.EvolutionCount(),
		"history": s.ws.ReadEvolution(0), // 0 = everything
	})
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	root := s.ws.Dir(workspace.WorkspaceDir)
	switch r.Method {
	case http.MethodPost:
		// the human edits the genome too — writes land live like the agent's
		var req struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
			http.Error(w, `{"error":"path required"}`, 400)
			return
		}
		p := filepath.Join(root, filepath.Clean(req.Path))
		if !strings.HasPrefix(p, root) {
			http.Error(w, `{"error":"escape"}`, 400)
			return
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			http.Error(w, `{"error":"mkdir"}`, 500)
			return
		}
		if err := os.WriteFile(p, []byte(req.Content), 0o644); err != nil {
			http.Error(w, `{"error":"write"}`, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	case http.MethodGet:
		q := r.URL.Query().Get("path")
		p := root
		if q != "" {
			clean := filepath.Clean(filepath.Join(root, q))
			if !strings.HasPrefix(clean, root) {
				http.Error(w, "escape", 400)
				return
			}
			p = clean
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(raw)
	case http.MethodDelete:
		// the human prunes the genome — same rules as write, lands live
		q := r.URL.Query().Get("path")
		if strings.TrimSpace(q) == "" {
			http.Error(w, `{"error":"path required"}`, 400)
			return
		}
		p := filepath.Join(root, filepath.Clean(q))
		if !strings.HasPrefix(p, root) || p == root {
			http.Error(w, `{"error":"escape"}`, 400)
			return
		}
		if err := os.RemoveAll(p); err != nil {
			http.Error(w, `{"error":"delete"}`, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	default:
		http.Error(w, "method", 405)
	}
}

// handleGenome lists every file in the living genome (workspace tree).
func (s *Server) handleGenome(w http.ResponseWriter, r *http.Request) {
	root := s.ws.Dir(workspace.WorkspaceDir)
	var files []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err == nil {
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	json.NewEncoder(w).Encode(map[string]any{"files": files})
}

// handleMind is a snapshot of everything the organism currently knows and
// carries: skills, prompts, grown tools, live plugins, mounted components.
func (s *Server) handleMind(w http.ResponseWriter, r *http.Request) {
	dirNames := func(rel string) []string {
		ents, err := os.ReadDir(s.ws.Dir(rel))
		if err != nil {
			return nil
		}
		var out []string
		seen := map[string]bool{}
		for _, e := range ents {
			if !e.IsDir() {
				name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
				// Typed tools have both name.js and name.json; the dashboard
				// should show the capability once, not once per backing file.
				if !seen[name] {
					out = append(out, name)
					seen[name] = true
				}
			}
		}
		sort.Strings(out)
		return out
	}
	components := s.ag.ListComponents()
	widgets := make([]map[string]any, 0, len(components))
	enabled := make([]string, 0, len(components))
	for _, name := range components {
		on := s.ws.Config.DisabledWidgets == nil || !s.ws.Config.DisabledWidgets[name]
		widgets = append(widgets, map[string]any{"name": name, "enabled": on})
		if on {
			enabled = append(enabled, name)
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"skills":     dirNames(workspace.SkillsDir),
		"prompts":    dirNames(workspace.PromptsDir),
		"tools":      dirNames(workspace.ToolsDir),
		"components": enabled,
		"widgets":    widgets,
		"plugins":    s.ag.PluginNames(),
		"evolutions": s.ag.EvolutionCount(),
		"snapshots":  len(s.ws.ListSnapshots()),
	})
}

// handleWidgets persists whether a grown component is visible. The source
// remains in the genome, so disabling is reversible and never destroys work.
func (s *Server) handleWidgets(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleMind(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "GET or POST", 405)
		return
	}
	var req struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, `{"error":"name required"}`, 400)
		return
	}
	known := false
	for _, name := range s.ag.ListComponents() {
		if name == req.Name {
			known = true
			break
		}
	}
	if !known {
		http.Error(w, `{"error":"unknown widget"}`, 404)
		return
	}
	if s.ws.Config.DisabledWidgets == nil {
		s.ws.Config.DisabledWidgets = map[string]bool{}
	}
	if req.Enabled {
		delete(s.ws.Config.DisabledWidgets, req.Name)
	} else {
		s.ws.Config.DisabledWidgets[req.Name] = true
	}
	s.ws.SaveConfig()
	s.bus.Publish("widget:state", map[string]any{"name": req.Name, "enabled": req.Enabled})
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "name": req.Name, "enabled": req.Enabled})
}

type creationInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Path           string `json:"path"`
	Hash           string `json:"hash"`
	Bytes          int64  `json:"bytes"`
	Updated        string `json:"updated"`
	Enabled        bool   `json:"enabled"`
	Pinned         bool   `json:"pinned"`
	Archived       bool   `json:"archived"`
	Status         string `json:"status"`
	SecurityStatus string `json:"security_status"`
	SecuritySum    string `json:"security_summary"`
}

func (s *Server) creationInventory() []creationInfo {
	type source struct{ kind, rel, ext string }
	sources := []source{
		{"widget", workspace.ComponentsDir, ".html"}, {"tool", workspace.ToolsDir, ".js"},
		{"skill", workspace.SkillsDir, ".md"}, {"prompt", workspace.PromptsDir, ".md"},
	}
	var out []creationInfo
	add := func(kind, rel, name string, info os.FileInfo) {
		full := s.ws.Dir(rel)
		raw, err := os.ReadFile(full)
		status := "healthy"
		if err != nil || len(raw) == 0 {
			status = "broken"
		}
		sum := sha256.Sum256(raw)
		id := kind + ":" + name
		enabled := true
		if kind == "widget" && s.ws.Config.DisabledWidgets != nil && s.ws.Config.DisabledWidgets[name] {
			enabled = false
		}
		secReport := lint.Lint(kind, name, string(raw))
		secStatus := "clean"
		if secReport.ErrorsCount > 0 {
			secStatus = "error"
		} else if secReport.WarnCount > 0 {
			secStatus = "warning"
		}
		out = append(out, creationInfo{
			ID:             id,
			Name:           name,
			Type:           kind,
			Path:           strings.ReplaceAll(rel, "\\", "/"),
			Hash:           fmt.Sprintf("sha256:%x", sum),
			Bytes:          info.Size(),
			Updated:        info.ModTime().UTC().Format(time.RFC3339),
			Enabled:        enabled,
			Pinned:         s.ws.Config.PinnedCreations[id],
			Archived:       s.ws.Config.ArchivedCreations[id],
			Status:         status,
			SecurityStatus: secStatus,
			SecuritySum:    secReport.Summary(),
		})
	}
	for _, src := range sources {
		ents, _ := os.ReadDir(s.ws.Dir(src.rel))
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), src.ext) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			name := strings.TrimSuffix(e.Name(), src.ext)
			add(src.kind, filepath.Join(src.rel, e.Name()), name, info)
		}
	}
	ents, _ := os.ReadDir(s.ws.Dir(workspace.PluginsDir))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		rel := filepath.Join(workspace.PluginsDir, e.Name(), "plugin.js")
		if info, err := os.Stat(s.ws.Dir(rel)); err == nil {
			add("plugin", rel, e.Name(), info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type == out[j].Type {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out
}

func (s *Server) handleCreations(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		items := s.creationInventory()
		json.NewEncoder(w).Encode(map[string]any{"creations": items, "count": len(items), "evolution": s.ws.ReadEvolution(500)})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "GET or POST", 405)
		return
	}
	var req struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Value  bool   `json:"value"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	parts := strings.SplitN(req.ID, ":", 2)
	if len(parts) != 2 {
		http.Error(w, "bad id", 400)
		return
	}
	if s.ws.Config.PinnedCreations == nil {
		s.ws.Config.PinnedCreations = map[string]bool{}
	}
	if s.ws.Config.ArchivedCreations == nil {
		s.ws.Config.ArchivedCreations = map[string]bool{}
	}
	switch req.Action {
	case "pin":
		if req.Value {
			s.ws.Config.PinnedCreations[req.ID] = true
		} else {
			delete(s.ws.Config.PinnedCreations, req.ID)
		}
	case "archive":
		if req.Value {
			s.ws.Config.ArchivedCreations[req.ID] = true
		} else {
			delete(s.ws.Config.ArchivedCreations, req.ID)
		}
	case "enable":
		if parts[0] != "widget" {
			http.Error(w, "only widgets can be toggled", 400)
			return
		}
		if s.ws.Config.DisabledWidgets == nil {
			s.ws.Config.DisabledWidgets = map[string]bool{}
		}
		if req.Value {
			delete(s.ws.Config.DisabledWidgets, parts[1])
		} else {
			s.ws.Config.DisabledWidgets[parts[1]] = true
		}
		s.bus.Publish("widget:state", map[string]any{"name": parts[1], "enabled": req.Value})
	default:
		http.Error(w, "unknown action", 400)
		return
	}
	s.ws.SaveConfig()
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleLint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rep := lint.Lint(req.Type, req.Name, req.Content)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(rep)
}

func (s *Server) handlePackExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	opts := workspace.ExportOptions{
		Author:        strings.TrimSpace(r.URL.Query().Get("author")),
		Description:   strings.TrimSpace(r.URL.Query().Get("desc")),
		IncludeLedger: r.URL.Query().Get("ledger") == "1" || r.URL.Query().Get("ledger") == "true",
	}
	filename := fmt.Sprintf("evolv-workspace-%s.evolvpack", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	if _, err := s.ws.Export(w, opts); err != nil {
		return
	}
}

func (s *Server) handlePackInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	reader, cleanup, err := getPackReader(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer cleanup()

	report, err := s.ws.Import(reader, workspace.ImportOptions{
		DryRun:            true,
		QuarantinePlugins: true,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(report)
}

func (s *Server) handlePackImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	reader, cleanup, err := getPackReader(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer cleanup()

	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true" || r.FormValue("force") == "true" || r.FormValue("force") == "1"
	quarantine := r.URL.Query().Get("quarantine") != "0" && r.URL.Query().Get("quarantine") != "false"

	report, err := s.ws.Import(reader, workspace.ImportOptions{
		DryRun:            false,
		Force:             force,
		QuarantinePlugins: quarantine,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	if s.ag != nil {
		s.ag.ReloadGrownCode()
	}
	for _, p := range append(report.Created, report.Overwritten...) {
		if s.bus != nil {
			s.bus.Publish("fs:change", map[string]string{
				"path":   filepath.ToSlash(filepath.Join("workspace", p)),
				"op":     "write",
				"source": "evolvpack",
			})
		}
	}
	if s.bus != nil {
		s.bus.Publish("evolution", map[string]any{
			"kind":        "pack",
			"name":        "import",
			"description": fmt.Sprintf("Imported %d creations from pack", len(report.Created)+len(report.Overwritten)),
			"source":      "evolvpack",
		})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(report)
}

func getPackReader(r *http.Request) (io.Reader, func(), error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			return nil, nil, err
		}
		file, _, err := r.FormFile("pack")
		if err != nil {
			file, _, err = r.FormFile("file")
		}
		if err != nil {
			return nil, nil, fmt.Errorf("missing pack file in form")
		}
		return file, func() { file.Close() }, nil
	}
	return r.Body, func() {}, nil
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.ag.Reset()
	w.Write([]byte(`{"ok":true}`))
}

// handleSnapshots lists the genome's frozen states (time travel).
func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"snapshots": s.ws.ListSnapshots(),
		"current":   s.ag.LastSnapshot(),
	})
}

// handleSnapshotRestore rewinds the organism to a snapshot id.
func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, `{"error":"id required"}`, 400)
		return
	}
	if err := s.ws.Restore(req.ID); err != nil {
		http.Error(w, `{"error":"`+strings.ReplaceAll(err.Error(), `"`, "")+`"}`, 400)
		return
	}
	s.ag.ReloadGrownCode()
	s.bus.Publish("shell:reload", map[string]string{"reason": "genome rolled back to " + req.ID})
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleSnapshotDiff shows what a mutation actually changed: line-level diff
// between two genome snapshots. ?from=<id>&to=<id> — omit `to` for current.
func (s *Server) handleSnapshotDiff(w http.ResponseWriter, r *http.Request) {
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" || to == "" {
		http.Error(w, `{"error":"from and to required"}`, 400)
		return
	}
	diffs, err := s.ws.DiffSnapshots(from, to)
	if err != nil {
		http.Error(w, `{"error":"`+strings.ReplaceAll(err.Error(), `"`, "")+`"}`, 400)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"diffs": diffs})
}

// handleDream puts the organism to sleep on demand (one autonomous evolution turn).
func (s *Server) handleDream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if s.ag.Dreaming() {
		json.NewEncoder(w).Encode(map[string]bool{"ok": false, "dreaming": true})
		return
	}
	go func() {
		s.ag.DreamNow()
	}()
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	provider, model, agentID, evolutions := "unknown", "", "unavailable", 0
	var stats agent.AgentStats
	tools, plugins, components := 0, 0, 0
	if s.ag != nil {
		provider, model = s.ag.ProviderModel()
		agentID, evolutions = s.ag.ProfileID(), s.ag.EvolutionCount()
		stats = s.ag.Stats()
		tools, plugins, components = s.ag.ToolCount(), s.ag.PluginCount(), s.ag.ComponentCount()
	}
	agents := 0
	s.agentMu.RLock()
	agents = len(s.agents)
	s.agentMu.RUnlock()
	if s.ag != nil {
		agents++
	}
	busSubs, busDropped := 0, uint64(0)
	if s.bus != nil {
		busSubs, busDropped = s.bus.SubscriberCount(), s.bus.Dropped()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	runSnapshot := map[string]any{}
	if s.runStats != nil {
		runSnapshot = s.runStats.snapshot()
	}
	json.NewEncoder(w).Encode(map[string]any{
		"runs":                  runSnapshot,
		"status":                "alive",
		"version":               workspace.Version,
		"uptime_seconds":        int64(time.Since(s.startedAt).Seconds()),
		"started_at":            s.startedAt.UTC().Format(time.RFC3339),
		"provider":              provider,
		"model":                 model,
		"agent":                 agentID,
		"agents":                agents,
		"tools":                 tools,
		"plugins":               plugins,
		"components":            components,
		"evolutions":            evolutions,
		"model_calls":           stats.ModelCalls,
		"model_errors":          stats.ModelErrors,
		"last_model_latency_ms": stats.LastModelLatencyMS,
		"live_connections":      s.liveCount(0),
		"bus_subscribers":       busSubs,
		"bus_dropped":           busDropped,
		"goroutines":            runtime.NumGoroutine(),
		"requests":              s.requests.Load(),
		"errors":                s.errors.Load(),
		"snapshots":             len(s.ws.ListSnapshots()),
	})
}

// handleMetrics exposes a tiny Prometheus-compatible surface. It is deliberately
// dependency-free: the UI does not need it, but operators and smoke tests can
// watch the organism without scraping the whole API.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	evolutions := 0
	var stats agent.AgentStats
	tools, plugins, components := 0, 0, 0
	if s.ag != nil {
		evolutions = s.ag.EvolutionCount()
		stats = s.ag.Stats()
		tools, plugins, components = s.ag.ToolCount(), s.ag.PluginCount(), s.ag.ComponentCount()
	}
	agents := 0
	s.agentMu.RLock()
	agents = len(s.agents)
	s.agentMu.RUnlock()
	if s.ag != nil {
		agents++
	}
	busSubs, busDropped := 0, uint64(0)
	if s.bus != nil {
		busSubs, busDropped = s.bus.SubscriberCount(), s.bus.Dropped()
	}
	if s.ag != nil {
		st := s.ag.Stats()
		s.runStats.turnsStarted.Store(st.Turns)
		s.runStats.turnsFinished.Store(st.Turns)
		s.runStats.turnsFailed.Store(st.TurnsFailed)
		s.runStats.turnsCanceled.Store(st.TurnsCancelled)
		s.runStats.turnsLimited.Store(st.TurnsLimited)
		s.runStats.toolCalls.Store(st.ToolCalls)
		s.runStats.toolFailures.Store(st.ToolFailures)
		s.runStats.lastTurnMS.Store(st.LastTurnMS)
	}
	if s.runStats != nil {
		s.runStats.writePrometheus(w)
	}

	fmt.Fprintf(w, "# HELP evolv_up 1 while the organism is serving.\n")
	fmt.Fprintf(w, "# TYPE evolv_up gauge\nevolv_up 1\n")
	fmt.Fprintf(w, "# HELP evolv_uptime_seconds Seconds since this server started.\n")
	fmt.Fprintf(w, "# TYPE evolv_uptime_seconds gauge\nevolv_uptime_seconds %d\n", int64(time.Since(s.startedAt).Seconds()))
	fmt.Fprintf(w, "# HELP evolv_http_requests_total HTTP requests handled.\n")
	fmt.Fprintf(w, "# TYPE evolv_http_requests_total counter\nevolv_http_requests_total %d\n", s.requests.Load())
	fmt.Fprintf(w, "# HELP evolv_http_errors_total HTTP responses with status >= 500.\n")
	fmt.Fprintf(w, "# TYPE evolv_http_errors_total counter\nevolv_http_errors_total %d\n", s.errors.Load())
	fmt.Fprintf(w, "# HELP evolv_ws_connections Live WebSocket clients.\n")
	fmt.Fprintf(w, "# TYPE evolv_ws_connections gauge\nevolv_ws_connections %d\n", s.liveCount(0))
	fmt.Fprintf(w, "# HELP evolv_agents Configured agent strands.\n")
	fmt.Fprintf(w, "# TYPE evolv_agents gauge\nevolv_agents %d\n", agents)
	fmt.Fprintf(w, "# HELP evolv_evolutions_total Mutations committed by the active agent.\n")
	fmt.Fprintf(w, "# TYPE evolv_evolutions_total counter\nevolv_evolutions_total %d\n", evolutions)
	fmt.Fprintf(w, "# HELP evolv_snapshots Genome snapshots available.\n")
	fmt.Fprintf(w, "# TYPE evolv_snapshots gauge\nevolv_snapshots %d\n", len(s.ws.ListSnapshots()))
	fmt.Fprintf(w, "# HELP evolv_model_calls_total Model stream calls made by the active agent.\n")
	fmt.Fprintf(w, "# TYPE evolv_model_calls_total counter\nevolv_model_calls_total %d\n", stats.ModelCalls)
	fmt.Fprintf(w, "# HELP evolv_model_errors_total Model stream calls that returned an error.\n")
	fmt.Fprintf(w, "# TYPE evolv_model_errors_total counter\nevolv_model_errors_total %d\n", stats.ModelErrors)
	fmt.Fprintf(w, "# HELP evolv_model_last_latency_ms Last model call latency in milliseconds.\n")
	fmt.Fprintf(w, "# TYPE evolv_model_last_latency_ms gauge\nevolv_model_last_latency_ms %d\n", stats.LastModelLatencyMS)
	fmt.Fprintf(w, "# HELP evolv_tools Grown tools available.\n")
	fmt.Fprintf(w, "# TYPE evolv_tools gauge\nevolv_tools %d\n", tools)
	fmt.Fprintf(w, "# HELP evolv_plugins Live plugins.\n")
	fmt.Fprintf(w, "# TYPE evolv_plugins gauge\nevolv_plugins %d\n", plugins)
	fmt.Fprintf(w, "# HELP evolv_components Mounted UI components.\n")
	fmt.Fprintf(w, "# TYPE evolv_components gauge\nevolv_components %d\n", components)
	fmt.Fprintf(w, "# HELP evolv_bus_subscribers Active bus subscribers.\n")
	fmt.Fprintf(w, "# TYPE evolv_bus_subscribers gauge\nevolv_bus_subscribers %d\n", busSubs)
	fmt.Fprintf(w, "# HELP evolv_bus_dropped_total Events dropped for slow subscribers.\n")
	fmt.Fprintf(w, "# TYPE evolv_bus_dropped_total counter\nevolv_bus_dropped_total %d\n", busDropped)
	fmt.Fprintf(w, "# HELP evolv_goroutines Go runtime goroutines.\n")
	fmt.Fprintf(w, "# TYPE evolv_goroutines gauge\nevolv_goroutines %d\n", runtime.NumGoroutine())
	s.latency.writePrometheus(w, "evolv_http_request_duration_seconds")
}

// WatchReleases polls GitHub for newer releases and pushes a live banner to
// every connected browser when one appears. Checks at boot and every 6 hours.
func (s *Server) WatchReleases() {
	go func() {
		check := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			info, err := update.Latest(ctx, workspace.Version)
			if err != nil || info == nil {
				return
			}
			s.mu.Lock()
			first := s.update == nil
			s.update = info
			s.mu.Unlock()
			if first {
				raw, _ := json.Marshal(info)
				s.bus.Publish("update:available", raw)
			}
		}
		check()
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for range t.C {
			check()
		}
	}()
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	u := s.update
	s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"current":   workspace.Version,
		"available": u,
	})
}

// handleUpdateRun performs an in-place self-update and answers with JSON.
func (s *Server) handleUpdateRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	ver, err := update.SelfUpdate(ctx)
	if err != nil {
		http.Error(w, `{"error":"`+strings.ReplaceAll(err.Error(), `"`, "")+`"}`, 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": ver, "note": "restart evolv to run the new genome"})
}

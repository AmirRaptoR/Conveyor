// Package server serves the pipeline view: the board, run history and live
// logs. It observes by default — ticking is what moves items, and that opens
// pull requests on real repositories, so it happens only when asked.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/source"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

//go:embed web
var webFS embed.FS

// State is one poll of every source, cached so the board is instant and the
// list scripts run on the poll interval rather than on every page load.
type State struct {
	Stages  []StageView  `json:"stages"`
	Sources []SourceView `json:"sources"`
	// Items is in the order the scheduler will work them (pipeline.Order),
	// applied on the way out so a drag lands before the next poll does.
	Items    []model.Item `json:"items"`
	Warnings []string     `json:"warnings,omitempty"`
	Order    []string     `json:"order"`
	// Blocks is why each marked item is standing still, keyed by item id. The
	// provider is the authority on *whether* an item is marked; this is the
	// engine's own note on *why*, recovered from the run that marked it.
	Blocks    map[string]Block `json:"blocks,omitempty"`
	UpdatedAt time.Time        `json:"updatedAt"`
	Polling   bool             `json:"polling"`
	// Agents is how each agent the sources call says it is doing — a usage
	// limit, a quota, whatever its own status script chose to report. Empty
	// when no agent provides one.
	Agents []AgentView `json:"agents,omitempty"`
	// Active is every transition running right now. The board lights those
	// stations; without it the page cannot tell work from stillness.
	Active []Active `json:"active"`
}

type Active struct {
	Source string `json:"source"`
	Stage  string `json:"stage"`
	ItemID string `json:"itemId"`
	Title  string `json:"title"`
}

type StageView struct {
	Name   string `json:"name"`
	Script string `json:"script,omitempty"`
	// Next is the stage this one leads to, so the board knows which column a
	// card may be dropped into. It is the only route out; see CONTRACTS §4.
	Next     string `json:"next,omitempty"`
	Runs     bool   `json:"runs"`
	Terminal bool   `json:"terminal"`
}

// Block is why an item stopped, and where to read the rest of it.
type Block struct {
	Reason string    `json:"reason"`
	Stage  string    `json:"stage"`
	RunID  string    `json:"runId,omitempty"`
	At     time.Time `json:"at"`
}

// AgentView is one agent's own report on itself.
//
// The engine understands exactly one field of it: State, three words wide, so
// a lamp can be lit. Everything else is passed through and rendered as given —
// what a usage window is, what a reset means, whether tokens or dollars are the
// interesting number, all of that is the agent's business and differs per
// agent, which is why it comes from a script and not from a struct here.
type AgentView struct {
	Name     string      `json:"name"`
	State    string      `json:"state"` // ok | limited | unknown
	Summary  string      `json:"summary,omitempty"`
	Detail   []AgentFact `json:"detail,omitempty"`
	ResetsAt string      `json:"resetsAt,omitempty"`
	// Error is this probe failing, which is not the agent being unwell: a
	// status script that cannot run tells you nothing about the agent.
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

type AgentFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type SourceView struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider"`
	Workdir  string   `json:"workdir"`
	Problems []string `json:"problems,omitempty"`
}

type Server struct {
	cfg *config.Config
	run *runner.Runner
	eng *pipeline.Engine
	// ctx is the server's own lifetime. Work started from a request outlives
	// the request — a transition takes an hour and the response is immediate —
	// so it must not hang off r.Context(), which is cancelled at the reply.
	ctx context.Context

	mu    sync.RWMutex
	state State
	// blocks is the note beside each mark, kept in memory and recovered from
	// run history after a restart: the runs are the durable record, this is
	// only the index into them that a board read cannot afford to rebuild.
	blocks map[string]Block

	hub    *hub
	order  *store.Order
	active sync.Map      // itemID -> Active, one entry per transition in flight
	tick   chan struct{} // one buffered slot: ticks never queue up
	// wake asks the scheduler to look again. One buffered slot, because the
	// question is always the same one — what can move now — and a queue of it
	// would be a queue of duplicates.
	wake     chan struct{}
	inFlight atomic.Int64
	// polling guards discovery against itself: the ticker and the button both
	// ask for it, and running every list script twice at once buys nothing.
	polling atomic.Bool
}

func New(cfg *config.Config, r *runner.Runner) *Server {
	s := &Server{
		cfg:   cfg,
		run:   r,
		eng:   pipeline.New(cfg, r),
		hub:   newHub(),
		order: store.OpenOrder(filepath.Join(cfg.DataDir(), "order.json")),
		tick:  make(chan struct{}, 1),
		wake:  make(chan struct{}, 1),
		ctx:   context.Background(),
	}
	s.blocks = map[string]Block{}
	s.state = State{Stages: stageViews(cfg), Sources: sourceViews(cfg)}

	// Every log line reaches the browser as it is produced. This is the whole
	// reason logs are a stream and not a file read at the end.
	prev := r.OnLog
	r.OnLog = func(runID string, l runner.LogLine) {
		if prev != nil {
			prev(runID, l)
		}
		s.hub.publish(event{Kind: "log", RunID: runID, Line: &l})
	}
	return s
}

func stageViews(c *config.Config) []StageView {
	out := make([]StageView, len(c.Stages))
	for i, st := range c.Stages {
		out[i] = StageView{Name: st.Name, Script: st.Script, Next: st.OnSuccess, Runs: st.Runs(), Terminal: st.Terminal}
	}
	return out
}

func sourceViews(c *config.Config) []SourceView {
	out := make([]SourceView, len(c.Sources))
	for i, s := range c.Sources {
		out[i] = SourceView{Name: s.Name, Provider: s.Provider.Name, Workdir: c.Workdir(s), Problems: s.Problems}
	}
	return out
}

// Run serves until ctx is done. auto drives the pipeline; without it the server
// only ever reads.
func (s *Server) Run(ctx context.Context, addr string, auto bool) error {
	s.ctx = ctx
	// Three loops, and they are separate on purpose. Discovery must keep its
	// interval while a 90-minute stage runs, so nothing that waits for work to
	// finish may share a goroutine with it.
	go s.poll(ctx)
	go s.button(ctx, auto)
	if auto {
		go s.schedule(ctx)
		if d := s.cfg.RetryStalled.D(); d > 0 {
			go s.stalled(ctx, d)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleRun)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/tick", s.handleTick)
	mux.HandleFunc("PUT /api/order", s.handleOrder)
	mux.HandleFunc("POST /api/items/{id}/start", s.handleStart)
	mux.HandleFunc("POST /api/items/{id}/unblock", s.handleUnblock)
	mux.HandleFunc("POST /api/unblock", s.handleUnblockAll)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	mode := "running: items advance on their own"
	if !auto {
		mode = "watching only: -watch is set, nothing will advance"
	}
	// ":8080" means every interface, so name a host you can actually open;
	// "127.0.0.1:8090" already names one and must not have a second glued on.
	shown := addr
	if strings.HasPrefix(addr, ":") {
		shown = "localhost" + addr
	}
	fmt.Printf("conveyor: http://%s\n  %s\n", shown, mode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// poll re-lists every source on the configured interval, and does nothing
// else.
//
// Discovery is the only thing that finds new work, so it must not be able to
// wait behind it. Draining used to run on this goroutine and returned only when
// the pipeline was at rest: a 90-minute implement was a 90-minute blackout in
// which no source was listed at all, and an issue opened in another repository
// was not deprioritised but simply unseen.
func (s *Server) poll(ctx context.Context) {
	s.refresh(ctx)
	t := time.NewTicker(s.cfg.Poll.D())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refresh(ctx)
		}
	}
}

// stalled watches for the board stopping altogether and, on an interval, hands
// it back. Only when *everything* is marked: while one item can still move, a
// mark is a decision a person has to answer, and clearing it spends an agent
// run to be told the same thing.
//
// A total stall is a different animal. It is almost always the outside world —
// every agent over its usage limit, a credential that expired overnight, a
// worktree one dead run left dirty — and those come back on their own, hours
// after the board gave up. Without this the line stays stopped until somebody
// looks at it, which on a Sunday is the whole weekend.
//
// It reports what it did. An automatic recovery nobody can see is how a board
// starts lying about why work restarted.
func (s *Server) stalled(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.inFlight.Load() != 0 {
			continue // something is running; by definition not stalled
		}
		s.mu.RLock()
		var held []model.Item
		moving := 0
		for _, it := range s.state.Items {
			switch {
			case it.Blocked:
				held = append(held, it)
			default:
				if _, ok := pipeline.Target(s.cfg, &it); ok {
					moving++
				}
			}
		}
		s.mu.RUnlock()
		if moving > 0 || len(held) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "conveyor: every item is marked and nothing can move; clearing %d mark(s) to try again\n", len(held))
		n := s.unblockAll(ctx, held)
		fmt.Fprintf(os.Stderr, "conveyor: handed %d item(s) back to the pipeline\n", n)
		s.wakeUp()
	}
}

// button serves the tick control. In a running pipeline it means "look now", so
// it wakes the scheduler; when watching, it is the only thing that ever moves
// an item, so it performs the transition itself.
//
// Its own goroutine, for the same reason as everything else here: an advance
// takes as long as a stage does, and the poll must not be behind it.
func (s *Server) button(ctx context.Context, auto bool) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.tick:
			if auto {
				s.wakeUp()
				continue
			}
			s.advance(ctx)
			s.refresh(ctx)
		}
	}
}

// wakeUp asks the scheduler for another pass. Never blocks: a full slot already
// carries the same question.
func (s *Server) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// schedule launches everything the locks permit and then sleeps until something
// could have changed — a transition finished, a listing arrived, the button was
// pressed. It never waits for the pipeline to be at rest, which is what let a
// long stage own the loop.
//
// One goroutine, so every launch decision is serialised: a concurrent refresh
// cannot make two passes decide the same transition, and the slot is still
// claimed before anything starts.
func (s *Server) schedule(ctx context.Context) {
	// since counts transitions launched since the pipeline was last at rest.
	// The bound is a circuit breaker, not a budget: items are finite and
	// terminal stages end the walk, so exceeding it means the stage graph
	// loops. That is a configuration error, and spinning on it silently would
	// burn an agent's turns until someone noticed.
	since, stalled := 0, false
	for {
		n := 0
		if !stalled {
			n = s.launch(ctx)
			since += n
		}
		switch {
		case n == 0 && s.inFlight.Load() == 0:
			// Nothing launchable and nothing running: the walk is over, and the
			// next one starts its count from zero.
			since, stalled = 0, false
		case !stalled && since > s.walkLimit():
			stalled = true
			fmt.Fprintln(os.Stderr, "conveyor: launched more transitions than there are items; the stage graph may loop")
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
	}
}

// walkLimit is how many transitions one walk of the board can reasonably take.
func (s *Server) walkLimit() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return 2*len(s.state.Items) + 1
}

func (s *Server) refresh(ctx context.Context) {
	if !s.polling.CompareAndSwap(false, true) {
		return // one is already running; a second would re-run every list script
	}
	defer s.polling.Store(false)
	// A listing is the other thing that can make work launchable, so the
	// scheduler is told about it the moment it lands.
	defer s.wakeUp()

	var items []model.Item
	var warnings []string

	s.mu.Lock()
	s.state.Polling = true
	s.mu.Unlock()
	s.hub.publish(event{Kind: "polling"})

	for _, src := range s.cfg.Sources {
		if !src.OK() {
			continue // its problems are already on the board
		}
		client, ok := s.eng.Client(src.Name)
		if !ok {
			continue
		}
		res, err := client.List(ctx)
		if err != nil {
			warnings = append(warnings, src.Name+": "+err.Error())
			continue
		}
		for _, w := range res.Warnings {
			warnings = append(warnings, src.Name+": "+w.String())
		}
		items = append(items, res.Items...)
	}

	s.askAgents(ctx)
	s.recallBlocks(items)

	s.mu.Lock()
	s.state.Items = items
	s.state.Warnings = warnings
	s.state.Sources = sourceViews(s.cfg)
	s.state.Order = s.order.IDs()
	s.state.UpdatedAt = time.Now()
	s.state.Polling = false
	// A mark cleared on the provider — a label removed by hand — takes its
	// note with it. The provider is the authority on whether, always.
	marked := map[string]bool{}
	for _, it := range items {
		if it.Blocked {
			marked[it.ID] = true
		}
	}
	for id := range s.blocks {
		if !marked[id] {
			delete(s.blocks, id)
		}
	}
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
}

// askAgents runs each agent's status script and collects what it says.
//
// On the discovery tick, beside the list scripts, because it is the same kind
// of question: what does the outside world look like right now. Not on a timer
// of its own — a status probe every minute would file a run a minute, and the
// run history is the log, not a metrics store.
//
// The engine reads three fields and interprets one word of them. It does not
// know what a usage window is, and must not learn: which limits an agent has,
// and what counts against them, is the agent's business.
func (s *Server) askAgents(ctx context.Context) {
	agents := s.cfg.AgentsInUse()
	out := make([]AgentView, 0, len(agents))
	for _, a := range agents {
		if a.Status == "" {
			continue // an agent with nothing to say is not a problem
		}
		v := AgentView{Name: a.Name, State: "unknown", At: time.Now()}
		res, err := s.run.Run(ctx, runner.Spec{
			Script:  a.Status,
			Kind:    "status",
			Source:  a.Name,
			Timeout: 30 * time.Second,
		})
		switch {
		case err != nil:
			v.Error = err.Error()
		case res.Run.Outcome != model.OutcomeSuccess:
			v.Error = fmt.Sprintf("status exited %d (%s)", res.Run.ExitCode, res.Run.Outcome)
		case len(res.Data) == 0:
			v.Error = "status wrote nothing to $CONVEYOR_RESULT"
		default:
			var got AgentView
			if json.Unmarshal(res.Data, &got) != nil {
				v.Error = "status result is not a JSON object"
				break
			}
			got.Name, got.At = a.Name, time.Now()
			if got.State != "ok" && got.State != "limited" {
				got.State = "unknown" // the vocabulary is closed; anything else is silence
			}
			v = got
		}
		out = append(out, v)
	}
	s.mu.Lock()
	s.state.Agents = out
	s.mu.Unlock()
}

// recallBlocks fills in why the marked items are marked, for marks this process
// did not make — everything on the board after a restart, and anything a person
// labelled by hand.
//
// The runs are the durable record: the one that marked an item wrote its reason
// to $CONVEYOR_RESULT, and CONTRACTS §6 pins that run against retention for
// exactly this. One walk fills every gap at once, because walking it per item
// would be one full history scan for each card on a stalled board.
func (s *Server) recallBlocks(items []model.Item) {
	s.mu.RLock()
	want := map[string]bool{}
	for _, it := range items {
		if it.Blocked {
			if _, known := s.blocks[it.ID]; !known {
				want[it.ID] = true
			}
		}
	}
	s.mu.RUnlock()
	if len(want) == 0 {
		return
	}

	found := map[string]Block{}
	s.walkRuns(func(m RunMeta) bool {
		if m.Kind != "stage" || !want[m.ItemID] {
			return true
		}
		switch m.Outcome {
		case model.OutcomeBlocked, model.OutcomeFailure, model.OutcomeTimeout:
		default:
			return true // a success is not why it stopped
		}
		var data json.RawMessage
		if b, err := os.ReadFile(filepath.Join(m.Dir, "result.json")); err == nil {
			data = b
		}
		timeout := time.Duration(0)
		if st, ok := s.cfg.Stage(m.To); ok {
			timeout = st.Timeout.D()
		}
		found[m.ItemID] = Block{
			Reason: pipeline.Reason(m.To, m.Run, data, timeout, 0),
			Stage:  m.To,
			RunID:  m.ID,
			At:     m.FinishedAt,
		}
		delete(want, m.ItemID)
		return len(want) > 0
	})

	s.mu.Lock()
	for id, b := range found {
		if _, known := s.blocks[id]; !known {
			s.blocks[id] = b
		}
	}
	// Marked, and no run to explain it: someone put the label on by hand.
	for id := range want {
		if _, known := s.blocks[id]; !known {
			s.blocks[id] = Block{Reason: "marked outside the pipeline; there is no run to explain it"}
		}
	}
	s.mu.Unlock()
}

// launch starts every transition the locks currently permit and returns how
// many it started. Candidates whose source or target stage is already busy are
// skipped rather than queued, so a slow stage never holds up a free one.
func (s *Server) launch(ctx context.Context) int {
	s.mu.RLock()
	items := append([]model.Item(nil), s.state.Items...)
	s.mu.RUnlock()
	order := s.order.IDs()

	n := 0
	// Claimed within this pass: Busy only reflects locks already taken, and a
	// goroutine launched a moment ago may not have taken its own yet.
	claimedSrc := map[string]bool{}
	claimedStage := map[string]bool{}

	for {
		free := items[:0:0]
		for _, it := range items {
			target, ok := pipeline.Target(s.cfg, &it)
			if !ok || claimedSrc[it.Source] || claimedStage[target] ||
				s.eng.Locks().Busy(it.Source, target) {
				continue
			}
			free = append(free, it)
		}
		item, target := pipeline.Pick(s.cfg, free, order)
		if item == nil {
			return n
		}
		// Claim before launching. Checking Busy and starting a goroutine leaves
		// a gap in which the next pass sees the slot free and decides the same
		// thing again; the duplicate then bails, having spent a launch.
		if !s.eng.Locks().TryAcquire(item.Source, target) {
			claimedSrc[item.Source] = true
			claimedStage[target] = true
			continue
		}
		claimedSrc[item.Source] = true
		claimedStage[target] = true

		it, to := *item, target
		s.inFlight.Add(1)
		n++
		go s.transition(ctx, it, to)
	}
}

// transition runs one launched transition and gives its slot back.
//
// The order of the three finishing steps is the whole of it: the lock goes
// first, the count second, and only then is the scheduler woken. Waking it
// while the slot is still held would have it find the source busy and go back
// to sleep, and the release that followed would wake nothing.
func (s *Server) transition(ctx context.Context, item model.Item, target string) {
	defer s.wakeUp()
	defer s.inFlight.Add(-1)
	defer s.eng.Locks().Release(item.Source, target)
	s.runOne(ctx, item, target)
}

// runOne performs one transition and keeps the board honest about it.
func (s *Server) runOne(ctx context.Context, item model.Item, target string) {
	s.setActive(item.ID, &Active{Source: item.Source, Stage: target, ItemID: item.ID, Title: item.Title})
	defer s.setActive(item.ID, nil)

	tr, _ := s.eng.Advance(ctx, item.Source, &item, target)
	if tr != nil {
		s.applyTransition(tr)
		s.hub.publish(event{Kind: "transition", Transition: tr})
	}
}

// applyTransition writes an item's new stage into the cache the moment it is
// known, rather than waiting for the next poll.
//
// Without it the drain re-listed while transitions were in flight, saw an item
// still sitting in a stage that runs a script, and treated finished work as an
// interrupted job — running an agent over it a second time. The provider is
// still the authority; this only stops the cache lying in the gap.
func (s *Server) applyTransition(tr *pipeline.Transition) {
	s.mu.Lock()
	for i := range s.state.Items {
		if s.state.Items[i].ID == tr.Item.ID {
			s.state.Items[i] = tr.Item
			break
		}
	}
	// Written here rather than recovered later: this is the one moment the
	// reason exists in full, and a run history sweep must not be what stands
	// between an operator and why their board stopped.
	if tr.Blocked {
		s.blocks[tr.Item.ID] = Block{Reason: tr.Reason, Stage: tr.Stage, RunID: tr.RunID, At: time.Now()}
	} else {
		delete(s.blocks, tr.Item.ID)
	}
	s.mu.Unlock()
}

// advance runs a single transition: the button, which works whether or not the
// pipeline is running itself.
func (s *Server) advance(ctx context.Context) bool {
	s.mu.RLock()
	items := append([]model.Item(nil), s.state.Items...)
	s.mu.RUnlock()

	item, target := pipeline.Pick(s.cfg, items, s.order.IDs())
	if item == nil {
		return false
	}
	if !s.eng.Locks().TryAcquire(item.Source, target) {
		return false
	}
	defer s.eng.Locks().Release(item.Source, target)
	s.runOne(ctx, *item, target)
	return true
}

// setActive records a transition as in flight, or clears it when a is nil.
func (s *Server) setActive(itemID string, a *Active) {
	if a == nil {
		s.active.Delete(itemID)
	} else {
		s.active.Store(itemID, *a)
	}
	s.mu.Lock()
	s.state.Active = s.activeList()
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
}

func (s *Server) activeList() []Active {
	out := []Active{}
	s.active.Range(func(_, v any) bool {
		out = append(out, v.(Active))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ItemID < out[j].ItemID })
	return out
}

// handleState hands out the board. Items go out in the order the scheduler
// will work them, so the page can draw them in the order they arrive instead of
// keeping a second copy of the ordering rules that is free to disagree.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	st := s.state
	st.Items = pipeline.Order(s.cfg, s.state.Items, s.state.Order)
	st.Blocks = make(map[string]Block, len(s.blocks))
	for id, b := range s.blocks {
		st.Blocks[id] = b
	}
	s.mu.RUnlock()
	writeJSON(w, st)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	go s.refresh(s.ctx)
	w.WriteHeader(http.StatusAccepted)
}

// handleTick advances one item. Non-blocking: a second press while one is
// running is dropped rather than queued, because two agents in one worktree is
// exactly what perSource exists to prevent.
func (s *Server) handleTick(w http.ResponseWriter, r *http.Request) {
	select {
	case s.tick <- struct{}{}:
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "a tick is already in flight", http.StatusConflict)
	}
}

// handleOrder replaces the manual input order. The whole list is sent, not a
// move: two browsers reordering at once should end with one of the two
// arrangements, not a merge of both.
func (s *Server) handleOrder(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		http.Error(w, "expected a JSON array of item ids", http.StatusBadRequest)
		return
	}
	if err := s.order.Set(ids); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.state.Order = s.order.IDs()
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
	w.WriteHeader(http.StatusNoContent)
}

// handleStart runs one item's next transition now, rather than when the poller
// next comes round. It is what a drag out of the backlog does.
//
// It can only start the transition the scheduler would have started anyway. A
// drop is a decision about *when*, never about which stage: a card dropped
// straight onto a deploy stage would be a deploy nobody reviewed, and the
// pipeline being human-authored is the point.
//
// A busy slot is a refusal, not a queue, and the refusal says what is holding
// it. Dropping something and seeing nothing happen reads as a broken board;
// "midgame is busy with midgame:49" reads as a reason to wait. The persisted
// input order is still what decides who goes next when the slot frees.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Stage string `json:"stage"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `expected {"stage": "..."}`, http.StatusBadRequest)
			return
		}
	}

	var item model.Item
	found := false
	s.mu.RLock()
	for _, it := range s.state.Items {
		if it.ID == id {
			item, found = it, true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		http.Error(w, "no item "+id+" on the board", http.StatusNotFound)
		return
	}

	target, ok := pipeline.Target(s.cfg, &item)
	if !ok {
		http.Error(w, s.whyStuck(item), http.StatusConflict)
		return
	}
	if body.Stage != "" && body.Stage != target {
		http.Error(w, fmt.Sprintf("%s goes to %s next, not %s — a drop starts the next stage, it cannot skip one",
			id, target, body.Stage), http.StatusConflict)
		return
	}

	// Claim before launching, exactly as the scheduler does: a check followed
	// by a goroutine leaves a gap the next pass can decide the same thing in.
	if !s.eng.Locks().TryAcquire(item.Source, target) {
		http.Error(w, s.whyBusy(item.Source, target), http.StatusConflict)
		return
	}
	s.inFlight.Add(1)
	go s.transition(s.ctx, item, target)
	w.WriteHeader(http.StatusAccepted)
}

// handleUnblock clears one item's mark. It is the other half of the board: the
// engine can mark an item, and only a person can take it back.
//
// The mark comes off where the item stands, never by moving it — that is the
// whole point of a mark over a column. The scheduler is woken because the item
// becomes pickable the instant the label is gone.
func (s *Server) handleUnblock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.RLock()
	var item model.Item
	found := false
	for _, it := range s.state.Items {
		if it.ID == id {
			item, found = it, true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		http.Error(w, "no item "+id+" on the board", http.StatusNotFound)
		return
	}
	if !item.Blocked {
		w.WriteHeader(http.StatusNoContent) // already where the caller wants it
		return
	}
	if err := s.unblock(s.ctx, item); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUnblockAll hands the whole board back at once, which is what an
// operator wants after fixing the thing that stopped it — an expired credential,
// a dirty worktree, an agent that was over its limit all night.
//
// In the background: each item is a provider write, and thirty of them is not a
// request. The board is republished as they land.
func (s *Server) handleUnblockAll(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var held []model.Item
	for _, it := range s.state.Items {
		if it.Blocked {
			held = append(held, it)
		}
	}
	s.mu.RUnlock()
	go s.unblockAll(s.ctx, held)
	writeJSON(w, map[string]int{"unblocking": len(held)})
}

// unblockAll clears marks one at a time. Sequentially on purpose: perSource is
// 1 because a source is a worktree, and while these writes touch no worktree,
// thirty concurrent `gh` calls against one repository is its own outage.
func (s *Server) unblockAll(ctx context.Context, held []model.Item) int {
	n := 0
	for _, it := range held {
		if err := s.unblock(ctx, it); err != nil {
			fmt.Fprintf(os.Stderr, "conveyor: could not unblock %s: %v\n", it.ID, err)
			continue
		}
		n++
	}
	return n
}

// unblock is one provider write: the same move that set the mark, with the mark
// off. The engine's note goes with it — the run that explains it is still
// filed, but the item is no longer asking anything of anyone.
func (s *Server) unblock(ctx context.Context, item model.Item) error {
	client, ok := s.eng.Client(item.Source)
	if !ok {
		return fmt.Errorf("%s: no provider client", item.Source)
	}
	if _, err := client.Move(ctx, &item, item.Stage, source.Mark{}); err != nil {
		return err
	}
	s.mu.Lock()
	for i := range s.state.Items {
		if s.state.Items[i].ID == item.ID {
			s.state.Items[i] = item
			break
		}
	}
	delete(s.blocks, item.ID)
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
	s.wakeUp()
	return nil
}

// whyStuck says why an item has nowhere to go, in the operator's terms.
func (s *Server) whyStuck(it model.Item) string {
	if it.Blocked {
		return it.ID + " is waiting for a person — clear its mark and the pipeline takes it back"
	}
	st, ok := s.cfg.Stage(it.Stage)
	switch {
	case !ok:
		return fmt.Sprintf("%s is in %s, which is not a stage in this pipeline", it.ID, it.Stage)
	case st.Terminal:
		return fmt.Sprintf("%s is in %s, the end of the line", it.ID, it.Stage)
	}
	return fmt.Sprintf("%s has nowhere to go from %s", it.ID, it.Stage)
}

// whyBusy names what is holding the slot. perSource comes first because it is
// the constraint an operator hits: one worktree, one agent.
func (s *Server) whyBusy(src, stage string) string {
	active := s.activeList()
	for _, a := range active {
		if a.Source == src {
			return fmt.Sprintf("%s is busy with %s in %s", src, a.ItemID, a.Stage)
		}
	}
	for _, a := range active {
		if a.Stage == stage {
			return fmt.Sprintf("%s is busy with %s", stage, a.ItemID)
		}
	}
	return fmt.Sprintf("%s or %s is already busy", src, stage)
}

// RunMeta is one run directory, as the board needs it.
type RunMeta struct {
	model.Run
	// Lines, not a blob: a log carries structure the writer already knew —
	// which stream a line came from — and handing back one string throws it
	// away, so a finished run could not be rendered the way a live one is.
	Lines []runner.LogLine `json:"lines,omitempty"`
}

// handleRuns lists recent runs, newest first, optionally for one item.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	item := r.URL.Query().Get("item")
	runs, err := s.listRuns(item, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, runs)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	runs, err := s.listRuns("", 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, run := range runs {
		if run.ID == id {
			b, _ := os.ReadFile(filepath.Join(run.Dir, "log.txt"))
			run.Lines = parseLog(string(b))
			writeJSON(w, run)
			return
		}
	}
	http.NotFound(w, r)
}

// listRuns walks the run root newest-day-first and stops once it has enough.
func (s *Server) listRuns(itemID string, limit int) ([]RunMeta, error) {
	out := []RunMeta{}
	s.walkRuns(func(m RunMeta) bool {
		if itemID != "" && m.ItemID != itemID {
			return true
		}
		out = append(out, m)
		return len(out) < limit
	})
	return out, nil
}

// walkRuns visits every run newest first, and stops when the visitor says so.
// One walk, one definition of "newest": the directory names are the clock, day
// then time-ordered id, so sorting them descending is the whole ordering.
func (s *Server) walkRuns(visit func(RunMeta) bool) {
	root := s.run.Root
	days, err := os.ReadDir(root)
	if err != nil {
		return
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Name() > days[j].Name() })

	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, day.Name()))
		if err != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, day.Name(), e.Name())
			b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
			if err != nil || len(b) == 0 {
				continue
			}
			var m RunMeta
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			m.Dir = dir
			if !visit(m) {
				return
			}
		}
	}
}

// parseLog turns a written log back into the lines it was made of. The format
// is the runner's: a timestamp, the stream, then the text, which may itself
// contain anything at all — so it is split exactly twice and no further.
func parseLog(s string) []runner.LogLine {
	out := []runner.LogLine{}
	for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if ln == "" {
			out = append(out, runner.LogLine{Stream: "stdout"})
			continue
		}
		parts := strings.SplitN(ln, " ", 3)
		if len(parts) < 3 {
			out = append(out, runner.LogLine{Stream: "stdout", Text: ln})
			continue
		}
		// Verbatim: the writer pads the stream to a fixed width, and every
		// stream name is already that width, so anything after the second
		// space is the script's own text — indentation included.
		out = append(out, runner.LogLine{Stream: strings.TrimSpace(parts[1]), Text: parts[2]})
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- SSE -------------------------------------------------------------------

type event struct {
	Kind       string               `json:"kind"` // log | state | polling | transition
	RunID      string               `json:"runId,omitempty"`
	Line       *runner.LogLine      `json:"line,omitempty"`
	Transition *pipeline.Transition `json:"transition,omitempty"`
}

type hub struct {
	mu   sync.Mutex
	subs map[chan event]struct{}
}

func newHub() *hub { return &hub{subs: map[chan event]struct{}{}} }

func (h *hub) subscribe() chan event {
	ch := make(chan event, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}

// publish never blocks: a browser that has stopped reading must not be able to
// stall the pipeline that is producing the logs.
func (h *hub) publish(e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e := <-ch:
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// Addr normalises a listen address so `-addr 8080` works like `-addr :8080`.
func Addr(a string) string {
	if a == "" {
		return ":8080"
	}
	if !strings.Contains(a, ":") {
		return ":" + a
	}
	return a
}

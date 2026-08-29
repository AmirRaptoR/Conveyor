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
	Items     []model.Item `json:"items"`
	Warnings  []string     `json:"warnings,omitempty"`
	Order     []string     `json:"order"`
	UpdatedAt time.Time    `json:"updatedAt"`
	Polling   bool         `json:"polling"`
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
	Name     string `json:"name"`
	Script   string `json:"script,omitempty"`
	Runs     bool   `json:"runs"`
	Terminal bool   `json:"terminal"`
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
		out[i] = StageView{Name: st.Name, Script: st.Script, Runs: st.Runs(), Terminal: st.Terminal}
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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleRun)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/tick", s.handleTick)
	mux.HandleFunc("PUT /api/order", s.handleOrder)

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

	s.mu.Lock()
	s.state.Items = items
	s.state.Warnings = warnings
	s.state.Sources = sourceViews(s.cfg)
	s.state.Order = s.order.IDs()
	s.state.UpdatedAt = time.Now()
	s.state.Polling = false
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
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
	root := s.run.Root
	days, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []RunMeta{}, nil
		}
		return nil, err
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Name() > days[j].Name() })

	out := []RunMeta{}
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
			if itemID != "" && m.ItemID != itemID {
				continue
			}
			m.Dir = dir
			out = append(out, m)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
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

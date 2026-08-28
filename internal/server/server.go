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
	Stages    []StageView  `json:"stages"`
	Sources   []SourceView `json:"sources"`
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

	mu    sync.RWMutex
	state State

	hub    *hub
	order  *store.Order
	active sync.Map      // itemID -> Active, one entry per transition in flight
	tick   chan struct{} // one buffered slot: ticks never queue up
}

func New(cfg *config.Config, r *runner.Runner) *Server {
	s := &Server{
		cfg:   cfg,
		run:   r,
		eng:   pipeline.New(cfg, r),
		hub:   newHub(),
		order: store.OpenOrder(filepath.Join(cfg.DataDir(), "order.json")),
		tick:  make(chan struct{}, 1),
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
	go s.poll(ctx, auto)

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
	fmt.Printf("conveyor: http://localhost%s\n  %s\n", addr, mode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// poll re-lists every source on the configured interval and, when running
// automatically, drains what it finds.
func (s *Server) poll(ctx context.Context, auto bool) {
	s.refresh(ctx)
	if auto {
		s.drain(ctx)
	}
	t := time.NewTicker(s.cfg.Poll.D())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.tick: // the button, which works whether or not auto is on
			s.advance(ctx)
			s.refresh(ctx)
		case <-t.C:
			s.refresh(ctx)
			if auto {
				s.drain(ctx)
			}
		}
	}
}

func (s *Server) refresh(ctx context.Context) {
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

// drain advances until nothing can move, running as much at once as the locks
// allow. One item per stage by default, one per source always, capped globally.
//
// The bound is a circuit breaker, not a budget: items are finite and terminal
// stages end the walk, so reaching it means the stage graph loops. That is a
// configuration error, and spinning on it silently would burn an agent's turns
// until someone noticed.
func (s *Server) drain(ctx context.Context) {
	s.mu.RLock()
	limit := 2*len(s.state.Items) + 1
	s.mu.RUnlock()

	var wg sync.WaitGroup
	var inFlight atomic.Int64
	finished := make(chan struct{}, 64)

	for started := 0; started < limit; {
		n := s.launch(ctx, &wg, &inFlight, finished)
		started += n
		if n > 0 {
			continue // a slot may still be free; fill it before waiting
		}
		// Nothing launchable. If nothing is running either, the pipeline is at
		// rest — checked after the launch attempt, so a slot freed mid-pass is
		// retried rather than mistaken for the end of the work.
		if inFlight.Load() == 0 {
			wg.Wait()
			s.hub.publish(event{Kind: "state"})
			return
		}
		select {
		case <-finished:
		case <-ctx.Done():
			wg.Wait()
			return
		}
	}
	wg.Wait()
	fmt.Fprintln(os.Stderr, "conveyor: drain hit its limit; the stage graph may loop")
	s.hub.publish(event{Kind: "state"})
}

// launch starts every transition the locks currently permit and returns how
// many it started. Candidates whose source or target stage is already busy are
// skipped rather than queued, so a slow stage never holds up a free one.
func (s *Server) launch(ctx context.Context, wg *sync.WaitGroup, inFlight *atomic.Int64, finished chan struct{}) int {
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
		wg.Add(1)
		inFlight.Add(1)
		n++
		go func() {
			defer wg.Done()
			defer inFlight.Add(-1)
			defer s.eng.Locks().Release(it.Source, to)
			s.runOne(ctx, it, to)
			select {
			case finished <- struct{}{}:
			default:
			}
		}()
	}
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

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	st := s.state
	s.mu.RUnlock()
	writeJSON(w, st)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	go s.refresh(context.Background())
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

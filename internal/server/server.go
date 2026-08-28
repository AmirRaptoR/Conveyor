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
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
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
	UpdatedAt time.Time    `json:"updatedAt"`
	Polling   bool         `json:"polling"`
	// Active is the stage running right now, if any. The board lights that
	// station; without it the page cannot tell work from stillness.
	Active *Active `json:"active,omitempty"`
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

	hub  *hub
	tick chan struct{} // one buffered slot: ticks never queue up
}

func New(cfg *config.Config, r *runner.Runner) *Server {
	s := &Server{
		cfg:  cfg,
		run:  r,
		eng:  pipeline.New(cfg, r),
		hub:  newHub(),
		tick: make(chan struct{}, 1),
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

// Run serves until ctx is done. autoTick drives the pipeline on the poll
// interval; without it the server only ever reads.
func (s *Server) Run(ctx context.Context, addr string, autoTick bool) error {
	go s.poll(ctx, autoTick)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleRun)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/tick", s.handleTick)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	fmt.Printf("conveyor: http://localhost%s  (auto-tick %v)\n", addr, autoTick)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// poll re-lists every source on the configured interval, and optionally
// advances one item per pass.
func (s *Server) poll(ctx context.Context, autoTick bool) {
	s.refresh(ctx)
	t := time.NewTicker(s.cfg.Poll.D())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.tick:
			s.advance(ctx)
			s.refresh(ctx)
		case <-t.C:
			if autoTick {
				s.advance(ctx)
			}
			s.refresh(ctx)
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
	s.state.UpdatedAt = time.Now()
	s.state.Polling = false
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
}

// advance runs one scheduling pass: the highest-priority item that can move.
func (s *Server) advance(ctx context.Context) {
	s.mu.RLock()
	items := append([]model.Item(nil), s.state.Items...)
	s.mu.RUnlock()

	item, target := pipeline.Pick(s.cfg, items, nil)
	if item == nil {
		return
	}

	s.setActive(&Active{Source: item.Source, Stage: target, ItemID: item.ID, Title: item.Title})
	defer s.setActive(nil)

	tr, _ := s.eng.Advance(ctx, item.Source, item, target)
	if tr != nil {
		s.hub.publish(event{Kind: "transition", Transition: tr})
	}
}

func (s *Server) setActive(a *Active) {
	s.mu.Lock()
	s.state.Active = a
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
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

// RunMeta is one run directory, as the board needs it.
type RunMeta struct {
	model.Run
	Log string `json:"log,omitempty"`
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
			run.Log = string(b)
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

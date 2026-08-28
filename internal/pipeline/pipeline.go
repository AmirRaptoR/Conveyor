// Package pipeline decides which item moves where, and enforces the ordering
// rules from docs/CONTRACTS.md §4 and §5.
//
// v1 is a pipeline, not a board: items advance automatically and the only human
// control surface is the order of the inputs.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/source"
)

// Locks enforces concurrency. perSource is 1 and not configurable above it: a
// source maps to a git worktree, and two agents in one checkout corrupt each
// other. global bounds how many sources run at once.
type Locks struct {
	mu     sync.Mutex
	inUse  map[string]bool
	global chan struct{}
}

func NewLocks(global int) *Locks {
	if global < 1 {
		global = 1
	}
	return &Locks{inUse: map[string]bool{}, global: make(chan struct{}, global)}
}

// TryAcquire takes the source's slot and one global slot, or reports false
// without blocking. Never blocks, so a busy source is skipped rather than
// queueing work behind it.
func (l *Locks) TryAcquire(src string) bool {
	l.mu.Lock()
	if l.inUse[src] {
		l.mu.Unlock()
		return false
	}
	l.inUse[src] = true
	l.mu.Unlock()

	select {
	case l.global <- struct{}{}:
		return true
	default:
		l.mu.Lock()
		delete(l.inUse, src)
		l.mu.Unlock()
		return false
	}
}

func (l *Locks) Release(src string) {
	l.mu.Lock()
	delete(l.inUse, src)
	l.mu.Unlock()
	select {
	case <-l.global:
	default:
	}
}

// Attempts counts consecutive failures per item so maxAttempts can force a
// blocked item out of a retry loop.
type Attempts struct {
	mu sync.Mutex
	n  map[string]int
}

func NewAttempts() *Attempts { return &Attempts{n: map[string]int{}} }

func (a *Attempts) Bump(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n[id]++
	return a.n[id]
}

func (a *Attempts) Clear(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.n, id)
}

func (a *Attempts) Get(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n[id]
}

// Transition is the record of one item moving through one stage.
type Transition struct {
	Item     model.Item    `json:"item"`
	Stage    string        `json:"stage"`
	From     string        `json:"from"`
	Next     string        `json:"next,omitempty"`
	Outcome  model.Outcome `json:"outcome"`
	RunID    string        `json:"runId,omitempty"`
	RunDir   string        `json:"runDir,omitempty"`
	Attempts int           `json:"attempts,omitempty"`
	Err      error         `json:"-"`
}

// Engine advances items. It owns no state beyond locks and attempt counts:
// provider state is the source of truth, re-read on every poll.
type Engine struct {
	cfg      *config.Config
	runner   *runner.Runner
	locks    *Locks
	attempts *Attempts
	clients  map[string]*source.Client
}

func New(cfg *config.Config, r *runner.Runner) *Engine {
	e := &Engine{
		cfg: cfg, runner: r,
		locks:    NewLocks(cfg.Concurrency.Global),
		attempts: NewAttempts(),
		clients:  map[string]*source.Client{},
	}
	for _, s := range cfg.Sources {
		e.clients[s.Name] = source.New(cfg, s, r)
	}
	return e
}

func (e *Engine) Client(name string) (*source.Client, bool) {
	c, ok := e.clients[name]
	return c, ok
}

// ErrBusy is returned when the source already has an item in flight.
var ErrBusy = errors.New("source is busy")

// Advance moves one item into a stage and runs that stage's script.
//
// The order is fixed by CONTRACTS.md §4 and the move comes first deliberately:
// if the process dies mid-stage the provider already reflects reality, so the
// item is not handed out twice on the next poll.
func (e *Engine) Advance(ctx context.Context, srcName string, item *model.Item, to string) (*Transition, error) {
	client, ok := e.clients[srcName]
	if !ok {
		return nil, fmt.Errorf("no source named %q", srcName)
	}
	stage, ok := e.cfg.Stage(to)
	if !ok {
		return nil, fmt.Errorf("no stage named %q", to)
	}
	if !e.locks.TryAcquire(srcName) {
		return nil, ErrBusy
	}
	defer e.locks.Release(srcName)

	from := item.Stage
	tr := &Transition{Item: *item, Stage: to, From: from}

	if _, err := client.Move(ctx, item, to); err != nil {
		tr.Err = err
		return tr, err
	}
	tr.Item = *item

	// A stage with no script is a queue, not an error: the item rests here
	// until something else moves it.
	if !stage.Work {
		tr.Outcome = model.OutcomeNoop
		return tr, nil
	}

	src := mustSource(e.cfg, srcName)
	script, ok := src.Scripts[to]
	if !ok {
		// resolveSources already recorded why, and Engine skips unhealthy
		// sources — reaching here means one went bad after load.
		tr.Outcome = model.OutcomeFailure
		tr.Err = fmt.Errorf("source %q has no script for stage %q", srcName, to)
		return tr, tr.Err
	}

	res, err := e.runner.Run(ctx, runner.Spec{
		Script:  script,
		Kind:    "stage",
		Workdir: e.cfg.Workdir(mustSource(e.cfg, srcName)),
		Env:     mergeEnv(src.Env, stage.Env),
		Source:  srcName,
		Item:    item,
		From:    from,
		To:      to,
		Timeout: stage.Timeout.D(),
		Stdin:   model.StageInput{Item: item, Stage: to, From: from},
	})
	if err != nil {
		tr.Err = err
		tr.Outcome = model.OutcomeFailure
		return tr, err
	}
	tr.Outcome = res.Run.Outcome
	tr.RunID = res.Run.ID
	tr.RunDir = res.Run.Dir

	next := e.route(stage, res.Run.Outcome, item.ID, tr)
	tr.Next = next
	if next == "" || next == to {
		return tr, nil
	}
	if _, err := client.Move(ctx, item, next); err != nil {
		tr.Err = err
		return tr, err
	}
	tr.Item = *item
	return tr, nil
}

// route applies the transition table, including the attempt ceiling. A stage
// that keeps failing must eventually stop being retried, or the scheduler will
// hand it the same item on every poll forever.
func (e *Engine) route(s *config.Stage, o model.Outcome, itemID string, tr *Transition) string {
	switch o {
	case model.OutcomeSuccess:
		e.attempts.Clear(itemID)
		return s.OnSuccess
	case model.OutcomeNoop:
		e.attempts.Clear(itemID)
		return ""
	case model.OutcomeBlocked:
		e.attempts.Clear(itemID)
		return s.OnBlocked
	default: // failure, timeout
		n := e.attempts.Bump(itemID)
		tr.Attempts = n
		if s.MaxAttempts > 0 && n >= s.MaxAttempts {
			e.attempts.Clear(itemID)
			if s.OnBlocked != "" {
				return s.OnBlocked
			}
		}
		return s.OnFailure
	}
}

// Target returns the stage an item should be moved into next, if any.
//
// Two cases, and the second is recovery. An item resting in a queue stage (no
// onEnter) advances to that stage's success target. An item found *in* a stage
// that has a script means a previous run did not finish — the engine writes
// provider state before running, so this is what a crash looks like — and the
// right answer is to run that stage again rather than skip past it.
func Target(cfg *config.Config, it *model.Item) (string, bool) {
	stage, ok := cfg.Stage(it.Stage)
	if !ok || stage.Terminal {
		return "", false
	}
	if stage.Work {
		return stage.Name, true // re-run: recover an interrupted stage
	}
	if stage.OnSuccess == "" {
		return "", false // a queue with nowhere to go: items rest here
	}
	return stage.OnSuccess, true
}

// Pick chooses the next item to work from a listing.
//
// v1's only human control is the order of the inputs, so ordering is honoured
// first: an item's position in `order` beats its priority. Items not named in
// `order` fall back to priority, then to the source's own listing order, which
// keeps the choice stable across polls.
func Pick(cfg *config.Config, items []model.Item, order []string) (*model.Item, string) {
	pos := make(map[string]int, len(order))
	for i, id := range order {
		pos[id] = i
	}
	best := -1
	var bestTarget string
	var bOrdered bool
	var bIdx, bPrio, bPos int

	for i := range items {
		it := &items[i]
		target, ok := Target(cfg, it)
		if !ok {
			continue
		}
		oi, ordered := pos[it.ID]
		prio := 1 << 30
		if it.Priority != nil {
			prio = *it.Priority
		}
		if best == -1 || better(ordered, oi, prio, i, bOrdered, bIdx, bPrio, bPos) {
			best, bestTarget = i, target
			bOrdered, bIdx, bPrio, bPos = ordered, oi, prio, i
		}
	}
	if best == -1 {
		return nil, ""
	}
	return &items[best], bestTarget
}

func better(aOrdered bool, aIdx, aPrio, aPos int, bOrdered bool, bIdx, bPrio, bPos int) bool {
	if aOrdered != bOrdered {
		return aOrdered // an explicitly ordered item always wins
	}
	if aOrdered && aIdx != bIdx {
		return aIdx < bIdx
	}
	if aPrio != bPrio {
		return aPrio < bPrio // 0 is most urgent
	}
	return aPos < bPos
}

// mergeEnv layers a stage's env over its source's. A stage overrides a
// repo-wide default, never the reverse: the narrower scope wins.
func mergeEnv(source, stage map[string]string) map[string]string {
	if len(stage) == 0 {
		return source
	}
	out := make(map[string]string, len(source)+len(stage))
	for k, v := range source {
		out[k] = v
	}
	for k, v := range stage {
		out[k] = v
	}
	return out
}

func mustSource(cfg *config.Config, name string) config.Source {
	for _, s := range cfg.Sources {
		if s.Name == name {
			return s
		}
	}
	return config.Source{}
}

// Package pipeline decides which item moves where, and enforces the ordering
// rules from docs/CONTRACTS.md §4 and §5.
//
// v1 is a pipeline, not a board: items advance automatically and the only human
// control surface is the order of the inputs.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/source"
)

// Locks enforces concurrency along three axes at once.
//
//   - perSource is 1 and not configurable above it: a source maps to a git
//     worktree, and two agents in one checkout corrupt each other.
//   - perStage bounds how many items sit in one stage at a time. One means the
//     pipeline behaves like a real line — a station works a single item, and
//     the next waits — while different stations run at once.
//   - global caps the total, because every slot is an agent and they are not
//     free.
//
// A transition needs a slot on all three, and takes none unless it can have
// them all: holding one while waiting for another is how two schedulers
// deadlock each other.
type Locks struct {
	mu       sync.Mutex
	bySource map[string]bool
	byStage  map[string]int
	perStage int
	global   chan struct{}
}

func NewLocks(global, perStage int) *Locks {
	if global < 1 {
		global = 1
	}
	if perStage < 1 {
		perStage = 1
	}
	return &Locks{
		bySource: map[string]bool{},
		byStage:  map[string]int{},
		perStage: perStage,
		global:   make(chan struct{}, global),
	}
}

// TryAcquire takes a slot on all three axes, or reports false without blocking.
// Never blocks, so a busy source is skipped rather than queueing work behind it.
func (l *Locks) TryAcquire(src, stage string) bool {
	l.mu.Lock()
	if l.bySource[src] || l.byStage[stage] >= l.perStage {
		l.mu.Unlock()
		return false
	}
	l.bySource[src] = true
	l.byStage[stage]++
	l.mu.Unlock()

	select {
	case l.global <- struct{}{}:
		return true
	default:
		// The global cap is full; give back what was taken rather than hold it.
		l.mu.Lock()
		delete(l.bySource, src)
		l.byStage[stage]--
		l.mu.Unlock()
		return false
	}
}

// Busy reports whether a transition would be refused right now. Advisory: the
// scheduler uses it to skip candidates cheaply, and TryAcquire remains the
// only authority.
func (l *Locks) Busy(src, stage string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bySource[src] || l.byStage[stage] >= l.perStage
}

func (l *Locks) Release(src, stage string) {
	l.mu.Lock()
	delete(l.bySource, src)
	if l.byStage[stage] > 0 {
		l.byStage[stage]--
	}
	l.mu.Unlock()
	select {
	case <-l.global:
	default:
	}
}

// Attempts counts consecutive failures per item AND stage so maxAttempts can
// force an item out of a retry loop.
//
// Per stage, not per item: stages can form a cycle — a review that sends work
// back to be reimplemented — and the implementing stage succeeds on every pass
// round. Counting per item alone let that success clear the reviewing stage's
// tally, so the loop could never reach its limit and would run forever.
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
	Item    model.Item    `json:"item"`
	Stage   string        `json:"stage"`
	From    string        `json:"from"`
	Next    string        `json:"next,omitempty"`
	Outcome model.Outcome `json:"outcome"`
	// Blocked is whether this transition left the item marked. It stays where
	// it is when it does, so Next is empty and the mark is the whole story.
	Blocked bool `json:"blocked,omitempty"`
	// Reason is what the mark says, in the words that will reach the provider
	// and the board. Empty unless Blocked.
	Reason   string `json:"reason,omitempty"`
	RunID    string `json:"runId,omitempty"`
	RunDir   string `json:"runDir,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Err      error  `json:"-"`
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
		locks:    NewLocks(cfg.Concurrency.Global, cfg.Concurrency.PerStage),
		attempts: NewAttempts(),
		clients:  map[string]*source.Client{},
	}
	for _, s := range cfg.Sources {
		e.clients[s.Name] = source.New(cfg, s, r)
	}
	return e
}

// Locks lets a scheduler ask what is currently refused, so it can skip a busy
// candidate instead of launching work that would only be turned away.
func (e *Engine) Locks() *Locks { return e.locks }

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
// Advance performs one transition. The caller must already hold the slot for
// (source, stage) and must release it afterwards — see Locks.TryAcquire.
//
// Scheduling is the caller's business, not the engine's: a scheduler that
// launches work concurrently has to claim the slot before it starts a
// goroutine, because the gap between deciding and acquiring is long enough to
// decide the same thing twice.
func (e *Engine) Advance(ctx context.Context, srcName string, item *model.Item, to string) (*Transition, error) {
	client, ok := e.clients[srcName]
	if !ok {
		return nil, fmt.Errorf("no source named %q", srcName)
	}
	stage, ok := e.cfg.Stage(to)
	if !ok {
		return nil, fmt.Errorf("no stage named %q", to)
	}

	from := item.Stage
	tr := &Transition{Item: *item, Stage: to, From: from}

	// Entering a stage writes the mark as well as the stage, and it writes it
	// off. The scheduler never hands over a marked item, so reaching here means
	// a person cleared it — saying so out loud keeps the provider honest even
	// if they cleared only half of it.
	if _, err := client.Move(ctx, item, to, source.Mark{}); err != nil {
		tr.Err = err
		return tr, err
	}
	tr.Item = *item

	// A stage with no script is a queue, not an error: the item rests here
	// until something else moves it.
	if !stage.Runs() {
		tr.Outcome = model.OutcomeNoop
		return tr, nil
	}

	src := mustSource(e.cfg, srcName)
	script, ok := src.Paths[stage.Script]
	// A stage's params reach only its own script, layered over what the source
	// is: two stages both want a PROMPT, and at source level the second would
	// overwrite the first.
	env := mergeEnv(src.Env, src.Scripts[stage.Script].Params)
	if stage.Run != "" {
		script, ok = "", true // written into the run directory instead
		env = src.Env
	}
	if !ok {
		// resolveSources already recorded why, and Engine skips unhealthy
		// sources — reaching here means one went bad after load.
		tr.Outcome = model.OutcomeFailure
		tr.Err = fmt.Errorf("source %q declares no script named %q", srcName, stage.Script)
		return tr, tr.Err
	}

	res, err := e.runner.Run(ctx, runner.Spec{
		Script:  script,
		Inline:  stage.Run,
		Kind:    "stage",
		Workdir: e.cfg.Workdir(mustSource(e.cfg, srcName)),
		Env:     env,
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

	next, mark := e.route(stage, res, item.ID, tr)
	tr.Next = next
	tr.Blocked = mark.Blocked
	tr.Reason = mark.Reason

	// Marked where it stands. The item does not move, so whoever clears the
	// mark gets the job back in the stage it stopped in — which, because the
	// engine writes provider state before it runs anything, is an interrupted
	// job the next pass simply re-runs.
	if mark.Blocked {
		if _, err := client.Move(ctx, item, to, mark); err != nil {
			tr.Err = err
			return tr, err
		}
		tr.Item = *item
		return tr, nil
	}
	if next == "" || next == to {
		return tr, nil
	}
	if _, err := client.Move(ctx, item, next, source.Mark{}); err != nil {
		tr.Err = err
		return tr, err
	}
	tr.Item = *item
	return tr, nil
}

// route decides what happens after a stage script exits: where the item goes,
// and whether it is marked.
//
// Success is the only route. Everything else leaves the item where it is —
// marked, and therefore out of the scheduler's reach, or unmarked and due
// another attempt. A stage that keeps failing must eventually stop being
// retried, or the scheduler hands it the same item on every poll forever, so
// the mark is what ends the loop and maxAttempts is how many tries it gets
// first: unset means one, and one failure marks.
func (e *Engine) route(s *config.Stage, res *runner.Result, itemID string, tr *Transition) (string, source.Mark) {
	key := itemID + "\x00" + s.Name
	switch res.Run.Outcome {
	case model.OutcomeSuccess:
		e.attempts.Clear(key)
		return s.OnSuccess, source.Mark{}
	case model.OutcomeNoop:
		e.attempts.Clear(key)
		return "", source.Mark{}
	case model.OutcomeBlocked:
		// A decision, not a fault: no retry could change the answer.
		e.attempts.Clear(key)
		return "", source.Mark{Blocked: true, Reason: Reason(s.Name, res.Run, res.Data, s.Timeout.D(), 0)}
	default: // failure, timeout
		n := e.attempts.Bump(key)
		tr.Attempts = n
		max := s.MaxAttempts
		if max < 1 {
			max = 1
		}
		if n < max {
			// Left unmarked in the stage it failed in, which is what an
			// unfinished job looks like: the next pass re-runs it.
			return "", source.Mark{}
		}
		e.attempts.Clear(key)
		return "", source.Mark{Blocked: true, Reason: Reason(s.Name, res.Run, res.Data, s.Timeout.D(), n)}
	}
}

// Reason says, in one line, why a finished run leaves the item needing a human.
//
// Two sources, and they mean different things. A script that decided a person
// is needed writes {"blocked": true, "reason": "..."} into $CONVEYOR_RESULT —
// that is the agents' convention and it wins, because it is the only one that
// can say anything specific. A script that merely broke says nothing, so the
// run record is asked instead: exit code and outcome, the same words the log
// is filed under. Never the log itself, which is prose and is never parsed.
func Reason(stage string, run model.Run, data json.RawMessage, timeout time.Duration, attempts int) string {
	if r := given(data); r != "" {
		return r
	}
	switch run.Outcome {
	case model.OutcomeBlocked:
		return fmt.Sprintf("%s asked for a human but gave no reason; read the run log", stage)
	case model.OutcomeTimeout:
		return fmt.Sprintf("%s timed out after %s", stage, timeout)
	}
	what := fmt.Sprintf("%s failed (exit %d)", stage, run.ExitCode)
	if attempts > 1 {
		what += fmt.Sprintf(" on attempt %d", attempts)
	}
	return what
}

// given reads the agents' convention out of the result file. It is a convention
// and not a requirement — a script that just exits 20 has no reason to give,
// and anything else in the file is simply not one.
func given(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var v struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(data, &v) != nil {
		return ""
	}
	return v.Reason
}

// Target returns the stage an item should be moved into next, if any.
//
// Two cases, and the second is recovery. An item resting in a queue stage (no
// onEnter) advances to that stage's success target. An item found *in* a stage
// that has a script means a previous run did not finish — the engine writes
// provider state before running, so this is what a crash looks like — and the
// right answer is to run that stage again rather than skip past it.
func Target(cfg *config.Config, it *model.Item) (string, bool) {
	// A marked item is waiting for a person, and not picking it is the whole
	// mechanism: it is what stops the stage it stopped in from being re-run on
	// every poll, which is the job the old terminal `blocked` stage did by
	// carrying the item out of the line.
	if it.Blocked {
		return "", false
	}
	stage, ok := cfg.Stage(it.Stage)
	if !ok || stage.Terminal {
		return "", false
	}
	if stage.Runs() {
		return stage.Name, true // re-run: recover an interrupted stage
	}
	if stage.OnSuccess == "" {
		return "", false // a queue with nowhere to go: items rest here
	}
	return stage.OnSuccess, true
}

// The ladder below is the whole of v1's scheduling.
//
// Recovery comes first, ahead of any human ordering: an item found inside a
// stage that runs a script is a job that started and did not finish, and the
// provider already says so. Leaving it behind a full backlog would strand it
// wearing a status it is not in, which is the opposite of what writing provider
// state before the run is for.
//
// After that, v1's only human control is the order of the inputs, so an item's
// position in `order` beats its priority. Items not named there fall back to
// priority, then to the source's own listing order, which keeps the choice
// stable across polls.
//
// Pick chooses the next item to work from a listing. It is the head of Order.
func Pick(cfg *config.Config, items []model.Item, order []string) (*model.Item, string) {
	pos := index(order)
	best := -1
	var bestTarget string
	var bestC candidate

	for i := range items {
		c, target := rate(cfg, &items[i], i, pos)
		if !c.workable {
			continue
		}
		if best == -1 || better(c, bestC) {
			best, bestTarget, bestC = i, target, c
		}
	}
	if best == -1 {
		return nil, ""
	}
	return &items[best], bestTarget
}

// Order sorts a listing into the order the scheduler will work it: the same
// ladder Pick maximises over, applied to everything instead of just the winner.
//
// It exists so the board cannot disagree with the engine. A UI that sorts the
// cards itself is a second copy of these rules, and the two drift — a card
// sitting at the top of a column that the scheduler will reach fourth is a
// board that lies. The server hands out items already in this order, and
// drawing them in the order they arrive is then enough.
//
// Items with nowhere to go — marked, terminal, resting in a queue with no exit
// — keep their listing order at the back. They are not in the queue, so they
// have no claim on a place in it.
func Order(cfg *config.Config, items []model.Item, order []string) []model.Item {
	pos := index(order)
	rated := make([]candidate, len(items))
	for i := range items {
		rated[i], _ = rate(cfg, &items[i], i, pos)
	}
	seq := make([]int, len(items))
	for i := range items {
		seq[i] = i
	}
	sort.SliceStable(seq, func(a, b int) bool { return better(rated[seq[a]], rated[seq[b]]) })
	out := make([]model.Item, len(items))
	for i, j := range seq {
		out[i] = items[j]
	}
	return out
}

func index(order []string) map[string]int {
	pos := make(map[string]int, len(order))
	for i, id := range order {
		pos[id] = i
	}
	return pos
}

// rate scores one item's claim on being next, and reports where it would go.
func rate(cfg *config.Config, it *model.Item, listed int, pos map[string]int) (candidate, string) {
	target, ok := Target(cfg, it)
	oi, ordered := pos[it.ID]
	prio := 1 << 30
	if it.Priority != nil {
		prio = *it.Priority
	}
	// Target returns the same stage an item is already in only when that stage
	// runs something and the previous run did not finish.
	return candidate{
		workable:   ok,
		recovering: ok && target == it.Stage,
		ordered:    ordered,
		idx:        oi,
		prio:       prio,
		pos:        listed,
	}, target
}

// candidate is one item's claim on being next.
type candidate struct {
	workable   bool // has anywhere to go at all
	recovering bool // a stage it is already in, left unfinished
	ordered    bool // named in the manual order
	idx        int  // where, if it is
	prio       int  // 0 most urgent; unranked is huge
	pos        int  // the source's own listing order
}

// better reports whether a should be worked before b. A ladder, most decisive
// first; each rung only matters when the ones above it tie.
func better(a, b candidate) bool {
	if a.workable != b.workable {
		return a.workable // nothing to run is not a place in the queue
	}
	if a.recovering != b.recovering {
		return a.recovering // finish what was started
	}
	if a.ordered != b.ordered {
		return a.ordered // an explicitly ordered item beats an unordered one
	}
	if a.ordered && a.idx != b.idx {
		return a.idx < b.idx
	}
	if a.prio != b.prio {
		return a.prio < b.prio // 0 is most urgent
	}
	return a.pos < b.pos
}

func mergeEnv(env, params map[string]string) map[string]string {
	if len(params) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(params))
	for k, v := range env {
		out[k] = v
	}
	for k, v := range params {
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

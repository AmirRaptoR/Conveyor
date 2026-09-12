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
	"strings"
	"sync"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/source"
)

// Locks enforces concurrency along three axes at once.
//
//   - perSource bounds how many items of one repository run at once. Above 1
//     only because an item works in its own git worktree rather than in the
//     source's checkout (agents/_worktree): two agents in one directory
//     overwrite each other, two agents in two worktrees of one repository share
//     only the object store, which git already makes safe.
//   - perStage bounds how many items sit in one stage at a time. One means the
//     pipeline behaves like a real line — a station works a single item, and
//     the next waits — while different stations run at once.
//   - global caps the total number of transitions.
//   - resources bound the scarce things the work actually consumes, by name:
//     one account's quota, one deploy host, one database. This is the axis
//     that means anything, because a transition is not what runs out. `global`
//     counts every transition equally, so the number had to be chosen for the
//     agents and a stage that only reads an API queued behind them for no
//     reason; a stage naming no resource is now bounded by nothing but
//     `global`, which is where the cheap work belongs.
//
// A transition needs a slot on every axis, and takes none unless it can have
// them all: holding one while waiting for another is how two schedulers
// deadlock each other.
type Locks struct {
	mu          sync.Mutex
	bySource    map[string]int
	byStage     map[string]int
	byResource  map[string]int
	perSource   int
	perStage    int
	perResource map[string]int
	global      chan struct{}
}

func NewLocks(global, perStage, perSource int, resources map[string]int) *Locks {
	if global < 1 {
		global = 1
	}
	if perStage < 1 {
		perStage = 1
	}
	if perSource < 1 {
		perSource = 1
	}
	limits := make(map[string]int, len(resources))
	for k, v := range resources {
		if v < 1 {
			v = 1
		}
		limits[k] = v
	}
	return &Locks{
		bySource:    map[string]int{},
		byStage:     map[string]int{},
		byResource:  map[string]int{},
		perSource:   perSource,
		perStage:    perStage,
		perResource: limits,
		global:      make(chan struct{}, global),
	}
}

// full reports the first axis that would refuse this transition, or "".
// Caller holds l.mu.
func (l *Locks) full(src, stage string, resources []string) string {
	if l.bySource[src] >= l.perSource {
		return "source " + src
	}
	if l.byStage[stage] >= l.perStage {
		return "stage " + stage
	}
	for _, r := range resources {
		// A resource with no declared limit is refused rather than treated as
		// unlimited. Config validation already rejects one, so reaching here
		// means the config changed under a running engine, and "unlimited" is
		// the wrong way to be wrong about a thing somebody named because it
		// was scarce.
		limit, ok := l.perResource[r]
		if !ok || l.byResource[r] >= limit {
			return "resource " + r
		}
	}
	return ""
}

// TryAcquire takes a slot on every axis, or reports false without blocking.
// Never blocks, so a busy source is skipped rather than queueing work behind it.
func (l *Locks) TryAcquire(src, stage string, resources ...string) bool {
	l.mu.Lock()
	if l.full(src, stage, resources) != "" {
		l.mu.Unlock()
		return false
	}
	l.bySource[src]++
	l.byStage[stage]++
	for _, r := range resources {
		l.byResource[r]++
	}
	l.mu.Unlock()

	select {
	case l.global <- struct{}{}:
		return true
	default:
		// The global cap is full; give back what was taken rather than hold it.
		l.give(src, stage, resources)
		return false
	}
}

// Busy reports whether a transition would be refused right now. Advisory: the
// scheduler uses it to skip candidates cheaply, and TryAcquire remains the
// only authority.
func (l *Locks) Busy(src, stage string, resources ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.full(src, stage, resources) != ""
}

// Holding names the axis that would refuse this transition, for a board that
// has to say why nothing started. Empty when nothing would refuse it.
func (l *Locks) Holding(src, stage string, resources ...string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.full(src, stage, resources)
}

func (l *Locks) Release(src, stage string, resources ...string) {
	l.give(src, stage, resources)
	select {
	case <-l.global:
	default:
	}
}

func (l *Locks) give(src, stage string, resources []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bySource[src] > 0 {
		l.bySource[src]--
	}
	if l.byStage[stage] > 0 {
		l.byStage[stage]--
	}
	for _, r := range resources {
		if l.byResource[r] > 0 {
			l.byResource[r]--
		}
	}
}

// Snapshot reports what the locks are currently holding.
//
// Diagnostic, and it exists because its absence cost an evening: a board that
// launches nothing looks identical whether the scheduler is asleep, the items
// are unworkable, or a slot was taken and never given back. The first two can
// be read off /api/state; the third could not be seen at all.
func (l *Locks) Snapshot() (bySource, byStage map[string]int, global, globalMax, perSource, perStage int) {
	bySource, byStage, _, global, globalMax, perSource, perStage = l.snapshot()
	return
}

// Resources reports each declared resource as held/limit, including the ones
// nothing is currently using — a limit nobody can see is a limit nobody can
// reason about when the board is not starting anything.
func (l *Locks) Resources() map[string][2]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string][2]int, len(l.perResource))
	for name, limit := range l.perResource {
		out[name] = [2]int{l.byResource[name], limit}
	}
	return out
}

func (l *Locks) snapshot() (bySource, byStage, byResource map[string]int, global, globalMax, perSource, perStage int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bySource, byStage, byResource = map[string]int{}, map[string]int{}, map[string]int{}
	for k, v := range l.bySource {
		if v != 0 {
			bySource[k] = v
		}
	}
	for k, v := range l.byStage {
		if v != 0 {
			byStage[k] = v
		}
	}
	for k, v := range l.byResource {
		if v != 0 {
			byResource[k] = v
		}
	}
	return bySource, byStage, byResource, len(l.global), cap(l.global), l.perSource, l.perStage
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
	// and the board; Kind is the same stop in one word. Empty unless Blocked.
	Reason   string `json:"reason,omitempty"`
	Kind     string `json:"kind,omitempty"`
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
		locks:    NewLocks(cfg.Concurrency.Global, cfg.Concurrency.PerStage, cfg.Concurrency.PerSource, cfg.Resources),
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
func (e *Engine) Advance(ctx context.Context, srcName string, item *model.Item, to string, resume model.Resume) (*Transition, error) {
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

	// A recovery re-entry into the very stage the item is already in (F04):
	// if the last run of this stage for this item already succeeded and only
	// its outgoing move never got confirmed, the stage does not run again —
	// only the move is retried. Re-running a stage script that already did
	// its work is the side effect this exists to stop.
	if to == from {
		if pending, ok := runner.PendingMove(e.runner.Root, item.ID, to); ok {
			return e.resumePendingMove(ctx, client, item, tr, pending)
		}
	}

	src := mustSource(e.cfg, srcName)
	script, ok := src.Paths[stage.Script]
	timeout := timeoutFor(src, stage)
	// A stage's params reach only its own script, layered over what the source
	// is: two stages both want a PROMPT, and at source level the second would
	// overwrite the first.
	env := mergeEnv(src.Env, src.Scripts[stage.Script].Params)
	if stage.Run != "" {
		script, ok = "", true // written into the run directory instead
		env = src.Env
	}
	// A person pressed a button, and this is the run it was armed for. In the
	// environment as well as on stdin because the scripts that read it are
	// shell, and one `[[ -n "$CONVEYOR_MANUAL" ]]` beats parsing stdin twice.
	if resume.Manual != "" {
		env = mergeEnv(env, map[string]string{"CONVEYOR_MANUAL": resume.Manual})
	}

	// res always ends up non-nil below unless the failure is one route() has
	// no run record to route through at all (the run directory itself could
	// not be created) — everything else, including a script that could not be
	// started and a source that declares none, is a stage outcome like any
	// other and must count toward maxAttempts rather than being retried
	// forever uncounted or returned before an attempt is recorded.
	var res *runner.Result
	var runErr error
	if !ok {
		// resolveSources already recorded why, and Engine skips unhealthy
		// sources — reaching here means one went bad after load.
		msg := fmt.Sprintf("source %q declares no script named %q", srcName, stage.Script)
		runErr = errors.New(msg)
		res = &runner.Result{Run: model.Run{
			Source: srcName, ItemID: item.ID, Kind: "stage", From: from, To: to,
			Outcome: model.OutcomeFailure, Error: msg,
		}}
	} else {
		res, runErr = e.runner.Run(ctx, runner.Spec{
			Script:  script,
			Inline:  stage.Run,
			Kind:    "stage",
			Workdir: e.cfg.Workdir(mustSource(e.cfg, srcName)),
			Env:     env,
			Source:  srcName,
			Item:    item,
			From:    from,
			To:      to,
			Timeout: timeout,
			Stdin: model.StageInput{Item: item, Stage: to, From: from,
				Answer: resume.Answer, Session: resume.Session, Manual: resume.Manual},
		})
		if runErr != nil && res == nil {
			// A genuine infrastructure failure before the script had any
			// chance to run — its own run directory could not even be
			// created. There is no run record to route through, so this
			// cannot be counted as an attempt or marked; the caller is left
			// to decide how to back off.
			tr.Err = runErr
			tr.Outcome = model.OutcomeFailure
			return tr, runErr
		}
	}

	tr.Outcome = res.Run.Outcome
	tr.RunID = res.Run.ID
	tr.RunDir = res.Run.Dir
	if runErr != nil {
		tr.Err = runErr
	}

	next, mark := e.route(stage, res, item.ID, tr)
	tr.Next = next
	tr.Blocked = mark.Blocked
	tr.Reason = mark.Reason
	tr.Kind = mark.Kind

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
		return tr, runErr
	}
	if next == "" || next == to {
		return tr, runErr
	}
	// Recorded against this run before the write is even attempted: if the
	// process dies between here and the move landing, the next recovery pass
	// still finds NextStage set and knows the script does not need re-running
	// — only the move does.
	res.Run.NextStage = next
	_ = runner.WriteMeta(&res.Run)
	if _, err := client.Move(ctx, item, next, source.Mark{}); err != nil {
		tr.Err = err
		return tr, err
	}
	res.Run.MoveConfirmed = true
	_ = runner.WriteMeta(&res.Run)
	tr.Item = *item
	return tr, runErr
}

// maxMoveAttempts bounds how many times resumePendingMove retries an
// outgoing write that keeps failing after its stage already succeeded,
// before marking the item so a person decides. Not configuration — a
// provider write either recovers in a poll or two or it needs a person,
// and this is the same order of magnitude as the rest of the engine's
// fixed retry constants (CLAUDE.md: RESUME_MAX_ATTEMPTS).
const maxMoveAttempts = 3

// resumePendingMove retries only the outgoing move a previous, successful run
// of this stage left unconfirmed (F04) — never the stage script itself.
func (e *Engine) resumePendingMove(ctx context.Context, client *source.Client, item *model.Item, tr *Transition, pending model.Run) (*Transition, error) {
	tr.Outcome = model.OutcomeSuccess
	tr.RunID = pending.ID
	tr.RunDir = pending.Dir
	next := pending.NextStage

	if _, err := client.Move(ctx, item, next, source.Mark{}); err != nil {
		pending.MoveAttempts++
		tr.Attempts = pending.MoveAttempts
		tr.Err = err
		if pending.MoveAttempts >= maxMoveAttempts {
			mark := source.Mark{Blocked: true, Kind: "error",
				Reason: fmt.Sprintf("%s: the stage finished but moving it on to %s kept failing: %s", pending.To, next, err)}
			if _, mErr := client.Move(ctx, item, pending.To, mark); mErr == nil {
				tr.Item = *item
				tr.Blocked = true
				tr.Reason = mark.Reason
				tr.Kind = mark.Kind
				// Marked: this pending move is settled one way or another,
				// so it must not be found and retried again.
				pending.MoveConfirmed = true
				_ = runner.WriteMeta(&pending)
				return tr, err
			}
			// The mark itself could not be written either; leave the
			// pending record as is and let the next poll try again.
		}
		_ = runner.WriteMeta(&pending)
		return tr, err
	}
	pending.MoveConfirmed = true
	_ = runner.WriteMeta(&pending)
	tr.Item = *item
	tr.Next = next
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
		return "", Marked(s.Name, res.Run, res.Data, s.Timeout.D(), 0)
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
		return "", Marked(s.Name, res.Run, res.Data, s.Timeout.D(), n)
	}
}

// Marked describes, in one line and one word, why a finished run leaves the
// item needing a human.
//
// Two sources, and they mean different things. A script that decided a person
// is needed writes {"blocked": true, "kind": "...", "reason": "..."} into
// $CONVEYOR_RESULT — that is the agents' convention and it wins, because it is
// the only one that can say anything specific. A script that merely broke says
// nothing, so the run record is asked instead: exit code and outcome, the same
// words the log is filed under. Never the log itself, which is prose and is
// never parsed.
func Marked(stage string, run model.Run, data json.RawMessage, timeout time.Duration, attempts int) source.Mark {
	said := given(data)
	m := source.Mark{Blocked: true, Reason: said.Reason, Kind: kind(said.Kind)}

	switch run.Outcome {
	case model.OutcomeBlocked:
		if m.Kind == "" {
			m.Kind = "decision" // exit 20 is the convention for "a human must decide"
		}
		if m.Reason == "" {
			m.Reason = fmt.Sprintf("%s asked for a human but gave no reason; read the run log", stage)
		}
	case model.OutcomeTimeout:
		if m.Kind == "" {
			m.Kind = "timeout"
		}
		if m.Reason == "" {
			m.Reason = fmt.Sprintf("%s timed out after %s", stage, timeout)
		}
	default:
		if m.Kind == "" {
			m.Kind = "error"
		}
		if m.Reason == "" {
			// run.Error is set when the script could not be run at all
			// (missing, not executable, or the source declares none) —
			// "exit -1" or "exit 0" would say nothing true about a script
			// that never started, so this is preferred whenever it is set.
			if run.Error != "" {
				m.Reason = fmt.Sprintf("%s: %s", stage, run.Error)
			} else {
				m.Reason = fmt.Sprintf("%s failed (exit %d)", stage, run.ExitCode)
			}
			if attempts > 1 {
				m.Reason += fmt.Sprintf(" on attempt %d", attempts)
			}
		}
	}
	return m
}

// kind normalises a script's one-word summary, or drops it.
//
// The vocabulary is the scripts' and the engine never reads it — but it becomes
// a label on someone's issue tracker and a chip on a board, so its *shape* is
// the engine's business. A paragraph in the kind field is a script that misread
// the contract, and passing it through would put a paragraph on a label.
func kind(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 24 {
		return ""
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == ' ' {
			continue
		}
		return ""
	}
	return s
}

// said is the agents' convention, read out of the result file. It is a
// convention and not a requirement — a script that just exits 20 has nothing to
// say, and anything else in the file is simply not it.
type said struct {
	Reason string `json:"reason"`
	Kind   string `json:"kind"`
}

func given(data json.RawMessage) said {
	var v said
	if len(data) == 0 || json.Unmarshal(data, &v) != nil {
		return said{}
	}
	return v
}

// Target returns the stage an item should be moved into next, if any.
//
// Two cases, and the second is recovery. An item resting in a queue stage (no
// onEnter) advances to that stage's success target. An item found *in* a stage
// that has a script means a previous run did not finish — the engine writes
// provider state before running, so this is what a crash looks like — and the
// right answer is to run that stage again rather than skip past it.
func Target(cfg *config.Config, it *model.Item, d Deps) (string, bool) {
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
	next := stage.Name
	if !stage.Runs() {
		if stage.OnSuccess == "" {
			return "", false // a queue with nowhere to go: items rest here
		}
		next = stage.OnSuccess
	}
	// An item sequenced behind something has nowhere to go until that thing
	// gets there first. Here, rather than only in `rate`, so the drag
	// endpoint and the stall counter get the same answer the scheduler does
	// — one statement of the rule, not three.
	if _, held := d.Held(it, next); held {
		return "", false
	}
	return next, true
}

// The ladder below is the whole of v1's scheduling.
//
// **The line is worked from its far end backwards.** How far along an item is
// outranks everything except being workable at all: a job one stage from done
// is picked before a job one stage from started. A pipeline exists to finish
// items, and a scheduler that always reaches for the head of the backlog keeps
// widening the band of half-finished work — every one of them holding a
// worktree, a branch and an open pull request that goes stale while the line
// starts something new. Depth is read off the stage an item is *heading into*,
// which is the config's own stage order, so "later" means later in the pipeline
// the person wrote and nothing else.
//
// Recovery comes next, and at equal depth it comes first: an item found inside
// a stage that runs a script is a job that started and did not finish, and the
// provider already says so. Leaving it behind a full backlog would strand it
// wearing a status it is not in, which is the opposite of what writing provider
// state before the run is for. It only ever ties with the queue feeding its own
// stage, so this still beats every backlog item competing with it.
//
// After that, v1's only human control is the order of the inputs, so an item's
// position in `order` beats its priority. Items not named there fall back to
// priority, then to the source's own listing order, which keeps the choice
// stable across polls. Those three decide *within* a stage — which is where the
// human lever belongs, because it is a choice about what to start next, and
// dragging a backlog card can no longer jump the queue past work in flight.
//
// Pick chooses the next item to work from a listing. It is the head of Order.
func Pick(cfg *config.Config, items []model.Item, order []string, d Deps) (*model.Item, string) {
	pos := index(order)
	depths := stageDepths(cfg)
	best := -1
	var bestTarget string
	var bestC candidate

	for i := range items {
		c, target := rate(cfg, &items[i], i, pos, depths, d)
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
// — sort at the back. They are not in the queue, so they have no claim on a
// place in it. Terminal items sort ahead of the rest of that tail, newest
// finishedAt first (falling back to listing order when it is empty or does
// not parse as RFC 3339); a marked item and a queue with no exit keep the
// rungs Pick would use if they were workable — manual order, then priority,
// then listing order — which is the doctor sweep's order too, unaffected by
// any of this since it only ever walks marked items.
func Order(cfg *config.Config, items []model.Item, order []string, d Deps) []model.Item {
	pos := index(order)
	depths := stageDepths(cfg)
	rated := make([]candidate, len(items))
	for i := range items {
		rated[i], _ = rate(cfg, &items[i], i, pos, depths, d)
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

// stageDepths numbers the stages along the line, so "further along" is a comparison
// and not a guess.
//
// The number is the stage's position in the config, which is already the line's
// order and not merely a listing: a stage that names no `onSuccess` falls
// through to the next one declared. Nothing else could serve. Following
// `onSuccess` from the front would have to answer what a rework edge means —
// review sending an item back to be implemented makes the graph a cycle, with
// no distance to measure — whereas the file says, in the order a person wrote
// it, how far along `approving` is compared to `ready`.
func stageDepths(cfg *config.Config) map[string]int {
	d := make(map[string]int, len(cfg.Stages))
	for i, s := range cfg.Stages {
		d[s.Name] = i
	}
	return d
}

// rate scores one item's claim on being next, and reports where it would go.
func rate(cfg *config.Config, it *model.Item, listed int, pos map[string]int, depth map[string]int, d Deps) (candidate, string) {
	target, ok := Target(cfg, it, d)
	oi, ordered := pos[it.ID]
	prio := 1 << 30
	if it.Priority != nil {
		prio = *it.Priority
	}
	terminal := false
	if stage, found := cfg.Stage(it.Stage); found {
		terminal = stage.Terminal
	}
	finish, hasFinish := time.Time{}, false
	if it.FinishedAt != "" {
		if t, err := time.Parse(time.RFC3339, it.FinishedAt); err == nil {
			finish, hasFinish = t, true
		}
	}
	// Target returns the same stage an item is already in only when that stage
	// runs something and the previous run did not finish.
	return candidate{
		workable:   ok,
		depth:      depth[target],
		recovering: ok && target == it.Stage,
		ordered:    ordered,
		idx:        oi,
		prio:       prio,
		pos:        listed,
		terminal:   terminal,
		hasFinish:  hasFinish,
		finish:     finish,
	}, target
}

// candidate is one item's claim on being next.
type candidate struct {
	workable   bool // has anywhere to go at all
	depth      int  // how far along the line the stage it is heading into sits
	recovering bool // a stage it is already in, left unfinished
	ordered    bool // named in the manual order
	idx        int  // where, if it is
	prio       int  // 0 most urgent; unranked is huge
	pos        int  // the source's own listing order

	// The rest describe an unworkable item's place at the back of the queue.
	// terminal items — finished work — never reach Pick (Target refuses a
	// terminal stage outright), so these three matter only inside Order.
	terminal  bool      // in a terminal stage: finished, not merely stuck
	hasFinish bool      // finishedAt parsed as RFC 3339
	finish    time.Time // parsed, only meaningful when hasFinish
}

// better reports whether a should be worked before b. A ladder, most decisive
// first; each rung only matters when the ones above it tie.
func better(a, b candidate) bool {
	if a.workable != b.workable {
		return a.workable // nothing to run is not a place in the queue
	}
	if !a.workable {
		// Both are off the queue entirely. Finished work sorts ahead of the
		// rest — marked items and queues with no exit — which is what keeps
		// the doctor sweep's order over marked items untouched: it never sees
		// a terminal item in the first place.
		if a.terminal != b.terminal {
			return a.terminal
		}
		if a.terminal {
			if a.hasFinish != b.hasFinish {
				return a.hasFinish // a usable timestamp outranks none at all
			}
			if a.hasFinish && !a.finish.Equal(b.finish) {
				return a.finish.After(b.finish) // newest-finished first
			}
			return a.pos < b.pos
		}
		// Two non-terminal unworkable items: today's rungs, unchanged.
	}
	if a.depth != b.depth {
		return a.depth > b.depth // finish an item before starting another
	}
	if a.recovering != b.recovering {
		return a.recovering // and at one depth, finish what was already running
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

// timeoutFor is how long this source's version of this stage's work gets.
//
// The stage says how long the work gets, and that is the right default: a
// timeout is a statement about the job. But the same job costs different
// amounts in different repositories — reviewing a Flutter app whose whole suite
// runs in 38 seconds is not reviewing a .NET service whose integration tests
// take a quarter of an hour — and one number for both is either too tight for
// the second or no guard at all for the first. Raising the stage to fit the
// slowest repository removes the guard everywhere it was doing its job.
//
// An inline `run:` stage has no per-source entry to carry an override, by
// construction: it exists for the case where every source does the same thing.
func timeoutFor(src config.Source, stage *config.Stage) time.Duration {
	if stage.Run != "" {
		return stage.Timeout.D()
	}
	if t := src.Scripts[stage.Script].Timeout.D(); t > 0 {
		return t
	}
	return stage.Timeout.D()
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

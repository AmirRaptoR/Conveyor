package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/push"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

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
	Blocks map[string]Block `json:"blocks,omitempty"`
	// Times is how long each item has been in the stage it is in, keyed by
	// item id — a sibling of Blocks, kept the same way: written the moment a
	// transition lands an item somewhere new, recovered from run history for
	// what this process did not do itself, pruned when the item leaves the
	// board.
	Times     map[string]ItemTime `json:"times,omitempty"`
	UpdatedAt time.Time           `json:"updatedAt"`
	Polling   bool                `json:"polling"`
	// Agents is how each agent the sources call says it is doing — a usage
	// limit, a quota, whatever its own status script chose to report. Empty
	// when no agent provides one.
	Agents []AgentView `json:"agents,omitempty"`
	// Paused is every agent the scheduler is holding work back from, and why.
	// Empty is the normal state, and an empty board with a pause on it is the
	// one question this view exists to answer: a paused line and an idle line
	// look identical otherwise.
	Paused []PauseView `json:"paused,omitempty"`
	// Slots is what the concurrency locks are holding, against their limits. It
	// is here rather than behind a debug flag because "nothing is starting" is
	// the question this board gets asked most, and a held slot is the one cause
	// that leaves no other trace.
	Slots SlotsView `json:"slots"`
	// Active is every transition running right now. The board lights those
	// stations; without it the page cannot tell work from stillness.
	Active []Active `json:"active"`
}

// SlotsView is the concurrency state, as the scheduler sees it.
type SlotsView struct {
	BySource  map[string]int `json:"bySource"`
	ByStage   map[string]int `json:"byStage"`
	Global    int            `json:"global"`
	GlobalMax int            `json:"globalMax"`
	PerSource int            `json:"perSource"`
	PerStage  int            `json:"perStage"`
	// Running is how many transitions are actually in flight. It should equal
	// Global; anything else is a slot taken and not given back, which stops the
	// board dead while every item still reads as free.
	Running int `json:"running"`
}

type Active struct {
	Source string `json:"source"`
	Stage  string `json:"stage"`
	ItemID string `json:"itemId"`
	Title  string `json:"title"`
	// StartedAt is when this transition's stage script began, so the board
	// can say how long the run in flight has been running.
	StartedAt time.Time `json:"startedAt"`
}

// ItemTime is when an item arrived in the stage it is currently in, as the
// engine last knows it.
//
// Stage is recorded beside the instant so a stale entry — the item has moved
// on since this was written — is detectable rather than silently attributed
// to wherever the card sits now.
type ItemTime struct {
	Stage        string    `json:"stage"`
	EnteredStage time.Time `json:"enteredStage"`
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
	// Kind is the stop in one word, for the card; Reason is the whole of it,
	// for the panel. A board that prints a paragraph on every red card is a
	// board you cannot read across, which is the one thing it is for.
	Kind   string    `json:"kind,omitempty"`
	Reason string    `json:"reason"`
	Stage  string    `json:"stage"`
	RunID  string    `json:"runId,omitempty"`
	At     time.Time `json:"at"`
	// Asked says this stop is a question waiting on a person, not a condition
	// that may have passed. The engine bulk-clears conditions — Unblock all, a
	// total stall, an agent's quota returning — and never bulk-clears
	// questions, because nothing answered one by waiting and handing it back
	// unanswered spends a run to be asked it again. The script declares it; the
	// engine reads the flag and never the word beside it.
	Asked bool `json:"asked"`
	// Session is the agent's own handle on the conversation that stopped, if it
	// left one. Opaque here and never shown: it exists so an answer can be said
	// back into the same conversation instead of starting one that has to
	// rediscover the repository first. Recovered from run history like Reason.
	Session string `json:"-"`
	// Questions is the AskUserQuestion shape the agent wrote beside its
	// reason, if it wrote one: what the board turns into a modal. Opaque
	// here, like Session — the engine never reads inside it.
	Questions json.RawMessage `json:"questions,omitempty"`
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

// PauseView is an agent the scheduler is not dispatching to, and why.
//
// A usage limit belongs to an agent, not to an item: every stage naming that
// agent meets the same wall, in every repository, because the quota is one
// account's. So the line stops for that agent rather than converting the board
// into marked items at whatever rate it can start runs — which is what it did
// before, once per item and then again every time a timer handed them back.
type PauseView struct {
	Agent string `json:"agent"`
	// Until is when the agent said its quota returns. Zero means it did not
	// say, and the pause lasts until a probe reports something else — never
	// forever: every discovery tick either renews it or lifts it.
	Until  time.Time `json:"until,omitempty"`
	Since  time.Time `json:"since"`
	Reason string    `json:"reason,omitempty"`
	// FromMark is a pause the first refused run caused, rather than one a
	// status probe found. It is the difference between stopping in seconds and
	// stopping a poll later, which at four global slots is several more runs
	// spent to be told the same thing.
	FromMark bool `json:"fromMark,omitempty"`
}

// Live reports whether this pause still holds at t.
func (p PauseView) Live(t time.Time) bool { return p.Until.IsZero() || t.Before(p.Until) }

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
	// times is when each item arrived in the stage it is currently in, kept
	// in memory and recovered from run history like blocks — the runs are
	// the durable record, this is only the index into them a board read
	// cannot afford to rebuild.
	times map[string]ItemTime
	// paused is the agents the scheduler is holding work back from, by name.
	//
	// Not persisted, deliberately: it is a fact about the outside world right
	// now, and the first discovery tick after a restart re-establishes it from
	// the agents' own status scripts. A pause recovered from disk would be a
	// guess about a window that may have closed while the process was down.
	paused map[string]PauseView

	hub   *hub
	order *store.Order
	// answers is what a person typed when handing an item back, held until the
	// run it was written for has been given it.
	answers *store.Answers
	active  sync.Map // itemID -> Active, one entry per transition in flight
	// working is which items have a transition in flight, recorded before the
	// goroutine starts rather than from inside it.
	//
	// The locks bound how many transitions run; they say nothing about *which*
	// item each one is for. While perStage and perSource were both 1 the two were
	// indistinguishable — a second launch of the same item needed a free slot in
	// its own stage, and there was never one. Raising either limit separated
	// them, and the scheduler cheerfully picked an item it had already started:
	// the cached listing still showed it unblocked in its old stage, because the
	// first run had not finished moving it yet.
	//
	// Two agents then worked the same issue in the same worktree — the exact
	// collision worktrees exist to prevent — and whichever exited first had its
	// outcome routed as though it were the other's.
	working sync.Map // itemID -> struct{}, held for the life of a transition
	// resting is the items a stage asked to be left alone until the next
	// listing, by ID.
	//
	// Exit 10 means "leave the item where it is, try again next poll"
	// (CONTRACTS §2). It was being answered with "try again now": a finished
	// transition wakes the scheduler, the item is still workable in the stage
	// it never left, and it is by construction the best candidate there is —
	// so it was picked again immediately. An `approving` stage deferring on a
	// quiet pull request re-ran itself every couple of seconds, fourteen
	// hundred runs an hour, every one of them a real API call. The circuit
	// breaker in schedule capped each burst and then let the next one start,
	// which is why this looked like a warning rather than a fault.
	//
	// Every listing clears it wholesale, because a listing IS the next poll.
	// So does the tick button: that gesture means "look again now", and
	// honouring a deferral against it would answer a person with nothing.
	resting map[string]bool
	tick    chan struct{} // one buffered slot: ticks never queue up
	// wake asks the scheduler to look again. One buffered slot, because the
	// question is always the same one — what can move now — and a queue of it
	// would be a queue of duplicates.
	wake     chan struct{}
	inFlight atomic.Int64
	// polling guards discovery against itself: the ticker and the button both
	// ask for it, and running every list script twice at once buys nothing.
	polling atomic.Bool

	// doctorMu guards doctorSweep, which the sweep's own goroutine mutates as
	// each row settles and GET /api/doctor reads concurrently.
	doctorMu    sync.Mutex
	doctorSweep *Sweep

	// Push notifications: the VAPID key pair the browser subscribes against
	// and the subscriptions taken. Nil keys means the data dir refused the
	// key file, and the board simply does not notify.
	pushKeys *push.Keys
	pushSubs *push.Store
}

func New(cfg *config.Config, r *runner.Runner) *Server {
	s := &Server{
		cfg:     cfg,
		run:     r,
		eng:     pipeline.New(cfg, r),
		hub:     newHub(),
		order:   store.OpenOrder(filepath.Join(cfg.DataDir(), "order.json")),
		answers: store.OpenAnswers(filepath.Join(cfg.DataDir(), "answers.json")),
		tick:    make(chan struct{}, 1),
		wake:    make(chan struct{}, 1),
		ctx:     context.Background(),
	}
	s.pushSubs = push.OpenStore(filepath.Join(cfg.DataDir(), "push.json"))
	if keys, err := push.LoadKeys(filepath.Join(cfg.DataDir(), "vapid.json")); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: push notifications off: %v\n", err)
	} else {
		s.pushKeys = keys
	}
	s.blocks = map[string]Block{}
	s.times = map[string]ItemTime{}
	s.paused = map[string]PauseView{}
	s.resting = map[string]bool{}
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

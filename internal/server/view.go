package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	Times map[string]ItemTime `json:"times,omitempty"`
	// TransitionErrors is the last infrastructure failure a transition hit
	// for an item, keyed by id — an initial provider move that failed, an
	// unknown source or stage. Distinct from Blocks: nothing here is a
	// decision for a person, only a fact the board must not lose behind the
	// next successful listing the way State.Warnings would.
	TransitionErrors map[string]TransitionError `json:"transitionErrors,omitempty"`
	// Answers is, per item, whether a person's reply is held and when it was
	// recorded, and — once a run has actually received it — that run's id.
	// Never the reverse order: an answer is never shown consumed before a run
	// has been given it. Pruned when the item leaves the board, the same
	// lifetime rule Times uses.
	Answers   map[string]AnswerView `json:"answers,omitempty"`
	UpdatedAt time.Time             `json:"updatedAt"`
	Polling   bool                  `json:"polling"`
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
	// Mode is what this process is willing to do: "auto", "manual" or
	// "observe" — see Mode. The board reads it to hide a control that would
	// only ever 403.
	Mode string `json:"mode"`
	// Storage is what the run store currently holds and how far back
	// retention still reaches.
	Storage StorageView `json:"storage"`
	// PersistFault is a run whose own record could not be trusted — a full
	// disk during its meta.json write — kept until a later run persists
	// cleanly. Unlike Warnings, refresh never rebuilds this: a disk-full
	// fault must survive to the next poll, not vanish within one interval.
	PersistFault *PersistFault `json:"persistFault,omitempty"`
	// PollNs is the configured poll interval, so the board can tell a stale
	// listing from a fresh one without guessing at a constant of its own —
	// the engine says what "stale" means, the same way it says everything
	// else the page renders (CLAUDE.md: "the board never derives what the
	// engine knows").
	PollNs time.Duration `json:"pollNs"`
}

// PersistFault is one run whose own record-keeping failed — set from
// runner.Result.PersistErr, which is what a failed meta.json write or
// log.txt append leaves there instead of a record that looks like a clean
// success.
type PersistFault struct {
	RunID   string    `json:"runId"`
	ItemID  string    `json:"itemId,omitempty"`
	Source  string    `json:"source,omitempty"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
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
	// RunID is the move run that landed the item in Stage — the same run
	// CONTRACTS §6 pins against retention so the stage-age chip never goes
	// blank out from under a currently-listed item.
	RunID string `json:"-"`
}

// TransitionError is an infrastructure failure a transition hit for an item —
// distinct from a mark: nothing here is a decision for a person, only a fact
// that the last attempt did not get as far as running or moving anything, and
// is due another one. Cleared the moment a later transition for the same item
// succeeds.
type TransitionError struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// AnswerView is what the board can say about a held reply without reading the
// answer itself: when it was recorded, and — once a run has actually been
// given it — which run that was.
type AnswerView struct {
	RecordedAt time.Time `json:"recordedAt"`
	// ConsumedBy is the run id that received this answer, set only after
	// store.Answers.Take has actually spent it — never before, and never
	// merely because a run started.
	ConsumedBy string `json:"consumedBy,omitempty"`
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
	// LastListedAt is the last listing that returned without error, RFC3339,
	// omitted when this source has never been listed successfully — not a
	// zero time, which would render as 1970 on the board.
	LastListedAt string `json:"lastListedAt,omitempty"`
	// ListError is the error from the latest listing attempt, empty when
	// that attempt succeeded (or none has been made yet). It describes only
	// the most recent attempt, not a high-water mark: a source that failed
	// and then succeeded shows no error here, even though LastListedAt is
	// what actually moved.
	ListError string `json:"listError,omitempty"`
	// Stale is this source's current items being its last good listing
	// rather than this poll's — the latest attempt failed or timed out, so
	// LastListedAt names when the items shown actually came from. Read-only:
	// CLAUDE.md — "stale state is for reading, never for acting on" — the
	// scheduler consults the same fact (Server.listErr) to refuse dispatching
	// against a source it cannot currently confirm.
	Stale bool `json:"stale,omitempty"`
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
	// transitionErrs is the last infrastructure error a transition hit for an
	// item — an initial provider move that failed, an unknown source or
	// stage — kept the same way blocks and times are: State.Warnings is
	// replaced wholesale on every refresh, so a transition error recorded
	// only there would vanish behind the next successful listing even though
	// nothing about the item actually changed. Pruned when the item leaves
	// the board, and cleared the moment a later transition for it succeeds.
	transitionErrs map[string]TransitionError
	// confirmedAt is when an item's entry in state.Items was last set by a
	// completed transition (applyTransition), keyed by id. refresh compares
	// this against sourceGen to decide whether a listing that began earlier
	// may still be reporting that transition's pre-image (F03): a listing is
	// trusted for an item only if it began after that item's last confirmed
	// change, never by comparing content. Not bumped by a listing itself —
	// only an engine transition counts, so a listing that merely repeats what
	// is already known does not raise the bar against the next one.
	confirmedAt map[string]time.Time
	// sourceGen is the instant refresh stamped immediately before calling
	// List for that source, keyed by source name — taken per source rather
	// than once for the whole poll, so a slow source cannot make a fast
	// source's fresh listing look stale in the same pass.
	sourceGen map[string]time.Time
	// answerInfo is, per item, when its currently-held or last-recorded
	// answer was recorded, and the run id that consumed it once one has.
	// Memory-only, like paused: it is a fact about this process's own
	// handling of a reply, not something a restart can recover from run
	// history or the answers file, which holds only the reply itself.
	answerInfo map[string]AnswerView
	// everSwept is whether the retention sweep has ever actually deleted a
	// run. It is what tells a run ID that resolves to nothing apart from "it
	// never existed" (404) from "it is gone because retention removed it"
	// (410) — an arbitrary lookup cutoff used to conflate the two.
	everSwept bool
	// sweepHorizon is the oldest day the last sweep left standing, named in a
	// 410 so an operator knows how far back retention still reaches.
	sweepHorizon time.Time
	// paused is the agents the scheduler is holding work back from, by name.
	//
	// Not persisted, deliberately: it is a fact about the outside world right
	// now, and the first discovery tick after a restart re-establishes it from
	// the agents' own status scripts. A pause recovered from disk would be a
	// guess about a window that may have closed while the process was down.
	paused map[string]PauseView
	// listedAt is the last successful listing for each source, by name; a
	// source absent from this map has never been listed. listErr is the
	// latest attempt's error, present only when that attempt failed — a
	// source can be in both maps at once (it listed successfully once, and
	// has failed on every attempt since). Neither survives a restart, the
	// same as paused: a just-started process has nothing to say yet about a
	// source it has not listed.
	listedAt map[string]time.Time
	listErr  map[string]string

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
	// resting is the items left alone until the next listing, by ID.
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
	// The same mechanism now also holds an item back after an infrastructure
	// failure that ran nothing at all — an initial provider move that failed,
	// or an unknown source or stage (F04): retrying those on every scheduler
	// wake instead of waiting for the next listing would hammer an already-
	// failing provider once per poll interval rather than once per listing.
	//
	// A listing clears an item's deferral — a listing IS the next poll — but
	// only if that item's source's own listing *began* after the deferral was
	// set (F03): one already in flight when the deferral was set has not had
	// the chance to answer it yet, and clearing it early would have the
	// scheduler retry a stage the item is not actually resting in front of.
	// See restingAt. The tick button's own clear ignores all of this: that
	// gesture means "look again now", and honouring a deferral against it
	// would answer a person with nothing.
	resting map[string]bool
	// restingAt is when each resting[id] entry was set. refresh clears a
	// deferral only when the item's source's listing this pass began after
	// this instant — one set by a transition that completed after that
	// listing began is not yet answered by it and must survive the clear.
	// The tick button's own clear ignores this: "look again now" overrides
	// every deferral regardless of when it was set.
	restingAt map[string]time.Time
	tick      chan struct{} // one buffered slot: ticks never queue up
	// wake asks the scheduler to look again. One buffered slot, because the
	// question is always the same one — what can move now — and a queue of it
	// would be a queue of duplicates.
	wake     chan struct{}
	inFlight atomic.Int64
	// shutdownWork counts everything drain must wait for that is not a
	// transition: the loops Run starts (poll, button, schedule, stalled,
	// sweep), the goroutines a handler spawns that run a script or write a
	// data file (refresh, runDoctorSweep, unblockAll, a push send), and every
	// in-flight HTTP request. Tracked apart from inFlight so inFlight keeps
	// meaning exactly "transitions" for handleState's Running and for
	// stalled/schedule's own checks, and so drain's give-up message can still
	// name which items a stuck *transition* belongs to — something this
	// counter alone cannot say.
	shutdownWork atomic.Int64
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

	// verify bounds the cost of Auth.Check — see authVerifier.
	verify *authVerifier
	// mode is what this process is willing to do to the pipeline: auto (the
	// scheduler drives it), manual (only the tick button does) or observe
	// (nothing here ever runs a stage, a move or a doctor script). Set once,
	// by Run, before any goroutine or route can read it.
	mode Mode
	// cop rejects unsafe cross-origin requests to every mutation route (F08).
	// GET/HEAD/OPTIONS are always let through, so SSE and the static board are
	// untouched.
	cop *http.CrossOriginProtection
	// listenHost is the host part of the address Run was given, so a Host
	// header naming it is accepted alongside loopback and auth.origins.
	listenHost string
	// listening is a test seam: when set before Run is called, Run reports the
	// address it actually bound (addr may be "127.0.0.1:0", letting the OS
	// pick a port) so a test can dial a real, running server rather than
	// reaching into its internals. Nil in production; Run skips the send.
	listening chan string

	// drainGrace bounds Run's shutdown wait, defaulted in New and overridden
	// only by tests — there is no config key for it, the same way there is
	// none for the runner's own gracePeriod.
	drainGrace time.Duration
}

func New(cfg *config.Config, r *runner.Runner) *Server {
	secureDataDir(cfg.DataDir())
	s := &Server{
		cfg:        cfg,
		run:        r,
		eng:        pipeline.New(cfg, r),
		hub:        newHub(),
		order:      store.OpenOrder(filepath.Join(cfg.DataDir(), "order.json")),
		answers:    store.OpenAnswers(filepath.Join(cfg.DataDir(), "answers.json")),
		tick:       make(chan struct{}, 1),
		wake:       make(chan struct{}, 1),
		ctx:        context.Background(),
		drainGrace: drainGrace,
	}
	s.pushSubs = push.OpenStore(filepath.Join(cfg.DataDir(), "push.json"))
	if keys, err := push.LoadKeys(filepath.Join(cfg.DataDir(), "vapid.json")); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: push notifications off: %v\n", err)
	} else {
		s.pushKeys = keys
	}
	s.blocks = map[string]Block{}
	s.times = map[string]ItemTime{}
	s.transitionErrs = map[string]TransitionError{}
	s.paused = map[string]PauseView{}
	s.resting = map[string]bool{}
	s.restingAt = map[string]time.Time{}
	s.confirmedAt = map[string]time.Time{}
	s.sourceGen = map[string]time.Time{}
	s.answerInfo = map[string]AnswerView{}
	s.mode = ModeAuto
	s.verify = newAuthVerifier(s.cfg.Auth.Check)
	s.listedAt = map[string]time.Time{}
	s.listErr = map[string]string{}
	s.state = State{
		Stages:  stageViews(cfg),
		Sources: sourceViews(cfg, nil, nil),
		Mode:    string(s.mode),
		PollNs:  cfg.Poll.D(),
	}

	// Every log line reaches the browser as it is produced. This is the whole
	// reason logs are a stream and not a file read at the end.
	prev := r.OnLog
	r.OnLog = func(runID string, l runner.LogLine) {
		if prev != nil {
			prev(runID, l)
		}
		s.hub.publish(event{Kind: "log", RunID: runID, Line: &l})
	}
	// Every run this Runner executes — list, move, stage, doctor, status —
	// reaches here, which is what lets one place notice a persistence fault
	// without a check threaded through every call site that starts a run.
	prevResult := r.OnResult
	r.OnResult = func(res *runner.Result) {
		if prevResult != nil {
			prevResult(res)
		}
		s.notePersistFault(res)
	}
	return s
}

// notePersistFault sets or clears the board-visible sticky fault from one
// run's outcome: a run whose meta.json write or log.txt append failed sets it
// (naming the run that failed to record itself honestly); any other run
// persisting means the disk is not, or is no longer, the problem, and clears
// it. It is intentional that this is not scoped to one item or source — a
// full disk is a fact about the run store, not about the run that happened to
// notice it first.
//
// This reads res.PersistErr, not res.Run.Error: Run.Error keeps only the
// first failure a run hit, so a run whose process failed to start *and*
// whose log.txt append failed on the same full disk would carry the start
// failure there, and a prefix check against it would miss the persistence
// failure entirely — masking the fault instead of raising it, and silently
// clearing a real one already on the board.
func (s *Server) notePersistFault(res *runner.Result) {
	if res == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if res.PersistErr != "" {
		s.state.PersistFault = &PersistFault{
			RunID: res.Run.ID, ItemID: res.Run.ItemID, Source: res.Run.Source,
			Message: res.PersistErr, At: time.Now(),
		}
	} else {
		s.state.PersistFault = nil
	}
}

func stageViews(c *config.Config) []StageView {
	out := make([]StageView, len(c.Stages))
	for i, st := range c.Stages {
		out[i] = StageView{Name: st.Name, Script: st.Script, Next: st.OnSuccess, Runs: st.Runs(), Terminal: st.Terminal}
	}
	return out
}

// sourceViews builds the board's per-source view. listedAt and listErr carry
// what refresh has learned about each source's listing so far — nil at
// construction, before anything has been listed — keyed by source name.
func sourceViews(c *config.Config, listedAt map[string]time.Time, listErr map[string]string) []SourceView {
	out := make([]SourceView, len(c.Sources))
	for i, s := range c.Sources {
		v := SourceView{Name: s.Name, Provider: s.Provider.Name, Workdir: c.Workdir(s), Problems: s.Problems}
		if t, ok := listedAt[s.Name]; ok {
			v.LastListedAt = t.UTC().Format(time.RFC3339)
		}
		v.ListError = listErr[s.Name]
		v.Stale = v.ListError != ""
		out[i] = v
	}
	return out
}

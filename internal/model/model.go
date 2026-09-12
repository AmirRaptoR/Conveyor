// Package model holds the shapes defined in docs/CONTRACTS.md. Nothing here
// knows about GitHub, git or AI: an item is whatever a source script emits.
package model

import "time"

// Item is the unit of work. See CONTRACTS.md §1. A GitHub issue and an Azure
// PBI both arrive in this shape, because the source script converts them.
type Item struct {
	// ID must be globally unique and stable across polls. It is the join key
	// for run history and ordering, so a source that lets it change breaks
	// both. Convention: "<source>:<ref>".
	ID     string `json:"id"`
	Ref    string `json:"ref"`
	Source string `json:"source"`
	Stage  string `json:"stage"`
	Title  string `json:"title"`

	Description string   `json:"description,omitempty"`
	URL         string   `json:"url,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	// Priority is 0 = most urgent; nil means unranked. Pointer so that
	// "unranked" and "most urgent" stay distinguishable.
	Priority  *int   `json:"priority,omitempty"`
	Assignee  string `json:"assignee,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	// FinishedAt is when the source considers this item finished; empty if it
	// does not say. Provider-supplied, like CreatedAt and UpdatedAt: a source
	// with no notion of finishing simply sends nothing.
	FinishedAt string `json:"finishedAt,omitempty"`

	// Blocked is a mark on the item, not a place it goes. An item that needs a
	// human stays in the stage it stopped in and wears this; the source reports
	// it in whatever vocabulary the provider has — a label, a field, a column.
	//
	// The scheduler never picks a marked item, and that is what stops a stage
	// being re-run on every poll. It is also what makes unblocking cheap: a
	// person deletes the mark, the next listing shows an unfinished job in the
	// stage it stopped in, and the work resumes there instead of restarting.
	Blocked bool `json:"blocked,omitempty"`

	// DependsOn names the items this one is sequenced behind, by item ID. The
	// engine never parses it out of anything: the provider translates its own
	// vocabulary ("Depends on #33" in a GitHub issue body) into IDs, exactly
	// as it already does for Priority and Blocked.
	//
	// An ID absent from the listing is ignored rather than held forever — a
	// dependency on an un-onboarded issue, another repository, or a typo must
	// not stop the line. agents/_deps still catches those at implement time,
	// which is where a fact about the outside world belongs.
	DependsOn []string `json:"dependsOn,omitempty"`

	// BlockReason is a listing-supplied reason for Blocked, read only when no
	// run in history produced one — a mark this pipeline did not itself make
	// (a closed issue found sitting in a non-terminal stage) has no run to
	// recover a reason from, and without this the card can say only that it
	// is blocked, not why. A run's own recorded reason always wins; see
	// CONTRACTS.md §6 and internal/server's recallBlocks.
	BlockReason string `json:"blockReason,omitempty"`
	// BlockKind is the same stop in one word, when the listing can say it —
	// the provider read it back off whatever it wrote when the mark went on.
	// Same standing as BlockReason: the item's own account of why it stopped,
	// which is the only account that survives a restart, a retention sweep,
	// and a mark set outside this process.
	BlockKind string `json:"blockKind,omitempty"`

	// Raw is provider passthrough: opaque to the engine, handed back to
	// scripts untouched.
	Raw map[string]any `json:"raw,omitempty"`
}

// Outcome is what an exit code means. See CONTRACTS.md §2.
type Outcome string

const (
	OutcomeSuccess Outcome = "success" // 0
	OutcomeNoop    Outcome = "noop"    // 10
	OutcomeBlocked Outcome = "blocked" // 20 — marks the item where it stands
	OutcomeFailure Outcome = "failure" // anything else
	OutcomeTimeout Outcome = "timeout" // killed at the deadline
	// OutcomeRunning is stamped before the script starts and replaced when it
	// ends. A meta.json still saying "running" once no scheduler owns it is a
	// run that was killed, and becomes OutcomeInterrupted.
	OutcomeRunning     Outcome = "running"
	OutcomeInterrupted Outcome = "interrupted"
)

// Exit codes with defined meaning. Every other non-zero code is a failure.
const (
	ExitSuccess = 0
	ExitNoop    = 10
	ExitBlocked = 20
)

// OutcomeFor maps an exit code to its meaning. timedOut is passed separately
// because a killed process's exit code is not meaningful on its own.
func OutcomeFor(code int, timedOut bool) Outcome {
	switch {
	case timedOut:
		return OutcomeTimeout
	case code == ExitSuccess:
		return OutcomeSuccess
	case code == ExitNoop:
		return OutcomeNoop
	case code == ExitBlocked:
		return OutcomeBlocked
	default:
		return OutcomeFailure
	}
}

// NeedsAttention reports whether an outcome should show red and be surfaced
// above everything else. Blocked and failure are both red but mean different
// things: blocked wants a decision, failure may just want a retry.
func (o Outcome) NeedsAttention() bool {
	return o == OutcomeBlocked || o == OutcomeFailure || o == OutcomeTimeout
}

// Run is one script invocation. It is persisted as a self-contained directory
// (CONTRACTS.md §6) so a failure can be handed to another person or agent with
// everything needed to understand it.
type Run struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	ItemID string `json:"itemId,omitempty"`
	// Kind is "list", "move", "stage", "doctor", "status" or "preflight".
	Kind   string `json:"kind"`
	Script string `json:"script"`
	// From and To are stage names; empty for list runs.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`

	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt time.Time     `json:"finishedAt"`
	Duration   time.Duration `json:"durationNs"`
	ExitCode   int           `json:"exitCode"`
	TimedOut   bool          `json:"timedOut"`
	Outcome    Outcome       `json:"outcome"`
	// Error is set when the script could not be run at all (missing file, not
	// executable) as opposed to running and failing.
	Error string `json:"error,omitempty"`

	Dir  string            `json:"-"`
	Env  map[string]string `json:"env,omitempty"`
	Item *Item             `json:"item,omitempty"`

	// NextStage, MoveConfirmed and MoveAttempts are the durable pending-
	// transition record (F04): a stage run that finished successfully but
	// whose *outgoing* move to NextStage has not yet been confirmed. Written
	// against the run that produced them, not a new store — CONTRACTS §6
	// already makes a run directory self-contained, and this is one more
	// fact about the run it belongs to. A recovery that finds NextStage set
	// and MoveConfirmed false retries only the move, never the script that
	// already succeeded; MoveAttempts bounds that retry so a provider that
	// keeps refusing the write eventually marks the item instead of retrying
	// forever.
	NextStage     string `json:"nextStage,omitempty"`
	MoveConfirmed bool   `json:"moveConfirmed,omitempty"`
	MoveAttempts  int    `json:"moveAttempts,omitempty"`
}

// StageInput is the JSON piped to a stage or move script's stdin.
type StageInput struct {
	Item  *Item  `json:"item"`
	Stage string `json:"stage"`
	From  string `json:"from,omitempty"`
	// Blocked is the mark a move script should leave the item wearing. Always
	// present, and always the whole truth: a move writes both the stage and the
	// mark, so there is no second verb to forget to call. Setting and clearing
	// are the same call with a different value.
	Blocked bool `json:"blocked"`
	// BlockedReason is why, when the script said so — the agents' convention is
	// a "reason" in $CONVEYOR_RESULT. A provider that can record it should; one
	// that cannot may ignore it.
	BlockedReason string `json:"blockedReason,omitempty"`
	// BlockedKind is the same stop in one word — "decision", "limit",
	// "worktree" — for a provider that can file it, and for a board that has a
	// line of space and a paragraph to fit in it. Free text to the engine,
	// which normalises it and passes it on; the vocabulary belongs to the
	// scripts, because what stops a deploy is not what stops a refine.
	BlockedKind string `json:"blockedKind,omitempty"`
	// Answer and Session are set only when this run follows a stop that a
	// person answered: the reply they typed, and the conversation to say it in.
	// An adapter that can resume uses both; one that cannot uses the answer
	// alone and starts fresh, which still beats asking the same question twice.
	Answer  string `json:"answer,omitempty"`
	Session string `json:"session,omitempty"`
	// Manual is the name of an action a person pressed on the board, armed
	// for exactly this run and spent by it. It is how a human overrides a
	// wait the script would otherwise sit out — "merge now" rather than
	// "quiet for ten more minutes" — without the engine knowing what any of
	// those words mean. Declared per stage in `actions:` so the board knows
	// which buttons to draw; opaque to the engine beyond that, like Answer.
	Manual string         `json:"manual,omitempty"`
	Config map[string]any `json:"config,omitempty"`
}

// Resume is what a person said, and where the agent should say it.
//
// Both are opaque to the engine. Answer is text a human typed into the board
// when they cleared a mark; Session is whatever handle the agent gave itself
// when it stopped to ask — a Claude Code session id today, anything at all
// tomorrow. The engine stores them, hands them back, and never reads either,
// exactly as it treats Item.Raw. What resuming *means* is the adapter's
// business, because it differs per agent and some cannot resume at all.
type Resume struct {
	Answer  string `json:"answer,omitempty"`
	Session string `json:"session,omitempty"`
	// Manual is an action a person pressed, armed for the next run of that
	// item's stage and spent by it. Kept here rather than in a store of its
	// own because it is the same fact in the same shape — something a person
	// said, held until the run that was waiting for it actually happens, and
	// then gone.
	Manual string `json:"manual,omitempty"`
}

// Waiting is a script saying what it is waiting for, and until when.
//
// Written into $CONVEYOR_RESULT beside a `noop` exit: "nothing to do yet, and
// here is the moment that changes". The engine stores it and hands it to the
// board, which draws the countdown; it never reads Why and never acts on
// Until. A stage that waits for something with no deadline — a review, a
// person — simply gives no Until, and the board says what it is waiting for
// without a clock.
//
// It exists because a resting item is otherwise indistinguishable from a
// stuck one: both sit still, and only the script knows which.
type Waiting struct {
	Until time.Time `json:"until,omitempty"`
	Why   string    `json:"why,omitempty"`
}

// DoctorInput is the JSON piped to a doctor script's stdin: one marked item,
// why it stopped, and its own run history. Its own shape, deliberately not
// StageInput — whose From means "the previous stage" and would be a lie here,
// since a doctor invocation follows no stage at all.
type DoctorInput struct {
	Item    *Item       `json:"item"`
	Stage   string      `json:"stage"`
	Blocked bool        `json:"blocked"`
	Block   DoctorBlock `json:"block"`
	// Runs is this item's own run history, newest first, capped at 20 — every
	// kind, including earlier doctor runs. A directory retention has swept, or
	// that the script cannot read, is the script's problem to tolerate.
	Runs []DoctorRun `json:"runs"`
}

// DoctorBlock is why the item stopped, trimmed for a doctor script. Session is
// deliberately absent — it is the agent's own handle on the conversation that
// stopped, spent by the engine itself when it records an answer, and never
// something a script reads or forwards.
type DoctorBlock struct {
	Kind   string    `json:"kind,omitempty"`
	Reason string    `json:"reason"`
	Stage  string    `json:"stage"`
	RunID  string    `json:"runId,omitempty"`
	At     time.Time `json:"at"`
	Asked  bool      `json:"asked"`
}

// DoctorRun is one run of an item's history, trimmed for a doctor script.
// Deliberately not Run: Run.Env and Run.Item would hand every source
// parameter — tokens included — to a model, while Run.Dir is json:"-" so the
// one field the script actually needs, the path to that run's own files, is
// not even there. Adding Dir to Run is not the fix — RunMeta embeds Run and
// would leak filesystem paths through the existing /api/runs.
type DoctorRun struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Stage      string    `json:"stage,omitempty"` // Run.To — empty for a list run
	Outcome    Outcome   `json:"outcome"`
	ExitCode   int       `json:"exitCode"`
	TimedOut   bool      `json:"timedOut"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Dir        string    `json:"dir"`
}

// ListInput is the JSON piped to a list script's stdin.
type ListInput struct {
	Source string   `json:"source"`
	Stages []string `json:"stages"`
	// TerminalStages is which of Stages are terminal, so a list script can
	// tell a finished item from one that merely stopped without keeping its
	// own copy of the stage graph in source env:. See CONTRACTS.md §1.
	TerminalStages []string       `json:"terminalStages,omitempty"`
	Config         map[string]any `json:"config,omitempty"`
}

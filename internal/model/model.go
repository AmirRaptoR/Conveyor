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

	// Tracking marks an explicit non-work item whose lifecycle is derived from
	// all of its declared Children. It is never inferred from Children alone:
	// ordinary work may carry hierarchy without becoming non-executable.
	Tracking bool `json:"tracking,omitempty"`
	// TrackingError says the source could not provide a complete, representable
	// required-child set. The engine fails the tracking lifecycle closed rather
	// than treating a partial provider response as completion.
	TrackingError string `json:"trackingError,omitempty"`

	// Parent and Children describe tracking structure, not execution order.
	// Every value is a globally-qualified item ID (normally
	// "<source>:<ref>"). Providers must report both directions when they know
	// them; the engine warns about missing or contradictory peers but never
	// turns these fields into scheduling dependencies.
	Parent   string   `json:"parent,omitempty"`
	Children []string `json:"children,omitempty"`

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
	// An ID absent from the listing is invalid state and fails closed. Providers
	// must retain or resolve completed referenced items even when their ordinary
	// history view is bounded; otherwise a real completed predecessor would be
	// indistinguishable from a typo.
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
	// Kind is "list", "move", "stage", "status" or "preflight".
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
	// RetentionClass records whether a stage was agent-backed at dispatch time,
	// so a later config change cannot reclassify its historical bytes.
	RetentionClass string `json:"retentionClass,omitempty"`
	// Error is set when the script could not be run at all (missing file, not
	// executable) as opposed to running and failing.
	Error string `json:"error,omitempty"`

	Dir  string            `json:"-"`
	Env  map[string]string `json:"env,omitempty"`
	Item *Item             `json:"item,omitempty"`

	// NextStage/PendingMark, MoveConfirmed and MoveAttempts are the durable
	// pending-provider-write record (F04): a stage run whose outgoing move or
	// mark has not yet been confirmed. Written
	// against the run that produced them, not a new store — CONTRACTS §6
	// already makes a run directory self-contained, and this is one more
	// fact about the run it belongs to. A recovery that finds NextStage set
	// and MoveConfirmed false retries only the provider write, never the script
	// that already completed.
	NextStage     string `json:"nextStage,omitempty"`
	PendingMark   bool   `json:"pendingMark,omitempty"`
	MarkKind      string `json:"markKind,omitempty"`
	MarkReason    string `json:"markReason,omitempty"`
	MoveConfirmed bool   `json:"moveConfirmed,omitempty"`
	MoveAttempts  int    `json:"moveAttempts,omitempty"`
}

// StageInput is the JSON piped to a stage or move script's stdin.
type StageInput struct {
	Item  *Item  `json:"item"`
	Stage string `json:"stage"`
	From  string `json:"from,omitempty"`
	// Terminal tells a provider move that the target is terminal in the
	// configured pipeline. It is provider-neutral metadata; providers decide
	// whether their native item status needs reconciling with that fact.
	Terminal bool `json:"terminal,omitempty"`
	// TrackingComplete is the narrow authorization that the full-list tracker
	// evaluator proved every required child complete. Terminal alone is not
	// enough: clearing a mark in a terminal stage must never finish an item.
	TrackingComplete bool `json:"trackingComplete,omitempty"`
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
	// ControlCarryover distinguishes an interrupted-run instruction from a
	// person's answer so an adapter can migrate its own older session shape.
	ControlCarryover bool `json:"controlCarryover,omitempty"`
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
	Answer           string `json:"answer,omitempty"`
	Session          string `json:"session,omitempty"`
	ControlCarryover bool   `json:"controlCarryover,omitempty"`
	// Stage binds an armed value to the stage that was waiting for it. Empty
	// preserves answers written before this field existed; new writes always
	// set it so a provider-side stage change cannot deliver stale input to a
	// different script.
	Stage string `json:"stage,omitempty"`
	// Script binds Stage to the executable that was waiting. The value is an
	// engine-only stable identity; adapters still receive only Answer and
	// Session, both opaque.
	Script string `json:"script,omitempty"`
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
// board, which draws the countdown. Untyped waits remain presentation-only;
// typed waits use Class, Key and Until for deterministic recovery while Why
// remains display text. A stage that waits for something with no deadline
// simply gives no Until, and the board says what it is waiting for without a
// clock.
//
// It exists because a resting item is otherwise indistinguishable from a
// stuck one: both sit still, and only the script knows which.
type Waiting struct {
	Until time.Time `json:"until,omitempty"`
	Why   string    `json:"why,omitempty"`
	// Class is the closed recovery policy: network, poll, until, quota or
	// worktree. Empty preserves an ordinary untyped deferral. Key is opaque
	// adapter state compared only for equality, so an unchanged diagnosis can
	// back off without running the stage again.
	Class string `json:"class,omitempty"`
	Key   string `json:"key,omitempty"`
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

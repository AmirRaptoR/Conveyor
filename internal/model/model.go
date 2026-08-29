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

	// Blocked is a mark on the item, not a place it goes. An item that needs a
	// human stays in the stage it stopped in and wears this; the source reports
	// it in whatever vocabulary the provider has — a label, a field, a column.
	//
	// The scheduler never picks a marked item, and that is what stops a stage
	// being re-run on every poll. It is also what makes unblocking cheap: a
	// person deletes the mark, the next listing shows an unfinished job in the
	// stage it stopped in, and the work resumes there instead of restarting.
	Blocked bool `json:"blocked,omitempty"`

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
	// Kind is "list", "move" or "stage".
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
	Answer  string         `json:"answer,omitempty"`
	Session string         `json:"session,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
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
}

// ListInput is the JSON piped to a list script's stdin.
type ListInput struct {
	Source string         `json:"source"`
	Stages []string       `json:"stages"`
	Config map[string]any `json:"config,omitempty"`
}

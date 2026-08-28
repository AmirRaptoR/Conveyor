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

	// Raw is provider passthrough: opaque to the engine, handed back to
	// scripts untouched.
	Raw map[string]any `json:"raw,omitempty"`
}

// Outcome is what an exit code means. See CONTRACTS.md §2.
type Outcome string

const (
	OutcomeSuccess Outcome = "success" // 0
	OutcomeNoop    Outcome = "noop"    // 10
	OutcomeBlocked Outcome = "blocked" // 20
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
	Item   *Item          `json:"item"`
	Stage  string         `json:"stage"`
	From   string         `json:"from,omitempty"`
	Config map[string]any `json:"config,omitempty"`
}

// ListInput is the JSON piped to a list script's stdin.
type ListInput struct {
	Source string         `json:"source"`
	Stages []string       `json:"stages"`
	Config map[string]any `json:"config,omitempty"`
}

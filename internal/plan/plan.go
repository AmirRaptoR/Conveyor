// Package plan defines the agent-neutral todo/plan protocol a run publishes
// on its own sidecar channel (plan.jsonl, named to the script by
// $CONVEYOR_PLAN) — never over stdout/stderr, which stay logs (CONTRACTS
// §2/§6).
//
// One JSON object per line is one revision. Validate checks a single line in
// isolation plus the one piece of cross-line state a caller must carry
// itself: the last accepted rev, so the sequence stays strictly increasing.
package plan

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxLineBytes bounds one line, newline included.
const MaxLineBytes = 64 * 1024

// MaxTodos bounds the todos array of one revision.
const MaxTodos = 200

// MaxTextLen bounds text and active, in Unicode code points.
const MaxTextLen = 500

// MaxIDLen bounds a todo id.
const MaxIDLen = 64

// MaxRevisions bounds how many accepted revisions one run's plan.jsonl may
// hold before a reader stops parsing further lines.
const MaxRevisions = 1000

// MaxRejectedLines bounds how many rejected lines one run's plan.jsonl may
// produce before a reader stops parsing further lines. The 10-line
// diagnostic cap only bounds how many rejections are worth a log line; this
// bounds the reader's own memory and event traffic, since a writer other
// than agents/_plan (the only one the protocol's own limits constrain) could
// otherwise flood plan.jsonl with malformed lines forever.
const MaxRejectedLines = 1000

// StatusPending, StatusInProgress and StatusCompleted are the closed set a
// todo's status is validated against. The engine derives no other meaning
// from it; progress and "current step" are computed by whoever is handed a
// Revision.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

// Todo is one item of a Revision.
type Todo struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
	// Active is the present-tense form shown while Status is in_progress
	// (Claude's activeForm). Omitted when the source event has no
	// equivalent; a reader falls back to Text.
	Active string `json:"active,omitempty"`
}

// Revision is one accepted line of plan.jsonl.
type Revision struct {
	V     int       `json:"v"`
	Rev   int       `json:"rev"`
	At    time.Time `json:"at"`
	Todos []Todo    `json:"todos"`
}

// Progress reports how many todos are completed, out of the total, and the
// active/text of the first in-progress todo in document order (empty when
// there is none).
func (r Revision) Progress() (completed, total int, inProgress string) {
	total = len(r.Todos)
	for _, t := range r.Todos {
		if t.Status == StatusCompleted {
			completed++
		}
		if inProgress == "" && t.Status == StatusInProgress {
			if t.Active != "" {
				inProgress = t.Active
			} else {
				inProgress = t.Text
			}
		}
	}
	return completed, total, inProgress
}

// currentVersion is the only value V may hold. A future protocol revision
// adds a new one rather than reinterpreting this one.
const currentVersion = 1

// wireTodo mirrors Todo but with fields left as json.RawMessage-adjacent
// generic types so Validate can tell "absent" apart from "wrong type"
// itself, rather than letting encoding/json's zero-value defaults blur an
// absent field into an empty string.
type wireTodo struct {
	ID     json.RawMessage `json:"id"`
	Text   json.RawMessage `json:"text"`
	Status json.RawMessage `json:"status"`
	Active json.RawMessage `json:"active"`
}

type wireRevision struct {
	V     json.RawMessage `json:"v"`
	Rev   json.RawMessage `json:"rev"`
	At    json.RawMessage `json:"at"`
	Todos json.RawMessage `json:"todos"`
}

// Validate checks one line (without its trailing newline) against the
// protocol, given the last accepted rev in this run (0 before any revision
// has been accepted). It returns the parsed Revision and an empty reason on
// acceptance, or a zero Revision and a one-sentence reason naming why the
// line was rejected. The line length itself (MaxLineBytes, including the
// newline a caller strips before calling this) is a caller concern, since
// Validate never sees the newline.
func Validate(line []byte, lastRev int) (Revision, string) {
	if len(line)+1 > MaxLineBytes {
		return Revision{}, "line too long"
	}
	if !json.Valid(line) {
		return Revision{}, "not valid JSON"
	}
	var w wireRevision
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&w); err != nil {
		return Revision{}, "not a JSON object"
	}

	if len(w.V) == 0 {
		return Revision{}, "missing v"
	}
	var v int
	if err := json.Unmarshal(w.V, &v); err != nil {
		return Revision{}, "v is not a number"
	}
	if v != currentVersion {
		return Revision{}, "unknown v"
	}

	if len(w.Rev) == 0 {
		return Revision{}, "missing rev"
	}
	var rev int
	if err := json.Unmarshal(w.Rev, &rev); err != nil {
		return Revision{}, "rev is not a number"
	}
	if rev < 1 {
		return Revision{}, "rev must be at least 1"
	}
	if rev <= lastRev {
		return Revision{}, "rev is not increasing"
	}

	if len(w.At) == 0 {
		return Revision{}, "missing at"
	}
	var atStr string
	if err := json.Unmarshal(w.At, &atStr); err != nil {
		return Revision{}, "at is not a string"
	}
	at, err := time.Parse(time.RFC3339, atStr)
	if err != nil {
		return Revision{}, "at is not RFC3339"
	}

	if len(w.Todos) == 0 {
		return Revision{}, "missing todos"
	}
	var rawTodos []json.RawMessage
	if err := json.Unmarshal(w.Todos, &rawTodos); err != nil {
		return Revision{}, "todos is not an array"
	}
	if len(rawTodos) > MaxTodos {
		return Revision{}, "too many todos"
	}

	todos := make([]Todo, 0, len(rawTodos))
	seen := make(map[string]bool, len(rawTodos))
	for _, raw := range rawTodos {
		var wt wireTodo
		if err := json.Unmarshal(raw, &wt); err != nil {
			return Revision{}, "todo is not an object"
		}

		if len(wt.ID) == 0 {
			return Revision{}, "todo missing id"
		}
		var id string
		if err := json.Unmarshal(wt.ID, &id); err != nil {
			return Revision{}, "todo id is not a string"
		}
		id = strings.TrimSpace(id)
		if id == "" {
			return Revision{}, "todo id is empty"
		}
		if utf8.RuneCountInString(id) > MaxIDLen {
			return Revision{}, "todo id too long"
		}
		if seen[id] {
			return Revision{}, "duplicate todo id"
		}
		seen[id] = true

		if len(wt.Text) == 0 {
			return Revision{}, "todo missing text"
		}
		var text string
		if err := json.Unmarshal(wt.Text, &text); err != nil {
			return Revision{}, "todo text is not a string"
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return Revision{}, "todo text is empty"
		}
		if utf8.RuneCountInString(text) > MaxTextLen {
			return Revision{}, "todo text too long"
		}

		if len(wt.Status) == 0 {
			return Revision{}, "todo missing status"
		}
		var status string
		if err := json.Unmarshal(wt.Status, &status); err != nil {
			return Revision{}, "todo status is not a string"
		}
		if status != StatusPending && status != StatusInProgress && status != StatusCompleted {
			return Revision{}, "todo status is not valid"
		}

		var active string
		if len(wt.Active) > 0 {
			if err := json.Unmarshal(wt.Active, &active); err != nil {
				return Revision{}, "todo active is not a string"
			}
			if utf8.RuneCountInString(active) > MaxTextLen {
				return Revision{}, "todo active too long"
			}
		}

		todos = append(todos, Todo{ID: id, Text: text, Status: status, Active: active})
	}

	return Revision{V: v, Rev: rev, At: at, Todos: todos}, ""
}

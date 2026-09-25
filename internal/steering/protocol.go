// Package steering defines the agent-neutral control protocol a run can be
// steered through — the mirror image of internal/plan's plan channel.
// control.jsonl (engine-written, script-read) carries commands;
// control-ack.jsonl (script-written, engine-read) carries the script's
// hello/session/ack lines. Neither is stdout: CONTRACTS §2/§2b/§6.
//
// One JSON object per line is one record. ValidateCommandLine and
// ValidateAckLine each check a single line in isolation plus the one piece
// of cross-line state a caller must carry itself: the last accepted seq in
// that file, so the sequence stays strictly increasing — exactly the
// discipline internal/plan.Validate already follows for `rev`.
package steering

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxLineBytes bounds one line of either channel, newline included.
const MaxLineBytes = 64 * 1024

// MaxTextLen bounds a command's text, in Unicode code points.
const MaxTextLen = 1024

// MaxReasonLen bounds an ack's reason, in Unicode code points.
const MaxReasonLen = 500

// MaxFieldLen bounds id, itemId, runId, session and by.
const MaxFieldLen = 256

// MaxCommands bounds how many commands may ever be appended to one run's
// control.jsonl over its whole lifetime — enforced by the HTTP handler that
// appends them, and defensively by a reader of the file.
const MaxCommands = 50

// MaxAckLines bounds how many ack lines a reader accepts from one run's
// control-ack.jsonl before it stops parsing further — the reader's own
// memory bound, mirroring internal/plan.MaxRevisions.
const MaxAckLines = 200

// KindInstruction and KindPause are the closed set a command's kind, and an
// ack's advertised accepts, are drawn from.
const (
	KindInstruction = "instruction"
	KindPause       = "pause"
)

// StateConsumed, StateRejected, StateCarried and StateDropped are the closed
// set an ack or a carried entry's state is validated against.
const (
	StateConsumed = "consumed"
	StateRejected = "rejected"
	StateCarried  = "carried"
	StateDropped  = "dropped"
)

const currentVersion = 1

// Command is one accepted "command"-type record of control.jsonl.
type Command struct {
	Seq     int       `json:"seq"`
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text"`
	ItemID  string    `json:"itemId"`
	RunID   string    `json:"runId"`
	Session string    `json:"session"`
	By      string    `json:"by"`
}

// CarriedEntry is one entry of a "carried"-type record.
type CarriedEntry struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// Carried is one accepted "carried"-type record of control.jsonl — the
// engine's own record of what restart carry-over did with commands still
// queued when a run ended.
type Carried struct {
	Seq     int            `json:"seq"`
	At      time.Time      `json:"at"`
	Entries []CarriedEntry `json:"entries"`
}

// ControlRecord is one accepted line of control.jsonl: exactly one of
// Command or Carried is set, discriminated by the line's own "type" field.
// A reader ignores a record type it does not know, so a later issue can add
// one without breaking an older engine mid-run.
type ControlRecord struct {
	Command *Command
	Carried *Carried
}

// AckHello is the script's one-time announcement of what it accepts.
type AckHello struct {
	Seq     int       `json:"seq"`
	At      time.Time `json:"at"`
	Accepts []string  `json:"accepts"`
	Session string    `json:"session"`
}

// AckSession names the session the run is currently in; a later one with a
// different, non-empty value replaces the current one.
type AckSession struct {
	Seq     int       `json:"seq"`
	At      time.Time `json:"at"`
	Session string    `json:"session"`
}

// AckAck resolves one command by id.
type AckAck struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	ID     string    `json:"id"`
	State  string    `json:"state"`
	Reason string    `json:"reason"`
}

// AckRecord is one accepted line of control-ack.jsonl: exactly one of
// Hello, Session or Ack is set.
type AckRecord struct {
	Hello   *AckHello
	Session *AckSession
	Ack     *AckAck
}

// validKind reports whether s is a member of the closed kind enum.
func validKind(s string) bool { return s == KindInstruction || s == KindPause }

func validState(s string) bool {
	return s == StateConsumed || s == StateRejected
}

func validCarriedState(s string) bool {
	return s == StateCarried || s == StateDropped
}

// rawFields decodes the handful of top-level fields every record variant on
// either channel might carry, left as json.RawMessage so ValidateX can tell
// "absent" apart from "wrong type" — the same technique internal/plan uses.
type rawFields struct {
	V       json.RawMessage `json:"v"`
	Type    json.RawMessage `json:"type"`
	Seq     json.RawMessage `json:"seq"`
	ID      json.RawMessage `json:"id"`
	At      json.RawMessage `json:"at"`
	Kind    json.RawMessage `json:"kind"`
	Text    json.RawMessage `json:"text"`
	ItemID  json.RawMessage `json:"itemId"`
	RunID   json.RawMessage `json:"runId"`
	Session json.RawMessage `json:"session"`
	By      json.RawMessage `json:"by"`
	Accepts json.RawMessage `json:"accepts"`
	State   json.RawMessage `json:"state"`
	Reason  json.RawMessage `json:"reason"`
	Entries json.RawMessage `json:"entries"`
}

func decodeRaw(line []byte) (rawFields, string) {
	var w rawFields
	if len(line)+1 > MaxLineBytes {
		return w, "line too long"
	}
	if !json.Valid(line) {
		return w, "not valid JSON"
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&w); err != nil {
		return w, "not a JSON object"
	}
	return w, ""
}

func decodeInt(raw json.RawMessage, field string) (int, string) {
	if len(raw) == 0 {
		return 0, "missing " + field
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, field + " is not a number"
	}
	return n, ""
}

func decodeSeq(raw json.RawMessage, lastSeq int) (int, string) {
	seq, reason := decodeInt(raw, "seq")
	if reason != "" {
		return 0, reason
	}
	if seq < 1 {
		return 0, "seq must be at least 1"
	}
	if lastSeq == 0 && seq != 1 {
		return seq, "seq must start at 1"
	}
	if lastSeq > 0 && seq <= lastSeq {
		return 0, "seq is not increasing"
	}
	return seq, ""
}

func decodeVersion(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "missing v"
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return "v is not a number"
	}
	if v != currentVersion {
		return "unknown v"
	}
	return ""
}

func decodeString(raw json.RawMessage, field string) (string, string) {
	if len(raw) == 0 {
		return "", "missing " + field
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", field + " is not a string"
	}
	return s, ""
}

func decodeTime(raw json.RawMessage, field string) (time.Time, string) {
	s, reason := decodeString(raw, field)
	if reason != "" {
		return time.Time{}, reason
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, field + " is not RFC3339"
	}
	return at, ""
}

func boundedField(raw json.RawMessage, field string, max int, required bool) (string, string) {
	if len(raw) == 0 {
		if required {
			return "", "missing " + field
		}
		return "", ""
	}
	s, reason := decodeString(raw, field)
	if reason != "" {
		return "", reason
	}
	if utf8.RuneCountInString(s) > max {
		return "", field + " too long"
	}
	return s, ""
}

func requiredArray(raw json.RawMessage, field string) ([]json.RawMessage, string) {
	if len(raw) == 0 {
		return nil, "missing " + field
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, field + " is not an array"
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, field + " is not an array"
	}
	return values, ""
}

// envelope validates the fields shared by every record. seq is deliberately
// decoded before every other semantic field: once a positive, forward-moving
// sequence number appears on a complete JSON object, that number is consumed
// even if the rest of the line is rejected.
func envelope(line []byte, lastSeq int) (rawFields, int, string) {
	w, reason := decodeRaw(line)
	if reason != "" {
		return w, 0, reason
	}
	seq, reason := decodeSeq(w.Seq, lastSeq)
	if reason != "" {
		return w, seq, reason
	}
	if reason = decodeVersion(w.V); reason != "" {
		return w, seq, reason
	}
	if _, reason = decodeString(w.Type, "type"); reason != "" {
		return w, seq, reason
	}
	if _, reason = decodeTime(w.At, "at"); reason != "" {
		return w, seq, reason
	}
	return w, seq, ""
}

// ValidateCommandLine checks one line of control.jsonl (without its
// trailing newline) against the protocol, given the last accepted seq in
// this run's control.jsonl (0 before any record has been accepted). A
// record of an unknown type is accepted with both fields of ControlRecord
// nil and an empty reason, so an older reader tolerates a later issue's new
// record type rather than rejecting the whole line.
func ValidateCommandLine(line []byte, lastSeq int) (ControlRecord, int, string) {
	w, seq, reason := envelope(line, lastSeq)
	if reason != "" {
		return ControlRecord{}, seq, reason
	}
	typ, reason := decodeString(w.Type, "type")
	if reason != "" {
		return ControlRecord{}, seq, reason
	}
	at, reason := decodeTime(w.At, "at")
	if reason != "" {
		return ControlRecord{}, seq, reason
	}

	switch typ {
	case "command":
		id, reason := boundedField(w.ID, "id", MaxFieldLen, true)
		if reason != "" {
			return ControlRecord{}, seq, reason
		}
		if strings.TrimSpace(id) == "" {
			return ControlRecord{}, seq, "id is empty"
		}
		kind, reason := decodeString(w.Kind, "kind")
		if reason != "" {
			return ControlRecord{}, seq, reason
		}
		if !validKind(kind) {
			return ControlRecord{}, seq, "kind is not valid"
		}
		text, reason := boundedField(w.Text, "text", MaxTextLen, true)
		if reason != "" {
			return ControlRecord{}, seq, reason
		}
		if kind == KindPause && text != "" {
			return ControlRecord{}, seq, "pause text must be empty"
		}
		if kind == KindInstruction && (strings.TrimSpace(text) == "" || strings.TrimSpace(text) != text) {
			return ControlRecord{}, seq, "instruction text must be non-empty and trimmed"
		}
		itemID, reason := boundedField(w.ItemID, "itemId", MaxFieldLen, true)
		if reason != "" {
			return ControlRecord{}, seq, reason
		}
		if strings.TrimSpace(itemID) == "" {
			return ControlRecord{}, seq, "itemId is empty"
		}
		runID, reason := boundedField(w.RunID, "runId", MaxFieldLen, true)
		if reason != "" {
			return ControlRecord{}, seq, reason
		}
		if strings.TrimSpace(runID) == "" {
			return ControlRecord{}, seq, "runId is empty"
		}
		session, reason := boundedField(w.Session, "session", MaxFieldLen, true)
		if reason != "" {
			return ControlRecord{}, seq, reason
		}
		by, reason := boundedField(w.By, "by", MaxFieldLen, true)
		if reason != "" {
			return ControlRecord{}, seq, reason
		}
		cmd := &Command{Seq: seq, ID: id, At: at, Kind: kind, Text: text, ItemID: itemID, RunID: runID, Session: session, By: by}
		return ControlRecord{Command: cmd}, seq, ""
	case "carried":
		rawEntries, reason := requiredArray(w.Entries, "entries")
		if reason != "" {
			return ControlRecord{}, seq, reason
		}
		entries := make([]CarriedEntry, 0, len(rawEntries))
		seen := make(map[string]bool, len(rawEntries))
		for _, raw := range rawEntries {
			var ef struct {
				ID     json.RawMessage `json:"id"`
				State  json.RawMessage `json:"state"`
				Reason json.RawMessage `json:"reason"`
			}
			if err := json.Unmarshal(raw, &ef); err != nil {
				return ControlRecord{}, seq, "entry is not an object"
			}
			id, reason := boundedField(ef.ID, "entry id", MaxFieldLen, true)
			if reason != "" {
				return ControlRecord{}, seq, reason
			}
			if strings.TrimSpace(id) == "" {
				return ControlRecord{}, seq, "entry id is empty"
			}
			if seen[id] {
				return ControlRecord{}, seq, "entries is not a set"
			}
			seen[id] = true
			state, reason := decodeString(ef.State, "entry state")
			if reason != "" {
				return ControlRecord{}, seq, reason
			}
			if !validCarriedState(state) {
				return ControlRecord{}, seq, "entry state is not valid"
			}
			reasonText, reason := boundedField(ef.Reason, "entry reason", MaxReasonLen, true)
			if reason != "" {
				return ControlRecord{}, seq, reason
			}
			entries = append(entries, CarriedEntry{ID: id, State: state, Reason: reasonText})
		}
		return ControlRecord{Carried: &Carried{Seq: seq, At: at, Entries: entries}}, seq, ""
	default:
		// Unknown record type: accepted (consumes a seq) but carries nothing
		// a caller can act on.
		return ControlRecord{}, seq, ""
	}
}

// ValidateAckLine checks one line of control-ack.jsonl (without its
// trailing newline) against the protocol, given the last accepted seq in
// this run's control-ack.jsonl. As with ValidateCommandLine, an unknown
// "type" is accepted with every field of AckRecord nil.
func ValidateAckLine(line []byte, lastSeq int) (AckRecord, int, string) {
	w, seq, reason := envelope(line, lastSeq)
	if reason != "" {
		return AckRecord{}, seq, reason
	}
	typ, reason := decodeString(w.Type, "type")
	if reason != "" {
		return AckRecord{}, seq, reason
	}
	at, reason := decodeTime(w.At, "at")
	if reason != "" {
		return AckRecord{}, seq, reason
	}

	switch typ {
	case "hello":
		rawAccepts, reason := requiredArray(w.Accepts, "accepts")
		if reason != "" {
			return AckRecord{}, seq, reason
		}
		var accepts []string
		seen := map[string]bool{}
		for _, raw := range rawAccepts {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return AckRecord{}, seq, "accepts entry is not a string"
			}
			if validKind(s) {
				if seen[s] {
					return AckRecord{}, seq, "accepts is not a set"
				}
				seen[s] = true
				accepts = append(accepts, s)
			}
			// An unknown kind in accepts is ignored, not rejected.
		}
		session, reason := boundedField(w.Session, "session", MaxFieldLen, true)
		if reason != "" {
			return AckRecord{}, seq, reason
		}
		return AckRecord{Hello: &AckHello{Seq: seq, At: at, Accepts: accepts, Session: session}}, seq, ""
	case "session":
		session, reason := boundedField(w.Session, "session", MaxFieldLen, true)
		if reason != "" {
			return AckRecord{}, seq, reason
		}
		return AckRecord{Session: &AckSession{Seq: seq, At: at, Session: session}}, seq, ""
	case "ack":
		id, reason := boundedField(w.ID, "id", MaxFieldLen, true)
		if reason != "" {
			return AckRecord{}, seq, reason
		}
		if strings.TrimSpace(id) == "" {
			return AckRecord{}, seq, "id is empty"
		}
		state, reason := decodeString(w.State, "state")
		if reason != "" {
			return AckRecord{}, seq, reason
		}
		if !validState(state) {
			return AckRecord{}, seq, "state is not valid"
		}
		reasonText, reason := boundedField(w.Reason, "reason", MaxReasonLen, true)
		if reason != "" {
			return AckRecord{}, seq, reason
		}
		return AckRecord{Ack: &AckAck{Seq: seq, At: at, ID: id, State: state, Reason: reasonText}}, seq, ""
	default:
		return AckRecord{}, seq, ""
	}
}

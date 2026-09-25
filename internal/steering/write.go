package steering

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// wireCommand is Command's on-disk shape, matching exactly what
// ValidateCommandLine parses back.
type wireCommand struct {
	V       int    `json:"v"`
	Type    string `json:"type"`
	Seq     int    `json:"seq"`
	ID      string `json:"id"`
	At      string `json:"at"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	ItemID  string `json:"itemId"`
	RunID   string `json:"runId"`
	Session string `json:"session"`
	By      string `json:"by"`
}

// AppendCommand appends one command to control.jsonl at path, deriving seq
// from the file's current line count plus one — the same "no counter file"
// rule agents/_control follows for the ack channel — and returns the seq
// it assigned. cmd.Seq is ignored on the way in.
//
// This is not safe for concurrent callers on its own: two goroutines
// appending to the same path at once can both read the same line count and
// assign the same seq, or interleave their writes. A caller that can be
// invoked concurrently for the same run (the HTTP handler, potentially)
// must serialize its own calls — an item-keyed mutex, not a lock this
// package holds, since only one call site (the engine) is ever this
// channel's writer at all.
func AppendCommand(path string, cmd Command) (int, error) {
	lastSeq, lines, ids, err := inspectControl(path)
	if err != nil {
		return 0, err
	}
	if lines >= MaxCommands {
		return 0, fmt.Errorf("command limit of %d reached", MaxCommands)
	}
	if ids[cmd.ID] {
		return 0, fmt.Errorf("command id %q is not unique", cmd.ID)
	}
	seq := lastSeq + 1
	wire := wireCommand{
		V: 1, Type: "command", Seq: seq,
		ID: cmd.ID, At: cmd.At.UTC().Format(time.RFC3339),
		Kind: cmd.Kind, Text: cmd.Text,
		ItemID: cmd.ItemID, RunID: cmd.RunID, Session: cmd.Session, By: cmd.By,
	}
	line, err := json.Marshal(wire)
	if err != nil {
		return 0, err
	}
	rec, parsedSeq, reason := ValidateCommandLine(line, lastSeq)
	if reason != "" || parsedSeq != seq || rec.Command == nil {
		if reason == "line too long" {
			return 0, errLineTooLong
		}
		return 0, fmt.Errorf("invalid assembled command: %s", reason)
	}
	return seq, appendLine(path, line)
}

var errLineTooLong = &lineTooLongError{}

type lineTooLongError struct{}

func (*lineTooLongError) Error() string { return "assembled command line exceeds MaxLineBytes" }

func inspectControl(path string) (lastSeq, lines int, ids map[string]bool, err error) {
	ids = map[string]bool{}
	if info, statErr := os.Stat(path); statErr == nil && info.Size() > int64(MaxCommands*MaxLineBytes) {
		return 0, 0, nil, fmt.Errorf("control.jsonl exceeds its physical-line bound")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return 0, 0, nil, statErr
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, ids, nil
		}
		return 0, 0, nil, err
	}
	if len(b) == 0 {
		return 0, 0, ids, nil
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		return 0, 0, nil, fmt.Errorf("control.jsonl has an unterminated final line")
	}
	for _, line := range bytes.Split(b[:len(b)-1], []byte("\n")) {
		lines++
		rec, seq, reason := ValidateCommandLine(line, lastSeq)
		if seq > lastSeq {
			lastSeq = seq
		}
		if reason != "" {
			return 0, 0, nil, fmt.Errorf("control.jsonl line %d is invalid: %s", lines, reason)
		}
		if rec.Command != nil {
			if ids[rec.Command.ID] {
				return 0, 0, nil, fmt.Errorf("control.jsonl line %d has duplicate command id %q", lines, rec.Command.ID)
			}
			ids[rec.Command.ID] = true
		}
	}
	return lastSeq, lines, ids, nil
}

func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

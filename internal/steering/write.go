package steering

import (
	"bytes"
	"encoding/json"
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
	seq, err := nextSeq(path)
	if err != nil {
		return 0, err
	}
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
	if len(line)+1 > MaxLineBytes {
		return 0, errLineTooLong
	}
	return seq, appendLine(path, line)
}

var errLineTooLong = &lineTooLongError{}

type lineTooLongError struct{}

func (*lineTooLongError) Error() string { return "assembled command line exceeds MaxLineBytes" }

func nextSeq(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, err
	}
	return bytes.Count(b, []byte("\n")) + 1, nil
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

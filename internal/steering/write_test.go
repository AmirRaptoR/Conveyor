package steering

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendCommandWritesAndReturnsSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	os.WriteFile(path, nil, 0o600)

	seq, err := AppendCommand(path, Command{ID: "c1", At: time.Now(), Kind: KindInstruction, Text: "hi", ItemID: "i1", RunID: "r1", By: "amir"})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("expected seq 1, got %d", seq)
	}

	r := NewCommandReader()
	events := r.Poll(path)
	if len(events) != 1 || !events[0].Accepted || events[0].Record.Command.ID != "c1" {
		t.Fatalf("appended line did not parse back, got %+v", events)
	}
}

func TestAppendCommandSeqIncreasesAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	os.WriteFile(path, nil, 0o600)

	seq1, _ := AppendCommand(path, Command{ID: "c1", At: time.Now(), Kind: KindInstruction, Text: "a", ItemID: "i1", RunID: "r1"})
	seq2, _ := AppendCommand(path, Command{ID: "c2", At: time.Now(), Kind: KindPause, ItemID: "i1", RunID: "r1"})
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("expected seq 1 then 2, got %d then %d", seq1, seq2)
	}
}

func TestAppendCommandCreatesFileIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	// deliberately not pre-created
	seq, err := AppendCommand(path, Command{ID: "c1", At: time.Now(), Kind: KindPause, ItemID: "i1", RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("expected seq 1, got %d", seq)
	}
}

func TestAppendCommandLargestLegalCommandRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	os.WriteFile(path, nil, 0o600)

	field := strings.Repeat("b", MaxFieldLen)
	text := strings.Repeat("a", MaxTextLen)
	seq, err := AppendCommand(path, Command{ID: field, At: time.Now(), Kind: KindInstruction, Text: text, ItemID: field, RunID: field, Session: field, By: field})
	if err != nil {
		t.Fatalf("expected the largest legal command to round-trip, got error: %v", err)
	}
	if seq != 1 {
		t.Fatalf("expected seq 1, got %d", seq)
	}
	r := NewCommandReader()
	events := r.Poll(path)
	if len(events) != 1 || !events[0].Accepted {
		t.Fatalf("largest legal command was rejected on read-back: %+v", events)
	}
}

func TestAppendCommandRefusesAnythingItsReaderWouldReject(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  Command
	}{
		{"missing id", Command{At: time.Now(), Kind: KindInstruction, Text: "x", ItemID: "i", RunID: "r"}},
		{"pause text", Command{ID: "c", At: time.Now(), Kind: KindPause, Text: "x", ItemID: "i", RunID: "r"}},
		{"unknown kind", Command{ID: "c", At: time.Now(), Kind: "other", Text: "x", ItemID: "i", RunID: "r"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "control.jsonl")
			os.WriteFile(path, nil, 0o600)
			if _, err := AppendCommand(path, tc.cmd); err == nil {
				t.Fatal("expected rejection")
			}
			if b, _ := os.ReadFile(path); len(b) != 0 {
				t.Fatalf("invalid command was written: %s", b)
			}
		})
	}
}

func TestAppendCommandRefusesDuplicateIDAndPhysicalLineLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.jsonl")
	os.WriteFile(path, nil, 0o600)
	base := Command{ID: "c0", At: time.Now(), Kind: KindInstruction, Text: "x", ItemID: "i", RunID: "r"}
	if _, err := AppendCommand(path, base); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendCommand(path, base); err == nil {
		t.Fatal("duplicate command id was appended")
	}
	for i := 1; i < MaxCommands; i++ {
		base.ID = fmt.Sprintf("c%d", i)
		if _, err := AppendCommand(path, base); err != nil {
			t.Fatal(err)
		}
	}
	base.ID = "overflow"
	if _, err := AppendCommand(path, base); err == nil {
		t.Fatal("command beyond physical-line cap was appended")
	}
}

package steering

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func cmdLine(seq int, id string) string {
	return fmt.Sprintf(`{"v":1,"type":"command","seq":%d,"id":%q,"at":"2026-09-24T12:00:00Z","kind":"instruction","text":"hi","itemId":"i1","runId":"r1"}`, seq, id)
}

func TestCommandReaderAcceptsAppended(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewCommandReader()

	writeLines(t, path, cmdLine(1, "c1"))
	events := r.Poll(path)
	if len(events) != 1 || !events[0].Accepted || events[0].Record.Command.ID != "c1" {
		t.Fatalf("expected accepted command, got %+v", events)
	}
	if r.Accepted() != 1 {
		t.Fatalf("expected accepted count 1, got %d", r.Accepted())
	}
}

func TestCommandReaderSeqMustIncrease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewCommandReader()

	writeLines(t, path, cmdLine(1, "c1"), cmdLine(1, "c2"))
	events := r.Poll(path)
	if len(events) != 2 || !events[0].Accepted || events[1].Accepted {
		t.Fatalf("expected second (non-increasing seq) rejected, got %+v", events)
	}
}

func TestCommandReaderCapsAt50(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewCommandReader()

	var lines []string
	for i := 1; i <= 60; i++ {
		lines = append(lines, cmdLine(i, fmt.Sprintf("c%d", i)))
	}
	writeLines(t, path, lines...)
	r.Poll(path)
	if r.Accepted() != MaxCommands {
		t.Fatalf("expected accepted capped at %d, got %d", MaxCommands, r.Accepted())
	}
}

func TestCommandReaderUnknownTypeToleratedAndAdvancesSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewCommandReader()

	writeLines(t, path,
		`{"v":1,"type":"future","seq":1,"at":"2026-09-24T12:00:00Z"}`,
		cmdLine(2, "c1"),
	)
	events := r.Poll(path)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %+v", events)
	}
	if !events[0].Accepted || events[0].Record.Command != nil {
		t.Fatalf("expected unknown-type accepted with no command, got %+v", events[0])
	}
	if !events[1].Accepted || events[1].Record.Command.ID != "c1" {
		t.Fatalf("expected the seq-2 command accepted after the unknown record, got %+v", events[1])
	}
}

func TestCommandReaderFinalHandlesTruncatedFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewCommandReader()

	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"v":1,"type":"command",`)
	f.Close()
	r.Poll(path)

	events := r.Final(path)
	if len(events) != 1 || events[0].Accepted || events[0].Reason != "truncated final line" {
		t.Fatalf("expected truncated final line rejection, got %+v", events)
	}
}

func TestCommandReaderStopsOnRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewCommandReader()
	writeLines(t, path, cmdLine(1, "c1"))
	r.Poll(path)

	os.Remove(path)
	events := r.Poll(path)
	found := false
	for _, e := range events {
		if e.Stopped {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a stopped event on removal, got %+v", events)
	}
}

func helloLine(seq int, accepts ...string) string {
	b := strings.Builder{}
	b.WriteString(`{"v":1,"type":"hello","seq":`)
	fmt.Fprintf(&b, "%d", seq)
	b.WriteString(`,"at":"2026-09-24T12:00:00Z","accepts":[`)
	for i, a := range accepts {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q", a)
	}
	b.WriteString(`]}`)
	return b.String()
}

func ackLine(seq int, id, state string) string {
	return fmt.Sprintf(`{"v":1,"type":"ack","seq":%d,"at":"2026-09-24T12:00:00Z","id":%q,"state":%q}`, seq, id, state)
}

func TestAckReaderAcceptsHelloSessionAck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control-ack.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewAckReader()

	writeLines(t, path,
		helloLine(1, "instruction", "pause"),
		`{"v":1,"type":"session","seq":2,"at":"2026-09-24T12:00:00Z","session":"s1"}`,
		ackLine(3, "c1", "consumed"),
	)
	events := r.Poll(path)
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %+v", events)
	}
	if events[0].Record.Hello == nil || events[1].Record.Session == nil || events[2].Record.Ack == nil {
		t.Fatalf("unexpected records: %+v", events)
	}
}

func TestAckReaderCapsAt200(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control-ack.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewAckReader()

	var lines []string
	for i := 1; i <= 250; i++ {
		lines = append(lines, ackLine(i, fmt.Sprintf("c%d", i), "consumed"))
	}
	writeLines(t, path, lines...)
	r.Poll(path)
	if r.Accepted() != MaxAckLines {
		t.Fatalf("expected accepted capped at %d, got %d", MaxAckLines, r.Accepted())
	}
}

func TestAckReaderRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control-ack.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewAckReader()

	writeLines(t, path, "not json", helloLine(1, "instruction"))
	events := r.Poll(path)
	if len(events) != 2 || events[0].Accepted || !events[1].Accepted {
		t.Fatalf("expected [rejected, accepted], got %+v", events)
	}
	if r.Rejected() != 1 {
		t.Fatalf("expected rejected count 1, got %d", r.Rejected())
	}
}

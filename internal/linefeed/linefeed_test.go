package linefeed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path string, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestPollSplitsCompleteLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	os.WriteFile(path, nil, 0o600)
	write(t, path, "one\ntwo\n")

	var got []string
	var c Cursor
	stopped, _ := Poll(&c, path, 1024, func(data []byte, oversize bool) bool {
		if oversize {
			t.Fatalf("unexpected oversize")
		}
		got = append(got, string(data))
		return false
	})
	if stopped {
		t.Fatalf("unexpected stop")
	}
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("got %v", got)
	}
}

func TestPollWaitsForPartialLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	os.WriteFile(path, nil, 0o600)
	write(t, path, "partial")

	var got []string
	var c Cursor
	Poll(&c, path, 1024, func(data []byte, oversize bool) bool {
		got = append(got, string(data))
		return false
	})
	if len(got) != 0 {
		t.Fatalf("expected no lines yet, got %v", got)
	}

	write(t, path, " done\n")
	Poll(&c, path, 1024, func(data []byte, oversize bool) bool {
		got = append(got, string(data))
		return false
	})
	if len(got) != 1 || got[0] != "partial done" {
		t.Fatalf("got %v", got)
	}
}

func TestPollOversizeCompleteLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	os.WriteFile(path, nil, 0o600)
	long := strings.Repeat("a", 20)
	write(t, path, long+"\nshort\n")

	var events []bool // oversize flag per callback
	var lines []string
	var c Cursor
	Poll(&c, path, 10, func(data []byte, oversize bool) bool {
		events = append(events, oversize)
		if !oversize {
			lines = append(lines, string(data))
		}
		return false
	})
	if len(events) != 2 || !events[0] || events[1] {
		t.Fatalf("expected [oversize, ok], got %v", events)
	}
	if len(lines) != 1 || lines[0] != "short" {
		t.Fatalf("got lines %v", lines)
	}
}

func TestPollOversizeGrowingFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	os.WriteFile(path, nil, 0o600)
	var c Cursor

	var oversized bool
	for i := 0; i < 5; i++ {
		write(t, path, "12345")
		Poll(&c, path, 10, func(data []byte, oversize bool) bool {
			if oversize {
				oversized = true
			}
			return false
		})
	}
	if !oversized {
		t.Fatalf("expected the growing fragment to be flagged oversize")
	}

	write(t, path, "\nnext\n")
	var got []string
	Poll(&c, path, 10, func(data []byte, oversize bool) bool {
		if !oversize {
			got = append(got, string(data))
		}
		return false
	})
	if len(got) != 1 || got[0] != "next" {
		t.Fatalf("expected 'next' read after the discarded fragment, got %v", got)
	}
}

func TestPollHaltStopsFurtherReadsThisCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	os.WriteFile(path, nil, 0o600)
	write(t, path, "one\ntwo\nthree\n")

	var got []string
	var c Cursor
	Poll(&c, path, 1024, func(data []byte, oversize bool) bool {
		got = append(got, string(data))
		return len(got) == 1 // halt after the first line
	})
	if len(got) != 1 || got[0] != "one" {
		t.Fatalf("expected halt after first line, got %v", got)
	}
}

func TestPollStopsOnTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	os.WriteFile(path, nil, 0o600)
	write(t, path, "one\n")
	var c Cursor
	Poll(&c, path, 1024, func(data []byte, oversize bool) bool { return false })

	os.WriteFile(path, nil, 0o600)
	stopped, reason := Poll(&c, path, 1024, func(data []byte, oversize bool) bool { return false })
	if !stopped || reason == "" {
		t.Fatalf("expected stopped with a reason, got stopped=%v reason=%q", stopped, reason)
	}
}

func TestPollStopsOnRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	os.WriteFile(path, nil, 0o600)
	write(t, path, "one\n")
	var c Cursor
	Poll(&c, path, 1024, func(data []byte, oversize bool) bool { return false })

	os.Remove(path)
	stopped, reason := Poll(&c, path, 1024, func(data []byte, oversize bool) bool { return false })
	if !stopped || reason == "" {
		t.Fatalf("expected stopped with a reason, got stopped=%v reason=%q", stopped, reason)
	}
}

func TestFinalReportsTruncatedFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	os.WriteFile(path, nil, 0o600)
	write(t, path, "unterminated")
	var c Cursor
	Poll(&c, path, 1024, func(data []byte, oversize bool) bool { return false })

	data, truncated := Final(&c)
	if !truncated || string(data) != "unterminated" {
		t.Fatalf("expected truncated fragment, got data=%q truncated=%v", data, truncated)
	}
	// A second Final call has nothing left to report.
	_, truncated2 := Final(&c)
	if truncated2 {
		t.Fatalf("expected no fragment left on a second Final call")
	}
}

func TestFinalIgnoresSkippingFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	os.WriteFile(path, nil, 0o600)
	write(t, path, "12345678901") // 11 bytes > maxLine 10, no newline
	var c Cursor
	Poll(&c, path, 10, func(data []byte, oversize bool) bool { return false })

	_, truncated := Final(&c)
	if truncated {
		t.Fatalf("expected a discarded oversize fragment to produce no Final rejection")
	}
}

package plan

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

func TestReaderAcceptsAppendedRevisions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewReader()

	writeLines(t, path, `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`)
	events := r.Poll(path)
	if len(events) != 1 || !events[0].Accepted || events[0].Revision.Rev != 1 {
		t.Fatalf("expected one accepted event, got %+v", events)
	}

	writeLines(t, path, `{"v":1,"rev":2,"at":"2026-09-24T12:00:01Z","todos":[]}`)
	events = r.Poll(path)
	if len(events) != 1 || !events[0].Accepted || events[0].Revision.Rev != 2 {
		t.Fatalf("expected second accepted event, got %+v", events)
	}
	if r.Accepted() != 2 || r.Rejected() != 0 {
		t.Fatalf("unexpected counts: accepted=%d rejected=%d", r.Accepted(), r.Rejected())
	}
	last, ok := r.Last()
	if !ok || last.Rev != 2 {
		t.Fatalf("expected last revision 2, got %+v ok=%v", last, ok)
	}
}

func TestReaderRejectsAndKeepsLastAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	writeLines(t, path,
		`{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`,
		`not json`,
		`{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`, // non-increasing
	)
	events := r.Poll(path)
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if !events[0].Accepted {
		t.Fatalf("expected first event accepted")
	}
	if events[1].Accepted || events[1].Reason == "" {
		t.Fatalf("expected second event rejected with a reason, got %+v", events[1])
	}
	if events[2].Accepted || events[2].Reason == "" {
		t.Fatalf("expected third event rejected with a reason, got %+v", events[2])
	}
	if r.Accepted() != 1 || r.Rejected() != 2 {
		t.Fatalf("unexpected counts: accepted=%d rejected=%d", r.Accepted(), r.Rejected())
	}
	last, ok := r.Last()
	if !ok || last.Rev != 1 {
		t.Fatalf("expected last accepted revision to stay 1, got %+v", last)
	}
}

func TestReaderPartialLineWaitsForNextPoll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z",`)
	f.Close()

	events := r.Poll(path)
	if len(events) != 0 {
		t.Fatalf("expected no events for an incomplete line, got %+v", events)
	}

	f, _ = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`"todos":[]}` + "\n")
	f.Close()

	events = r.Poll(path)
	if len(events) != 1 || !events[0].Accepted {
		t.Fatalf("expected the completed line to be accepted, got %+v", events)
	}
}

func TestReaderFinalHandlesTruncatedFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	writeLines(t, path, `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`)
	r.Poll(path)

	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"v":1,"rev":2,`) // never terminated
	f.Close()
	r.Poll(path)

	events := r.Final(path)
	if len(events) != 1 || events[0].Accepted || events[0].Reason != "truncated final line" {
		t.Fatalf("expected one truncated-final-line rejection, got %+v", events)
	}
	if r.Rejected() != 1 {
		t.Fatalf("expected rejected count 1, got %d", r.Rejected())
	}
}

func TestReaderOverlongFragmentDiscardedMidStream(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	long := strings.Repeat("a", MaxLineBytes+10)
	writeLines(t, path,
		fmt.Sprintf(`{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"%s","status":"pending"}]}`, long),
		`{"v":1,"rev":2,"at":"2026-09-24T12:00:01Z","todos":[]}`,
	)
	events := r.Poll(path)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	if events[0].Accepted {
		t.Fatalf("expected the oversized line rejected")
	}
	if !events[1].Accepted || events[1].Revision.Rev != 2 {
		t.Fatalf("expected the next well-formed line accepted, got %+v", events[1])
	}
}

func TestReaderCapsAt1000AcceptedRevisions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	for batch := 0; batch < 2; batch++ {
		var lines []string
		for i := 0; i < 600; i++ {
			rev := batch*600 + i + 1
			lines = append(lines, fmt.Sprintf(`{"v":1,"rev":%d,"at":"2026-09-24T12:00:00Z","todos":[]}`, rev))
		}
		writeLines(t, path, lines...)
		r.Poll(path)
	}
	if r.Accepted() != MaxRevisions {
		t.Fatalf("expected accepted capped at %d, got %d", MaxRevisions, r.Accepted())
	}
}

func TestReaderStopsStreamingAtAcceptedRevisionCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	var content strings.Builder
	for rev := 1; rev <= MaxRevisions; rev++ {
		fmt.Fprintf(&content, `{"v":1,"rev":%d,"at":"2026-09-24T12:00:00Z","todos":[]}`+"\n", rev)
	}
	content.WriteString(strings.Repeat("x", 8*1024*1024))
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewReader()
	r.Poll(path)
	if r.Accepted() != MaxRevisions || !r.capped {
		t.Fatalf("accepted=%d capped=%v, want %d and true", r.Accepted(), r.capped, MaxRevisions)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.offset >= info.Size() {
		t.Fatalf("reader scanned all %d bytes after reaching the cap", info.Size())
	}
	before := r.offset
	if events := r.Poll(path); len(events) != 0 || r.offset != before {
		t.Fatalf("capped reader kept scanning: events=%+v offset=%d want %d", events, r.offset, before)
	}
}

func TestReaderCapsAt1000RejectedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	for batch := 0; batch < 2; batch++ {
		var lines []string
		for i := 0; i < 600; i++ {
			lines = append(lines, "not json")
		}
		writeLines(t, path, lines...)
		r.Poll(path)
	}
	if r.Rejected() != MaxRejectedLines {
		t.Fatalf("expected rejected capped at %d, got %d", MaxRejectedLines, r.Rejected())
	}
	if !r.capped {
		t.Fatalf("expected reader capped after %d rejections", MaxRejectedLines)
	}
}

func TestReaderStopsStreamingAtRejectedLineCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	var content strings.Builder
	for i := 0; i < MaxRejectedLines; i++ {
		content.WriteString("not json\n")
	}
	content.WriteString(strings.Repeat("x", 8*1024*1024))
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewReader()
	r.Poll(path)
	if r.Rejected() != MaxRejectedLines || !r.capped {
		t.Fatalf("rejected=%d capped=%v, want %d and true", r.Rejected(), r.capped, MaxRejectedLines)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.offset >= info.Size() {
		t.Fatalf("reader scanned all %d bytes after reaching the rejected cap", info.Size())
	}
	before := r.offset
	if events := r.Poll(path); len(events) != 0 || r.offset != before {
		t.Fatalf("capped reader kept scanning: events=%+v offset=%d want %d", events, r.offset, before)
	}
}

func TestReaderDiagnosticsCappedAtTenWithSuppressedSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	var lines []string
	for i := 0; i < 15; i++ {
		lines = append(lines, "not json")
	}
	writeLines(t, path, lines...)
	events := r.Poll(path)

	diagnosed := 0
	for _, e := range events {
		if !e.Accepted && e.Diagnose {
			diagnosed++
		}
	}
	if diagnosed != 10 {
		t.Fatalf("expected 10 diagnosed rejections, got %d", diagnosed)
	}

	final := r.Final(path)
	found := false
	for _, e := range final {
		if e.Summary {
			found = true
			if !strings.Contains(e.Reason, "5") {
				t.Fatalf("expected summary to mention 5 suppressed, got %q", e.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("expected a summary event naming suppressed rejections")
	}
}

func TestReaderPendingFragmentDiscardedWhenGrowingAcrossPolls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	// Write an unterminated fragment in small chunks across several polls,
	// crossing MaxLineBytes before any newline ever arrives.
	chunk := strings.Repeat("a", 1024)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"`)
	f.Close()
	var rejected bool
	for i := 0; i < MaxLineBytes/1024+2; i++ {
		f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		f.WriteString(chunk)
		f.Close()
		for _, e := range r.Poll(path) {
			if !e.Accepted && e.Reason == "line too long" {
				rejected = true
			}
		}
	}
	if !rejected {
		t.Fatalf("expected the growing fragment to be discarded as line too long")
	}

	// The next well-formed line, after the fragment's own newline, is still read.
	writeLines(t, path, "", `{"v":1,"rev":2,"at":"2026-09-24T12:00:01Z","todos":[]}`)
	events := r.Poll(path)
	found := false
	for _, e := range events {
		if e.Accepted && e.Revision.Rev == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected rev 2 accepted after the discarded fragment's newline, got %+v", events)
	}
}

func TestReaderStopsOnTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	writeLines(t, path, `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`)
	r.Poll(path)

	// Truncate below the held offset.
	os.WriteFile(path, []byte(""), 0o600)
	events := r.Poll(path)
	stopped := false
	for _, e := range events {
		if e.Stopped {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("expected a stopped event when the file shrinks below the held offset")
	}

	// Further appends are ignored once stopped.
	writeLines(t, path, `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`)
	events = r.Poll(path)
	if len(events) != 0 {
		t.Fatalf("expected no further events once stopped, got %+v", events)
	}
}

func TestReaderStopsOnRemovedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	os.WriteFile(path, nil, 0o600)
	r := NewReader()

	writeLines(t, path, `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`)
	r.Poll(path)

	os.Remove(path)
	events := r.Poll(path)
	stopped := false
	for _, e := range events {
		if e.Stopped {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("expected a stopped event when the file is removed")
	}
}

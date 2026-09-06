package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(t *testing.T, body string, timeout time.Duration) *Result {
	t.Helper()
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, body), Kind: "stage", Workdir: t.TempDir(),
		Timeout: timeout, Source: "test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func TestExitCodesMapToOutcomes(t *testing.T) {
	for _, tc := range []struct {
		body string
		want model.Outcome
	}{
		{"exit 0", model.OutcomeSuccess},
		{"exit 10", model.OutcomeNoop},
		{"exit 20", model.OutcomeBlocked},
		{"exit 1", model.OutcomeFailure},
		{"exit 7", model.OutcomeFailure},
	} {
		if got := run(t, tc.body, time.Minute).Run.Outcome; got != tc.want {
			t.Errorf("%q: got %s want %s", tc.body, got, tc.want)
		}
	}
}

// Logs must never be parsed as data: a script that prints JSON to stdout but
// writes nothing to $CONVEYOR_RESULT has produced no data at all.
func TestStdoutIsNeverData(t *testing.T) {
	res := run(t, `echo '{"pr": 999}'`, time.Minute)
	if res.Data != nil {
		t.Fatalf("stdout leaked into Data: %s", res.Data)
	}
	if len(res.Log) != 1 || !strings.Contains(res.Log[0].Text, "999") {
		t.Fatalf("expected the JSON as a log line, got %+v", res.Log)
	}
}

func TestResultFileIsData(t *testing.T) {
	res := run(t, `echo noise; echo '{"ok":true}' > "$CONVEYOR_RESULT"`, time.Minute)
	if res.Data == nil || !strings.Contains(string(res.Data), "true") {
		t.Fatalf("result file not captured: %s", res.Data)
	}
}

func TestMalformedResultIsIgnoredNotFatal(t *testing.T) {
	res := run(t, `echo 'not json' > "$CONVEYOR_RESULT"; exit 0`, time.Minute)
	if res.Run.Outcome != model.OutcomeSuccess {
		t.Errorf("outcome changed by bad result file: %s", res.Run.Outcome)
	}
	if res.Data != nil {
		t.Errorf("invalid JSON should not be surfaced as data")
	}
}

func TestStderrAndStdoutBothCaptured(t *testing.T) {
	res := run(t, `echo out; echo err >&2`, time.Minute)
	var streams []string
	for _, l := range res.Log {
		streams = append(streams, l.Stream+":"+l.Text)
	}
	joined := strings.Join(streams, ",")
	if !strings.Contains(joined, "stdout:out") || !strings.Contains(joined, "stderr:err") {
		t.Fatalf("missing a stream: %s", joined)
	}
}

// A timeout must kill the whole process group, not just the parent: an AI stage
// script spawns children that would otherwise hold the source's lock.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	res := run(t, `( while true; do sleep 1; done ) & sleep 60`, 500*time.Millisecond)
	if res.Run.Outcome != model.OutcomeTimeout {
		t.Fatalf("got %s want timeout", res.Run.Outcome)
	}
	if !res.Run.TimedOut {
		t.Error("TimedOut not set")
	}
}

// A run must be self-contained: everything needed to debug it, on disk.
func TestRunDirectoryIsSelfContained(t *testing.T) {
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `echo hi; echo '{"a":1}' > "$CONVEYOR_RESULT"`),
		Kind:   "stage", Workdir: t.TempDir(), Timeout: time.Minute, Source: "test",
		Item:  &model.Item{ID: "x:1", Ref: "1"},
		Stdin: map[string]string{"hello": "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"meta.json", "stdin.json", "log.txt", "result.json"} {
		if _, err := os.Stat(filepath.Join(res.Run.Dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(res.Run.Dir, "stdin.json"))
	if !strings.Contains(string(b), "world") {
		t.Error("stdin.json did not record the input")
	}
}

func TestMissingScriptIsAnErrorNotAnOutcome(t *testing.T) {
	r := New(t.TempDir())
	_, err := r.Run(context.Background(), Spec{
		Script: "/nonexistent/nope.sh", Kind: "stage", Workdir: t.TempDir(), Source: "test",
	})
	if err == nil {
		t.Fatal("expected an error when the script cannot be run at all")
	}
}

// An agent sixty turns into a run cannot feel elapsed time, so a duration told
// to it at the start is worth nothing. It is handed the instant it will be
// killed instead, which one `date` verifies.
func TestDeadlineIsExportedAsAnInstant(t *testing.T) {
	res := run(t, `echo "$CONVEYOR_DEADLINE" > "$CONVEYOR_RESULT".deadline`, 30*time.Minute)

	got := res.Run.Env["CONVEYOR_DEADLINE"]
	at, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("CONVEYOR_DEADLINE = %q, want an RFC3339 instant: %v", got, err)
	}
	// The engine's own limit, not a second calculation of it: within a second
	// of thirty minutes out.
	if d := time.Until(at); d < 29*time.Minute+59*time.Second || d > 30*time.Minute {
		t.Errorf("deadline is %s away, want ~30m — it must be the limit the context enforces", d)
	}
	// And the script really received it, not just the record of the run.
	b, err := os.ReadFile(filepath.Join(res.Run.Dir, "result.json.deadline"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != got {
		t.Errorf("script saw %q, run record says %q", strings.TrimSpace(string(b)), got)
	}
}

// A stage with no timeout is a real configuration, and an adapter must be able
// to tell it apart from a deadline that has passed. Empty, never a zero time:
// telling an agent it is out of time would end every run of such a stage on the
// first instruction it read.
func TestNoTimeoutMeansNoDeadline(t *testing.T) {
	res := run(t, `exit 0`, 0)
	if got, ok := res.Run.Env["CONVEYOR_DEADLINE"]; ok {
		t.Errorf("CONVEYOR_DEADLINE = %q for a stage with no timeout, want it unset", got)
	}
}

// A run ID resolved from a request must be rejected before it is ever built
// into a path — traversal, an absolute path, an encoded separator and an
// empty string all fail the shape Run() actually produces.
func TestValidIDRejectsAnythingNotShapedLikeARun(t *testing.T) {
	res := run(t, `exit 0`, time.Minute)
	if !ValidID(res.Run.ID) {
		t.Fatalf("a real run ID %q was rejected", res.Run.ID)
	}
	for _, bad := range []string{
		"", "../etc/passwd", "/etc/passwd", "..%2Fescape",
		"150405.000-abc/../x", "150405.000-" + strings.Repeat("a", 40),
	} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true, want false", bad)
		}
	}
}

// Result.Log has no production consumer (grep-confirmed: only this package's
// tests read it), so bounding it to a tail cannot break a caller. log.txt on
// disk must still carry everything.
func TestResultLogIsBoundedToLast1000Lines(t *testing.T) {
	res := run(t, `for i in $(seq 1 100000); do echo "line $i"; done`, time.Minute)
	if len(res.Log) != 1000 {
		t.Fatalf("len(Log) = %d, want 1000", len(res.Log))
	}
	if res.Log[0].Text != "line 99001" {
		t.Errorf("first kept line = %q, want line 99001 (the tail)", res.Log[0].Text)
	}
	if res.Log[999].Text != "line 100000" {
		t.Errorf("last kept line = %q, want line 100000", res.Log[999].Text)
	}
	b, err := os.ReadFile(filepath.Join(res.Run.Dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 100000 {
		t.Errorf("log.txt has %d lines, want all 100000 on disk", n)
	}
}

// A single line over 64 KiB must not blow up memory or the SSE stream, but
// log.txt is the durable record and gets it whole.
func TestOversizedLineIsCappedInMemoryNotOnDisk(t *testing.T) {
	res := run(t, `head -c 100000 /dev/zero | tr '\0' 'a'; echo`, time.Minute)
	var long *LogLine
	for i := range res.Log {
		if len(res.Log[i].Text) > 0 && res.Log[i].Stream == "stdout" {
			long = &res.Log[i]
		}
	}
	if long == nil {
		t.Fatal("no stdout line captured")
	}
	if len(long.Text) > 64*1024+200 {
		t.Fatalf("in-memory line is %d bytes, want capped near 64 KiB", len(long.Text))
	}
	if !strings.Contains(long.Text, "truncated") {
		t.Errorf("capped line has no truncation marker: %q", long.Text[:80])
	}
	b, err := os.ReadFile(filepath.Join(res.Run.Dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "a"); n < 100000 {
		t.Errorf("log.txt only has %d a's, want the full 100000-byte line on disk", n)
	}
}

// meta.json is written to a temp file and renamed into place, so a run that
// dies mid-write leaves the previous meta.json intact rather than truncated.
func TestMetaJSONWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	run := &model.Run{ID: "r1", Outcome: model.OutcomeRunning}
	if err := writeMeta(run, dir); err != nil {
		t.Fatalf("writeMeta: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "meta.json" {
			t.Errorf("stray file left behind: %s (temp file not cleaned up / renamed)", e.Name())
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got model.Run
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "r1" {
		t.Errorf("ID = %q, want r1", got.ID)
	}
}

// A meta.json write that fails (a full disk) must not look like a clean
// success: the run's error is set and an engine log line names it.
func TestPersistenceFailureIsReportedNotSilentlyDropped(t *testing.T) {
	root := t.TempDir()
	r := New(root)
	res, err := r.Run(context.Background(), Spec{
		// Makes its own run directory read-only before exiting, so the final
		// meta.json write — which happens after the script's own work is
		// done — fails exactly like a full disk would.
		Script:  script(t, `dir=$(dirname "$CONVEYOR_RESULT"); echo working; chmod 500 "$dir"`),
		Kind:    "stage", Workdir: t.TempDir(), Timeout: time.Minute, Source: "test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(res.Run.Dir, 0o755) })
	if res.Run.Error == "" {
		t.Error("Run.Error is empty, want the persistence failure named")
	}
	foundEngineLine := false
	for _, l := range res.Log {
		if l.Stream == "engine" && strings.Contains(l.Text, "meta.json") {
			foundEngineLine = true
		}
	}
	if !foundEngineLine {
		t.Errorf("no engine log line naming the meta.json failure; log = %+v", res.Log)
	}
}

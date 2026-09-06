package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// Cancelling for an ordinary reason — not a deadline — must still reap the
// group before Run returns. A well-behaved parent exits on TERM, which lets
// wg.Wait/cmd.Wait return once a resistant descendant has closed the pipes
// being watched, even though that descendant is still alive; only an
// unconditional sweep on any cancellation catches it.
func TestCancellationReapsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	body := `
trap 'exit 0' TERM
(
	trap '' TERM
	exec >/dev/null 2>&1
	echo $BASHPID > "` + pidFile + `"
	sleep 60
) &
disown
sleep 60
`
	r := New(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		res *Result
		err error
	}
	resCh := make(chan outcome, 1)
	go func() {
		res, err := r.Run(ctx, Spec{
			Script: script(t, body), Kind: "stage", Workdir: t.TempDir(), Source: "test",
		})
		resCh <- outcome{res, err}
	}()

	var pid int
	waitUntil := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitUntil) {
		if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				pid = p
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the TERM-resistant grandchild never recorded its pid")
	}

	cancel()

	var got outcome
	select {
	case got = <-resCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if got.err != nil {
		t.Fatalf("run: %v", got.err)
	}

	if syscall.Kill(pid, 0) == nil {
		t.Fatal("the grandchild is still alive after Run returned")
	}

	if got.res.Run.Outcome != model.OutcomeInterrupted {
		t.Errorf("outcome = %s, want interrupted", got.res.Run.Outcome)
	}
	if got.res.Run.TimedOut {
		t.Error("TimedOut set for a cancellation that was not a deadline")
	}

	// The record on disk is what a restarted engine and the board both read —
	// it has to say the same thing as the in-memory result.
	b, err := os.ReadFile(filepath.Join(got.res.Run.Dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk model.Run
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Outcome != model.OutcomeInterrupted || onDisk.TimedOut {
		t.Errorf("meta.json = {outcome: %s, timedOut: %v}, want {interrupted, false}", onDisk.Outcome, onDisk.TimedOut)
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

// The script itself still sees the real value — cmd.Env is never touched by
// redaction — but what reaches meta.json (and so GET /api/runs) replaces it,
// keeping only the key: a source's env:/params: is exactly where a token
// lives, and CONVEYOR_RESULT proves the process really did see it since the
// script could not otherwise have written a result at all.
func TestRunEnvIsRedactedOnDiskButNotInTheProcess(t *testing.T) {
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Script:  script(t, `echo "{\"saw\":\"$SECRET_TOKEN\"}" > "$CONVEYOR_RESULT"`),
		Kind:    "stage",
		Workdir: t.TempDir(),
		Timeout: time.Minute,
		Source:  "test",
		Env:     map[string]string{"SECRET_TOKEN": "swordfish"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Data), "swordfish") {
		t.Fatalf("the script did not see the real value: result.json = %s", res.Data)
	}

	b, err := os.ReadFile(filepath.Join(res.Run.Dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "swordfish") {
		t.Error("meta.json still contains the secret value")
	}
	if !strings.Contains(string(b), "SECRET_TOKEN") {
		t.Error("meta.json dropped the key entirely; it should keep the name and redact only the value")
	}

	var onDisk model.Run
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Env["SECRET_TOKEN"] != redactedValue {
		t.Errorf("meta.json's SECRET_TOKEN = %q, want the redaction marker", onDisk.Env["SECRET_TOKEN"])
	}
	if !strings.HasPrefix(onDisk.Env["CONVEYOR_SOURCE"], "test") {
		t.Errorf("a CONVEYOR_ key was redacted: CONVEYOR_SOURCE = %q", onDisk.Env["CONVEYOR_SOURCE"])
	}

	// The in-process value is untouched too, not merely the file: the same
	// map the caller can still see keeps the real secret.
	if res.Run.Env["SECRET_TOKEN"] != "swordfish" {
		t.Errorf("res.Run.Env was redacted in-process; want it unaffected, got %q", res.Run.Env["SECRET_TOKEN"])
	}
}

// A run directory holds prompts, a person's typed answer and logs — all
// meant for the operator running conveyor, nobody else on the box.
func TestRunDirectoryAndFilesAreRestrictedToTheOwner(t *testing.T) {
	res := run(t, `echo hi; echo '{"a":1}' > "$CONVEYOR_RESULT"`, time.Minute)

	info, err := os.Stat(res.Run.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("run dir mode = %o, want 0700", perm)
	}
	for _, f := range []string{"meta.json", "stdin.json", "log.txt", "result.json"} {
		fi, err := os.Stat(filepath.Join(res.Run.Dir, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 0600", f, perm)
		}
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

// OnResult reaches every run this Runner executes, which is what lets a
// single subscriber notice an operational fault without threading a check
// through every call site that starts one.
func TestOnResultFiresForEveryRun(t *testing.T) {
	r := New(t.TempDir())
	var got *Result
	r.OnResult = func(res *Result) { got = res }
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `exit 0`), Kind: "stage", Workdir: t.TempDir(), Source: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Run.ID != res.Run.ID {
		t.Fatalf("OnResult did not fire with this run's result")
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
	if res.PersistErr == "" {
		t.Error("PersistErr is empty, want the persistence failure named there too")
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

package runner

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func archivedLog(t *testing.T, dir string) []byte {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(dir, "log.txt")); err == nil {
		return b
	}
	f, err := os.Open(filepath.Join(dir, "log.txt.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

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

func TestPreExecFailureAfterDirectoryCreationReturnsStructuredRun(t *testing.T) {
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{Kind: "stage", Source: "s1", Item: &model.Item{ID: "s1:1"}, Stdin: func() {}})
	if err == nil {
		t.Fatal("unmarshalable stdin did not fail")
	}
	if res == nil || res.Run.ID == "" || res.Run.Dir == "" || res.Run.Outcome != model.OutcomeFailure || res.Run.Error == "" {
		t.Fatalf("result = %+v, want identified structured failure", res)
	}
	var persisted model.Run
	b, readErr := os.ReadFile(filepath.Join(res.Run.Dir, "meta.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if json.Unmarshal(b, &persisted) != nil || persisted.ID != res.Run.ID || persisted.Error == "" {
		t.Fatalf("persisted pre-exec run = %+v", persisted)
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
	// The engine's own limit, not a second calculation of it. Allow two seconds
	// for RFC3339 truncation and race-instrumented process startup.
	if d := time.Until(at); d < 29*time.Minute+58*time.Second || d > 30*time.Minute {
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
	b := archivedLog(t, res.Run.Dir)
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
	b := archivedLog(t, res.Run.Dir)
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

func TestRunDirectoryCreationFailureCleansPartialDirectoryAndReportsPersistenceFault(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "runs"))
	r.mkdirAll = func(path string, mode fs.FileMode) error {
		if strings.Contains(path, string(filepath.Separator)+"runs"+string(filepath.Separator)) {
			if err := os.MkdirAll(path, mode); err != nil {
				return err
			}
			return syscall.ENOSPC
		}
		return os.MkdirAll(path, mode)
	}
	var callback *Result
	r.OnResult = func(res *Result) { callback = res }
	res, err := r.Run(context.Background(), Spec{Script: script(t, `exit 0`), Kind: "stage", Model: true, Workdir: t.TempDir()})
	if err == nil || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Run error = %v, want ENOSPC", err)
	}
	if callback != res || res == nil || !strings.Contains(res.PersistErr, "create run dir") {
		t.Fatalf("OnResult = %+v, result = %+v, want creation persistence fault", callback, res)
	}
	if _, err := os.Stat(res.Run.Dir); !os.IsNotExist(err) {
		t.Fatalf("partial run directory remains: %v", err)
	}
}

func TestScratchDirectoryCreationFailureCleansPartialDirectoriesAndReportsPersistenceFault(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "runs"))
	r.mkdir = func(path string, mode fs.FileMode) error {
		if err := os.Mkdir(path, mode); err != nil {
			return err
		}
		return syscall.ENOSPC
	}
	var callback *Result
	r.OnResult = func(res *Result) { callback = res }
	res, err := r.Run(context.Background(), Spec{Script: script(t, `exit 0`), Kind: "stage", Model: true, Workdir: t.TempDir()})
	if err == nil || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Run error = %v, want ENOSPC", err)
	}
	if callback != res || res == nil || !strings.Contains(res.PersistErr, "create run temporary directory") {
		t.Fatalf("OnResult = %+v, result = %+v, want creation persistence fault", callback, res)
	}
	if _, err := os.Stat(res.Run.Dir); !os.IsNotExist(err) {
		t.Fatalf("run directory remains after scratch creation failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.TempRoot, res.Run.ID)); !os.IsNotExist(err) {
		t.Fatalf("partial scratch directory remains: %v", err)
	}
}

func TestUnchangedDiscoveryPayloadsUseOneCompressedBlob(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	r := New(root)
	body := `printf '%s' '[{"id":"s:1"}]' >"$CONVEYOR_RESULT"`
	for i := 0; i < 2; i++ {
		res, err := r.Run(context.Background(), Spec{Script: script(t, body), Kind: "list", Workdir: t.TempDir(), Source: "s"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(res.Run.Dir, "result.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"payload":"sha256:`) {
			t.Fatalf("result reference = %s", b)
		}
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(root), "payloads"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("payload blobs = %d, want one", len(entries))
	}
}

func TestMalformedLargeDiscoveryResultIsCompressedButRemainsInvalid(t *testing.T) {
	for _, kind := range []string{"list", "status"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "runs")
			r := New(root)
			body := `dd if=/dev/zero bs=1048576 count=2 2>/dev/null | tr '\000' x >"$CONVEYOR_RESULT"`
			res, err := r.Run(context.Background(), Spec{Script: script(t, body), Kind: kind, Workdir: t.TempDir(), Source: "s"})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Data) != 0 {
				t.Fatalf("Data = %d bytes, want malformed result rejected", len(res.Data))
			}
			b, err := os.ReadFile(filepath.Join(res.Run.Dir, "result.json"))
			if err != nil {
				t.Fatal(err)
			}
			var ref map[string]string
			if json.Unmarshal(b, &ref) != nil || !strings.HasPrefix(ref["payload"], "sha256:") {
				t.Fatalf("result reference = %s, want content-addressed payload", b)
			}
			blob := filepath.Join(filepath.Dir(root), "payloads", strings.TrimPrefix(ref["payload"], "sha256:")+".json.gz")
			if info, err := os.Stat(blob); err != nil || info.Size() == 0 {
				t.Fatalf("compressed malformed payload = (%v, %v), want non-empty blob", info, err)
			}
		})
	}
}

func TestPayloadReferenceFailurePreservesResultAndPropagatesPersistErr(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	r := New(root)
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `printf '%s' '[{"id":"s:1"}]' >"$CONVEYOR_RESULT"; chmod 500 "$(dirname "$CONVEYOR_RESULT")"`),
		Kind:   "list", Workdir: t.TempDir(), Source: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(res.Run.Dir, 0o700) })
	if !strings.Contains(res.PersistErr, "persist result reference") {
		t.Fatalf("PersistErr = %q, want payload reference persistence failure", res.PersistErr)
	}
	b, err := os.ReadFile(filepath.Join(res.Run.Dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `[{"id":"s:1"}]` {
		t.Fatalf("result after failed reference write = %s, want original complete payload", b)
	}
}

func TestPayloadSweepFailsClosedOnUnreadableReference(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "runs"))
	runDir := filepath.Join(r.Root, "2026-01-01", "000000.000-run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(runDir, "missing"), filepath.Join(runDir, "result.json")); err != nil {
		t.Fatal(err)
	}
	payloadRoot := filepath.Join(filepath.Dir(r.Root), "payloads")
	if err := os.MkdirAll(payloadRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(payloadRoot, strings.Repeat("a", 64)+".json.gz")
	if err := os.WriteFile(blob, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SweepPayloads(time.Now().Add(time.Hour)); err == nil {
		t.Fatal("SweepPayloads succeeded despite an unreadable result reference")
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("payload was deleted while references were unreadable: %v", err)
	}
}

func TestPayloadPublicationAndSweepAreSerialized(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "runs"))
	dir := filepath.Join(r.Root, "2026-01-01", "000000.000-run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(dir, "result.json")
	if err := os.WriteFile(result, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = r.referencePayload(result, []byte(strconv.Itoa(i)))
		}(i)
		go func() {
			defer wg.Done()
			_, _ = r.SweepPayloads(time.Now().Add(time.Hour))
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(b, &ref); err != nil {
		t.Fatalf("result reference is partial: %s: %v", b, err)
	}
	blob := filepath.Join(filepath.Dir(r.Root), "payloads", strings.TrimPrefix(ref.Payload, "sha256:")+".json.gz")
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("published reference names missing blob: %v", err)
	}
}

func TestRunnerOwnsAndRemovesScriptTemporaryDirectory(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "runs"))
	seen := filepath.Join(t.TempDir(), "tmpdir")
	body := `printf '%s' "$TMPDIR" >"` + seen + `"; touch "$TMPDIR/artifact"`
	if _, err := r.Run(context.Background(), Spec{Script: script(t, body), Kind: "stage", Workdir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(b)); !os.IsNotExist(err) {
		t.Fatalf("scratch directory remains: %v", err)
	}
}

func TestSweepTempRetainsAndReportsInvalidMarkers(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	cases := map[string]string{
		"120000.000-badjson":  "not a lease\n",
		"120000.000-empty":    `{}`,
		"120000.000-mismatch": `{"runId":"120000.000-other","ended":true}`,
		"120000.000-arbitrary": `{"runId":"120000.000-arbitrary","ended":true,
"surprise":"anything"}`,
	}
	for name, contents := range cases {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, ".conveyor-owned")
		if err := os.WriteFile(marker, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(marker, old, old); err != nil {
			t.Fatal(err)
		}
	}
	nonRegular := filepath.Join(root, "120000.000-unreadable")
	if err := os.MkdirAll(filepath.Join(nonRegular, ".conveyor-owned"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(nonRegular, ".conveyor-owned"), old, old); err != nil {
		t.Fatal(err)
	}
	unowned := filepath.Join(root, "unowned")
	if err := os.Mkdir(unowned, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	_, err := SweepTemp(root, time.Now().Add(-24*time.Hour))
	if err == nil {
		t.Fatal("SweepTemp succeeded despite invalid markers")
	}
	for _, want := range []string{"badjson", "empty", "mismatch", "arbitrary", "unreadable", "not a regular file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("SweepTemp error %q does not report %q", err, want)
		}
	}
	for name := range cases {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("invalidly marked directory %s was not retained: %v", name, err)
		}
	}
	if _, err := os.Stat(nonRegular); err != nil {
		t.Errorf("directory with unreadable marker was not retained: %v", err)
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Errorf("unowned dir touched: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("symlink target touched: %v", err)
	}
}

func TestSweepTempReclaimsExpiredRunLeaseEvenWhileServerPIDLives(t *testing.T) {
	root := t.TempDir()
	runID := "120000.000-old"
	dir := filepath.Join(root, runID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, ".conveyor-owned")
	if err := writeTempLease(marker, tempLease{RunID: runID, PID: os.Getpid(), Deadline: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepTemp(root, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expired run lease remained because the server PID is live: %v", err)
	}
}

func TestCompactLogFailurePropagatesToPersistErrAndResultCallback(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "runs"))
	r.compactLog = func(string) error { return syscall.ENOSPC }
	var callback *Result
	r.OnResult = func(res *Result) { callback = res }

	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `printf '%s\n' logged`), Kind: "stage", Workdir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.PersistErr, "compact log.txt") || !strings.Contains(res.PersistErr, syscall.ENOSPC.Error()) {
		t.Fatalf("PersistErr = %q, want injected compaction failure", res.PersistErr)
	}
	if callback != res || !strings.Contains(callback.PersistErr, "compact log.txt") {
		t.Fatalf("OnResult got %+v, want result carrying compaction persistence fault", callback)
	}
}

func TestOnStartCarriesLiveRunIdentity(t *testing.T) {
	r := New(t.TempDir())
	var got model.Run
	r.OnStart = func(run model.Run) { got = run }
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `exit 0`), Kind: "stage", To: "working",
		Workdir: t.TempDir(), Source: "test", Item: &model.Item{ID: "test:1", Ref: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != res.Run.ID || got.ItemID != "test:1" || got.Kind != "stage" || got.To != "working" {
		t.Fatalf("OnStart got %+v, final run %+v", got, res.Run)
	}
}

func TestOnStartDoesNotFireWhenProcessFailsToStart(t *testing.T) {
	r := New(t.TempDir())
	started := false
	result := false
	r.OnStart = func(model.Run) { started = true }
	r.OnResult = func(*Result) { result = true }
	_, err := r.Run(context.Background(), Spec{Script: "/not/here", Kind: "stage", Workdir: t.TempDir()})
	if err == nil {
		t.Fatal("expected start failure")
	}
	if started {
		t.Fatal("OnStart fired for a process that never started")
	}
	if !result {
		t.Fatal("OnResult did not fire for the failed start")
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
		Script: script(t, `dir=$(dirname "$CONVEYOR_RESULT"); echo working; chmod 500 "$dir"`),
		Kind:   "stage", Workdir: t.TempDir(), Timeout: time.Minute, Source: "test",
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

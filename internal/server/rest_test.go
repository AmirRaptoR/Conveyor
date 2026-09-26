package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// deferringPipeline lays down one item already sitting in a stage whose script
// always exits 10 — the shape of `approving` waiting on a pull request that has
// not settled. Every run of it appends a line, and so does every listing, so a
// test can compare the two.
func deferringPipeline(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	lists := filepath.Join(dir, "lists")
	runs := filepath.Join(dir, "stageruns")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
echo x >> `+lists+`
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"working","title":"waiting on the outside world"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\necho x >> "+runs+"\nexit 10\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 250ms
stages:
  - name: working
    script: work
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo
    scripts:
      work:
        script: ./work.sh
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), dir
}

func TestTypedWaitPersistsOnFirstEntryAndRestoresAfterRestart(t *testing.T) {
	cfg, r, dir := deferringPipeline(t)
	runDir := filepath.Join(dir, "first-entry")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "result.json"), []byte(
		`{"waiting":{"class":"poll","key":"draft:7","why":"PR #7 is still a draft"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(cfg, r)
	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Title: "first entry"}
	s.state.Items = []model.Item{item}
	s.applyTransition(&pipeline.Transition{
		Item: item, From: "backlog", Stage: "working",
		Outcome: model.OutcomeNoop, RunDir: runDir,
	})
	entry, ok := s.recovery.Get("item", item.ID)
	if !ok || entry.Class != recoveryPoll || entry.Key != "draft:7" {
		t.Fatalf("first-entry recovery = %+v, present=%v", entry, ok)
	}

	s2 := New(cfg, r)
	s2.mu.RLock()
	resting := s2.resting[item.ID]
	wait := s2.waiting[item.ID]
	s2.mu.RUnlock()
	if !resting || wait.Class != recoveryPoll || wait.Key != "draft:7" {
		t.Fatalf("restart restored resting=%v wait=%+v", resting, wait)
	}
}

// Exit 10 says "leave it, try again next poll" (CONTRACTS §2), and the engine
// answered "try again now": a finished transition wakes the scheduler, the item
// is still workable in the stage it never left, and nothing else can outrank a
// stage that deep — so it was picked again straight away. In production one
// `approving` stage waiting on a quiet pull request re-ran itself every couple
// of seconds, fourteen hundred runs an hour, each a real API call.
//
// The invariant that ends it: a deferred stage runs at most once per listing.
func TestANoOpWaitsForTheNextListing(t *testing.T) {
	cfg, r, dir := deferringPipeline(t)
	lists, stageRuns := filepath.Join(dir, "lists"), filepath.Join(dir, "stageruns")

	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, r)
	s.ctx = ctx
	defer drain(t, s, cancel)
	go s.poll(ctx)
	go s.schedule(ctx)

	waitFor(t, "the deferring stage to run once", func() bool { return countLines(stageRuns) >= 1 })
	// Long enough for a tight loop to file hundreds of runs, and for several
	// listings to land.
	time.Sleep(1500 * time.Millisecond)

	got, listings := countLines(stageRuns), countLines(lists)
	if got > listings {
		t.Errorf("stage ran %d times against %d listings; a no-op must wait for the next one", got, listings)
	}
	// It must still be retried — deferring is not giving up.
	if got < 2 {
		t.Errorf("stage ran %d time(s) across %d listings; it should retry once per listing", got, listings)
	}
}

func TestTypedWaitUsesTransientProbesUntilDiagnosisChanges(t *testing.T) {
	cfg, r, dir := deferringPipeline(t)
	stageRuns := filepath.Join(dir, "stageruns")
	probes := filepath.Join(dir, "probes")
	changed := filepath.Join(dir, "changed")
	rediagnosed := filepath.Join(dir, "rediagnosed")
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
echo x >> `+filepath.Join(dir, "lists")+`
stage=working
if [ -e `+changed+` ]; then stage=done; fi
printf '[{"id":"s1:1","ref":"1","source":"s1","stage":"%s","title":"waiting on the outside world"}]\n' "$stage" > "$CONVEYOR_RESULT"
`)
	writeScript(t, filepath.Join(dir, "work.sh"), `#!/bin/sh
if [ "${CONVEYOR_RECOVERY_PROBE:-}" = 1 ]; then
  echo x >> `+probes+`
  if [ -e `+changed+` ]; then exit 0; fi
  if [ -e `+rediagnosed+` ]; then
    printf '%s\n' '{"waiting":{"class":"poll","key":"checks:7:PENDING","why":"checks are pending"}}' > "$CONVEYOR_RESULT"
    exit 10
  fi
  printf '%s\n' '{"waiting":{"class":"poll","key":"draft:7","why":"PR #7 is still a draft"}}' > "$CONVEYOR_RESULT"
  exit 10
fi
echo x >> `+stageRuns+`
if [ -e `+changed+` ]; then exit 0; fi
if [ -e `+rediagnosed+` ]; then
  printf '%s\n' '{"waiting":{"class":"poll","key":"checks:7:PENDING","why":"checks are pending"}}' > "$CONVEYOR_RESULT"
  exit 10
fi
printf '%s\n' '{"waiting":{"class":"poll","key":"draft:7","why":"PR #7 is still a draft"}}' > "$CONVEYOR_RESULT"
exit 10
`)

	oldSweep, oldBase, oldCap := recoverySweepInterval, recoveryPollBase, recoveryPollCap
	recoverySweepInterval, recoveryPollBase, recoveryPollCap = 10*time.Millisecond, 20*time.Millisecond, 40*time.Millisecond
	defer func() { recoverySweepInterval, recoveryPollBase, recoveryPollCap = oldSweep, oldBase, oldCap }()

	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, r)
	s.ctx = ctx
	defer drain(t, s, cancel)
	go s.poll(ctx)
	go s.schedule(ctx)
	go s.recover(ctx)
	go s.button(ctx, ModeAuto)

	waitFor(t, "the stage to diagnose its deterministic wait", func() bool { return countLines(stageRuns) == 1 })
	waitFor(t, "a transient recovery probe", func() bool { return countLines(probes) >= 1 })
	time.Sleep(150 * time.Millisecond)
	if got := countLines(stageRuns); got != 1 {
		t.Fatalf("unchanged diagnosis created %d stage runs, want exactly the first", got)
	}
	beforeProbes := countLines(probes)
	s.tick <- struct{}{}
	waitFor(t, "the tick to expedite a transient probe", func() bool { return countLines(probes) > beforeProbes })
	if got := countLines(stageRuns); got != 1 {
		t.Fatalf("tick bypassed recovery and created %d stage runs, want exactly the first", got)
	}

	if err := os.WriteFile(rediagnosed, []byte("checks"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "one real stage run after the typed diagnosis changed", func() bool { return countLines(stageRuns) == 2 })
	time.Sleep(100 * time.Millisecond)
	if got := countLines(stageRuns); got != 2 {
		t.Fatalf("changed typed diagnosis created %d stage runs, want exactly two total", got)
	}
	if err := os.Remove(rediagnosed); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(changed, []byte("ready"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "one real stage run after the probe observed readiness", func() bool { return countLines(stageRuns) == 3 })
	time.Sleep(100 * time.Millisecond)
	if got := countLines(stageRuns); got != 3 {
		t.Fatalf("two changed diagnoses created %d stage runs, want exactly three total", got)
	}
	if _, ok := s.recovery.Get("item", "s1:1"); ok {
		t.Fatal("settled item retained its recovery ledger entry")
	}
}

// The tick button means "look again now". A deferral is exactly the kind of
// answer it must not receive.
func TestTheTickButtonClearsADeferral(t *testing.T) {
	cfg, r, dir := deferringPipeline(t)
	stageRuns := filepath.Join(dir, "stageruns")

	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, r)
	s.ctx = ctx
	defer drain(t, s, cancel)
	s.refresh(ctx) // one listing, no poll ticker: nothing else will clear it
	go s.schedule(ctx)
	go s.button(ctx, ModeAuto)

	waitFor(t, "the deferring stage to run once", func() bool { return countLines(stageRuns) >= 1 })
	waitFor(t, "the item to be resting", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.resting["s1:1"]
	})
	before := countLines(stageRuns)

	s.tick <- struct{}{}
	waitFor(t, "the button to run it again", func() bool { return countLines(stageRuns) > before })
}

// F02: the guard a manual start overrides is resting, and only resting — a
// drop out of the backlog is a person asking for another look right now, in
// spirit if not in fact (CLAUDE.md). A plain scheduler wake, by contrast,
// must go on respecting the deferral.
func TestStartOverridesResting(t *testing.T) {
	cfg, r, dir := deferringPipeline(t)
	stageRuns := filepath.Join(dir, "stageruns")

	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, r)
	s.ctx = ctx
	defer drain(t, s, cancel)
	s.refresh(ctx) // one listing, no poll ticker: nothing else will clear it
	go s.schedule(ctx)

	waitFor(t, "the deferring stage to run once", func() bool { return countLines(stageRuns) >= 1 })
	waitFor(t, "the item to be resting", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.resting["s1:1"]
	})
	before := countLines(stageRuns)

	// A plain wake must not retry a resting item.
	s.wakeUp()
	time.Sleep(200 * time.Millisecond)
	if got := countLines(stageRuns); got != before {
		t.Fatalf("stage ran %d time(s) after a plain wake, want %d — resting must hold against it", got, before)
	}

	// A manual start does override it.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:1/start", nil)
	req.SetPathValue("id", "s1:1")
	s.handleStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202 — a manual start must override resting", w.Code, w.Body.String())
	}
	waitFor(t, "the manually started run to happen", func() bool { return countLines(stageRuns) > before })
}

// drain stops the server and waits for the runs it started to finish
// writing. t.TempDir's cleanup runs right after the test returns, and a run
// still filing its record into that directory made it "not empty" — a flake
// that failed a deploy, since the release script runs this suite.
func drain(t *testing.T, s *Server, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	waitFor(t, "runs to drain", func() bool { return len(s.activeList()) == 0 })
	time.Sleep(150 * time.Millisecond) // the poll goroutine's own last listing
}

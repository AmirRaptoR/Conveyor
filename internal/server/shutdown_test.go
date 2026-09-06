package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// latestMeta reads the meta.json of the one run a test expects to find under
// root — pipelineFor's stage runs exactly once per test here.
func latestMeta(t *testing.T, root string) model.Run {
	t.Helper()
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Base(path) == "meta.json" {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatal("no meta.json written under " + root)
	}
	b, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	var run model.Run
	if err := json.Unmarshal(b, &run); err != nil {
		t.Fatal(err)
	}
	return run
}

// resistantPipelineFor is pipelineFor's shape with one difference: its stage
// script ignores SIGTERM, the way a genuinely wedged process would, so
// cancelling the server's context does not end it — only the release file
// does. That is what makes it possible to observe a drain that is still
// waiting, and a drain that gives up.
func resistantPipelineFor(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	lists := filepath.Join(dir, "lists")
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
echo x >> `+lists+`
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"backlog","title":"the only item"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), `#!/bin/sh
trap '' TERM
touch `+started+`
while [ ! -f `+release+` ]; do sleep 0.02; done
`)
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 100ms
stages:
  - name: backlog
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

// blockingListPipelineFor lays down a one-source pipeline whose list script
// blocks until the test releases it, so discovery itself can be held
// mid-flight — the counterpart to pipelineFor holding a stage.
func blockingListPipelineFor(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	started := filepath.Join(dir, "list-started")
	release := filepath.Join(dir, "list-release")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
trap '' TERM
touch `+started+`
while [ ! -f `+release+` ]; do sleep 0.02; done
echo "[]" > "$CONVEYOR_RESULT"
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 100ms
stages:
  - name: backlog
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), dir
}

// Drain must wait for discovery, not just transitions: poll's own list run
// writes under the data directory the same as a stage does, and a restart
// racing it is exactly the half-written record the owner lock exists to
// prevent.
func TestDrainWaitsForDiscoveryNotJustTransitions(t *testing.T) {
	cfg, r, dir := blockingListPipelineFor(t)
	runsRoot := filepath.Join(dir, "runs")

	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, r)
	s.drainGrace = 5 * time.Second

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, "127.0.0.1:0", false) }()

	waitFor(t, "the list script to start", func() bool {
		_, err := os.Stat(filepath.Join(dir, "list-started"))
		return err == nil
	})

	cancel()

	select {
	case err := <-runErr:
		t.Fatalf("Run returned (err=%v) while the list run was still going", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := os.WriteFile(filepath.Join(dir, "list-release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return once the list run finished")
	}

	run := latestMeta(t, runsRoot)
	if run.FinishedAt.IsZero() {
		t.Error("the list run's meta.json has no finishedAt after Run returned")
	}
}

// The end-to-end version of the guarantee: once Run has returned, cmdServe's
// defer releases the owner lock on the spot, so nothing Run started may still
// be writing under the data directory by then. Cancelling ctx before calling
// Run, as TestServingOpenOffLoopbackIsRefused does, is the sharpest version of
// this — the poll goroutine's first list run starts against an
// already-cancelled context, so any goroutine outliving Run's return would be
// racing this test's own RemoveAll.
func TestNothingWritesAfterRunReturns(t *testing.T) {
	cfg, _, _ := pipelineFor(t)
	// Rooted where production actually puts it (cfg.DataDir()/runs), not
	// pipelineFor's own runs/ beside the config — this test's whole point is
	// removing exactly the directory everything here writes under.
	r := runner.New(filepath.Join(cfg.DataDir(), "runs"))
	s := New(cfg, r)
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "s")}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx, ":0", false); err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	dataDir := cfg.DataDir()
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatalf("RemoveAll(%s) right after Run returned: %v — something is still there", dataDir, err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(dataDir); err == nil {
		t.Errorf("%s was recreated within 500ms of Run returning: something Run started outlived it", dataDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", dataDir, err)
	}
}

// Shutdown must stop dispatch, cancel workers, wait for their completion and
// only then let the caller release ownership — cmdServe's defer unlocks the
// data directory the instant Run returns, so returning early hands a fresh
// engine a lock over a worktree the old run might still be writing to.
func TestDrainWaitsForInFlightTransitionToFinish(t *testing.T) {
	cfg, r, dir := pipelineFor(t)
	runsRoot := filepath.Join(dir, "runs")

	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, r)
	s.ctx = ctx
	s.drainGrace = 5 * time.Second
	go s.poll(ctx)
	go s.schedule(ctx)

	waitFor(t, "the stage to start", func() bool {
		_, err := os.Stat(filepath.Join(dir, "started"))
		return err == nil
	})

	// Cancellation reaches the runner too (agents/runner.go), which ends this
	// well-behaved script quickly — so what is being asserted is that drain
	// does not return before that teardown, and everything it writes, is done.
	cancel()
	s.drain()

	if n := s.inFlight.Load(); n != 0 {
		t.Errorf("inFlight = %d after drain returned, want 0", n)
	}

	run := latestMeta(t, runsRoot)
	if run.Outcome != model.OutcomeInterrupted {
		t.Errorf("outcome = %s, want interrupted for a run cancelled by shutdown", run.Outcome)
	}
	if run.FinishedAt.IsZero() {
		t.Error("meta.json has no finishedAt after drain returned")
	}
}

// A worktree, a lease, an agent still writing: none of it should still be
// running once Run has returned. With a resistant script, drain must
// actually wait — not just happen to return after a script that dies fast.
func TestDrainWaitsWhileATransitionResistsCancellation(t *testing.T) {
	cfg, r, dir := resistantPipelineFor(t)

	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, r)
	s.ctx = ctx
	s.drainGrace = 5 * time.Second
	go s.poll(ctx)
	go s.schedule(ctx)

	waitFor(t, "the stage to start", func() bool {
		_, err := os.Stat(filepath.Join(dir, "started"))
		return err == nil
	})
	cancel()

	drained := make(chan struct{})
	go func() { s.drain(); close(drained) }()

	select {
	case <-drained:
		t.Fatal("drain returned while a TERM-resistant transition was still running")
	case <-time.After(300 * time.Millisecond):
	}

	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return once the resistant script was released")
	}
	if n := s.inFlight.Load(); n != 0 {
		t.Errorf("inFlight = %d after drain returned, want 0", n)
	}
}

// In -watch mode schedule launches nothing at all — advance, run off the tick
// button, is the only mover. A drain that only watched inFlight from launch
// and handleStart would return while that transition was still running.
func TestDrainWaitsForAdvanceInWatchMode(t *testing.T) {
	cfg, r, dir := resistantPipelineFor(t)

	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, r)
	s.ctx = ctx
	s.drainGrace = 5 * time.Second
	s.refresh(ctx)
	go s.button(ctx, false)

	s.tick <- struct{}{}
	waitFor(t, "the stage to start", func() bool {
		_, err := os.Stat(filepath.Join(dir, "started"))
		return err == nil
	})

	cancel()

	drained := make(chan struct{})
	go func() { s.drain(); close(drained) }()

	select {
	case <-drained:
		t.Fatal("drain returned while advance's transition was still running")
	case <-time.After(300 * time.Millisecond):
	}

	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return once advance's transition finished")
	}
}

// The end-to-end shape of the same guarantee: Server.Run itself, not just the
// drain() call inside it, must not return while a transition it launched is
// still running.
func TestServerRunDoesNotReturnUntilDrained(t *testing.T) {
	cfg, r, dir := resistantPipelineFor(t)

	ctx, cancel := context.WithCancel(context.Background())
	s := New(cfg, r)
	s.drainGrace = 5 * time.Second

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, "127.0.0.1:0", true) }()

	waitFor(t, "the stage to start", func() bool {
		_, err := os.Stat(filepath.Join(dir, "started"))
		return err == nil
	})

	cancel()

	select {
	case err := <-runErr:
		t.Fatalf("Run returned (err=%v) while the transition was still running", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return once the transition finished")
	}
}

// A drain that outlives its grace must return rather than hang forever on a
// run that will not finish, and say what is still holding it up.
func TestDrainGivesUpAfterItsGraceAndNamesWhatIsStillRunning(t *testing.T) {
	cfg, r, dir := resistantPipelineFor(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(cfg, r)
	s.ctx = ctx
	s.drainGrace = 150 * time.Millisecond
	go s.poll(ctx)
	go s.schedule(ctx)

	waitFor(t, "the stage to start", func() bool {
		_, err := os.Stat(filepath.Join(dir, "started"))
		return err == nil
	})
	cancel()

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = stderrW

	done := make(chan struct{})
	start := time.Now()
	go func() { s.drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		os.Stderr = old
		t.Fatal("drain did not give up after its grace")
	}
	elapsed := time.Since(start)

	os.Stderr = old
	stderrW.Close()
	buf := make([]byte, 4096)
	n, _ := stderrR.Read(buf)
	msg := string(buf[:n])

	if elapsed > time.Second {
		t.Errorf("drain took %s to give up on a 150ms grace", elapsed)
	}
	if !strings.Contains(msg, "s1:1") {
		t.Errorf("stderr = %q, want it to name the item still running (s1:1)", msg)
	}

	// Clean up the still-running stage so the test process does not leak it.
	_ = os.WriteFile(filepath.Join(dir, "release"), nil, 0o644)
	waitFor(t, "the leftover stage to finish", func() bool { return s.inFlight.Load() == 0 })
}

// None of the three paths that claim an item may do so once the engine is
// shutting down: a transition started after cancellation would outlive the
// engine that launched it, same as the one drain exists to wait for.
func TestNoTransitionClaimsAnItemAfterCancellation(t *testing.T) {
	cfg, r, _ := pipelineFor(t)

	s := New(cfg, r)
	s.refresh(context.Background())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	s.ctx = cancelled

	if n := s.launch(cancelled); n != 0 {
		t.Errorf("launch after cancellation started %d transition(s)", n)
	}
	if s.advance(cancelled) {
		t.Error("advance after cancellation started a transition")
	}
	if n := s.inFlight.Load(); n != 0 {
		t.Errorf("inFlight = %d, want 0 — nothing should have launched", n)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:1/start", nil)
	req.SetPathValue("id", "s1:1")
	s.handleStart(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("handleStart after cancellation = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}

	w2 := httptest.NewRecorder()
	s.handleTick(w2, httptest.NewRequest("POST", "/api/tick", nil))
	if w2.Code != http.StatusServiceUnavailable {
		t.Errorf("handleTick after cancellation = %d, want %d", w2.Code, http.StatusServiceUnavailable)
	}
}

// A listen error is a return path too: Run must not leave its loops running
// on the caller's context, which the caller has no reason to cancel because
// Run itself failed. Binding the address first is what forces ListenAndServe
// to fail synchronously, before any loop has a chance to do real work.
func TestErrorPathDrainsToo(t *testing.T) {
	cfg, r, _ := pipelineFor(t)

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	addr := taken.Addr().String()

	s := New(cfg, r)
	s.drainGrace = 5 * time.Second

	if err := s.Run(context.Background(), addr, false); err == nil {
		t.Fatal("Run on an address already in use returned no error")
	}

	if n := s.shutdownWork.Load(); n != 0 {
		t.Errorf("shutdownWork = %d once Run's listen error returned, want 0 — a loop is still running", n)
	}
	if n := s.inFlight.Load(); n != 0 {
		t.Errorf("inFlight = %d once Run's listen error returned, want 0", n)
	}
}

// The request-tracking wrapper is its own function so a test can drive it
// directly, the same idiom as webHandler and authed: order.json, answers.json
// and push.json are all written synchronously inside a handler, so drain must
// wait for the handler's own lifetime, not just for a transition it launched.
func TestTrackedWaitsForInFlightRequests(t *testing.T) {
	cfg, r, _ := pipelineFor(t)
	s := New(cfg, r)
	s.drainGrace = 5 * time.Second

	started := make(chan struct{})
	release := make(chan struct{})
	h := s.tracked(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	<-started

	drained := make(chan struct{})
	go func() { s.drain(); close(drained) }()

	select {
	case <-drained:
		t.Fatal("drain returned while the handler was still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)

	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not return once the handler finished")
	}
}

// The give-up message must not name a transition that does not exist: with
// drainGrace short and the only outstanding work a non-transition worker
// (spawn, not a launched transition), the message can only say how much work
// is outstanding, never "still running: " followed by an empty list.
func TestGiveUpMessageNamesNonTransitionWorkHonestly(t *testing.T) {
	cfg, r, _ := pipelineFor(t)
	s := New(cfg, r)
	s.drainGrace = 100 * time.Millisecond

	block := make(chan struct{})
	defer close(block)
	s.spawn(func() { <-block })

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = stderrW

	done := make(chan struct{})
	go func() { s.drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		os.Stderr = old
		t.Fatal("drain did not give up after its grace")
	}

	os.Stderr = old
	stderrW.Close()
	buf := make([]byte, 4096)
	n, _ := stderrR.Read(buf)
	msg := string(buf[:n])

	if strings.Contains(msg, "transition(s)") {
		t.Errorf("stderr = %q names a transition, but none were outstanding", msg)
	}
	if !strings.Contains(msg, "task(s) still running") {
		t.Errorf("stderr = %q, want it to say how much non-transition work is outstanding", msg)
	}
}

package server

import (
	"context"
	"encoding/json"
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

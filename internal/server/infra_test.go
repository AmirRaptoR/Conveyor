package server

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// failsToEnterWorking builds a pipeline whose move script refuses only the
// move into "working" — the initial move Advance makes before a stage script
// ever runs — and succeeds for everything else, so the failure under test is
// isolated to that one write.
func failsToEnterWorking(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	attempts := filepath.Join(dir, "move-attempts")
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"backlog","title":"the only item"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), `#!/bin/sh
body=$(cat)
case "$body" in
  *'"stage": "working"'*)
    echo x >> `+attempts+`
    exit 1
    ;;
  *) exit 0 ;;
esac
`)
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
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

// F04: an initial provider move that fails must preserve the answer it read
// (nothing ran to spend it), and must not be retried on every scheduler wake
// — the item is deferred until the next listing, the same mechanism a
// deferring stage already uses, so a failing provider is not hammered once
// per poll interval instead of once per listing.
func TestInitialMoveFailurePreservesTheAnswerAndDefersToTheNextListing(t *testing.T) {
	cfg, r, dir := failsToEnterWorking(t)
	attempts := filepath.Join(dir, "move-attempts")
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}}
	if err := s.answers.Set("s1:1", model.Resume{Answer: "keep me", Session: "sess-1"}); err != nil {
		t.Fatal(err)
	}

	n := s.launch(s.ctx)
	if n != 1 {
		t.Fatalf("launch dispatched %d transition(s), want 1", n)
	}
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })

	if got := s.answers.Get("s1:1"); got.Answer != "keep me" {
		t.Errorf("answer = %+v after a failed initial move, want it preserved", got)
	}
	s.mu.RLock()
	stage := s.state.Items[0].Stage
	resting := s.resting["s1:1"]
	terr, hasErr := s.transitionErrs["s1:1"]
	s.mu.RUnlock()
	if stage != "backlog" {
		t.Errorf("stage = %q, want backlog — nothing ran, so nothing should have moved", stage)
	}
	if !resting {
		t.Error("the item was not deferred after a failed initial move")
	}
	if !hasErr || terr.Reason == "" {
		t.Error("no transition error was recorded for the board")
	}

	// A second scheduler wake, with no listing in between, must not retry it.
	if got := countLines(attempts); got != 1 {
		t.Fatalf("move was attempted %d time(s) before a second wake, want 1", got)
	}
	if n := s.launch(s.ctx); n != 0 {
		t.Fatalf("a second wake dispatched %d transition(s), want 0 — deferred until the next listing", n)
	}
	if got := countLines(attempts); got != 1 {
		t.Errorf("move was attempted %d time(s) across two wakes with no listing, want 1", got)
	}

	// The next listing clears the deferral (and, since the fixture always
	// reports the item in backlog, restates the same state) and survives:
	// the transition error must still be visible even though refresh
	// replaces State.Warnings wholesale on every poll.
	s.refresh(s.ctx)
	s.mu.RLock()
	_, stillThere := s.transitionErrs["s1:1"]
	s.mu.RUnlock()
	if !stillThere {
		t.Error("the transition error did not survive a listing that did not touch this item")
	}
	if n := s.launch(s.ctx); n != 1 {
		t.Fatalf("after the next listing, launch dispatched %d transition(s), want 1 (one retry per listing)", n)
	}
	waitFor(t, "the retried transition to finish", func() bool { return s.inFlight.Load() == 0 })
	if got := countLines(attempts); got != 2 {
		t.Errorf("move was attempted %d time(s) total, want exactly 2 (one per listing)", got)
	}
}

// The board must be able to say what a transition error is, and it must
// survive a subsequent successful listing rather than vanishing behind
// State.Warnings being replaced wholesale.
func TestTransitionErrorsSurviveAListing(t *testing.T) {
	cfg, r, _ := failsToEnterWorking(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}}

	s.launch(s.ctx)
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })
	s.refresh(s.ctx)

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	if !strings.Contains(w.Body.String(), `"s1:1"`) || !strings.Contains(w.Body.String(), "transitionErrors") {
		t.Errorf("/api/state does not carry s1:1's transition error: %s", w.Body.String())
	}
}

// F04: Advance returns a nil transition for an item whose source is not one
// the engine knows about — a config problem, not a transient one. runOne
// must not drop that error silently: it belongs on the board the same way
// any other transition error does, and the claim and slot it took must come
// back regardless.
func TestUnknownSourceIsSurfacedAndReleasesItsClaim(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	// "ghost" names no configured source at all — the shape of an item whose
	// source was removed from the config, or a stale id from elsewhere.
	s.state.Items = []model.Item{{ID: "ghost:1", Ref: "1", Source: "ghost", Stage: "backlog"}}

	n := s.launch(s.ctx)
	if n != 1 {
		t.Fatalf("launch dispatched %d transition(s), want 1", n)
	}
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })

	s.mu.RLock()
	terr, hasErr := s.transitionErrs["ghost:1"]
	resting := s.resting["ghost:1"]
	s.mu.RUnlock()
	if !hasErr || terr.Reason == "" {
		t.Fatal("no transition error was recorded for an item naming an unknown source")
	}
	if !strings.Contains(terr.Reason, "ghost") {
		t.Errorf("reason = %q, want it to name the unknown source", terr.Reason)
	}
	if !resting {
		t.Error("the item was not deferred after an unknown-source error")
	}

	if _, working := s.working.Load("ghost:1"); working {
		t.Error("working still holds the item after its claim should have been released")
	}
	bySrc, byStage, held, _, _, _ := s.eng.Locks().Snapshot()
	if len(bySrc) != 0 || len(byStage) != 0 || held != 0 {
		t.Errorf("locks not fully released: bySource=%v byStage=%v held=%d", bySrc, byStage, held)
	}
}

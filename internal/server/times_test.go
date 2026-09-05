package server

import (
	"encoding/json"
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

// A transition that lands an item somewhere it was not starts a fresh clock.
func TestApplyTransitionRecordsArrivalTime(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	before := time.Now()
	tr := &pipeline.Transition{
		Item:    model.Item{ID: "s1:1", Source: "s1", Stage: "working"},
		From:    "backlog",
		Stage:   "working",
		Outcome: model.OutcomeSuccess,
	}
	s.applyTransition(tr)

	got, ok := s.times["s1:1"]
	if !ok {
		t.Fatal("no time entry written for an item that arrived somewhere new")
	}
	if got.Stage != "working" {
		t.Errorf("stage = %q, want working", got.Stage)
	}
	if got.EnteredStage.Before(before) {
		t.Errorf("enteredStage = %v, want at or after %v", got.EnteredStage, before)
	}
}

// A re-run, a confirmation, a mark: the item did not arrive anywhere, so its
// clock does not restart.
func TestApplyTransitionLeavesTimeUntouchedWhenTheItemDidNotMove(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	old := time.Now().Add(-time.Hour)
	s.times["s1:1"] = ItemTime{Stage: "working", EnteredStage: old}

	tr := &pipeline.Transition{
		Item:    model.Item{ID: "s1:1", Source: "s1", Stage: "working"},
		From:    "working",
		Stage:   "working",
		Outcome: model.OutcomeNoop,
	}
	s.applyTransition(tr)

	got := s.times["s1:1"]
	if !got.EnteredStage.Equal(old) {
		t.Errorf("enteredStage = %v, want untouched at %v", got.EnteredStage, old)
	}
}

// /api/state hands the times map out, keyed by item id.
func TestStateReturnsTimes(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}}
	entered := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.times["s1:1"] = ItemTime{Stage: "working", EnteredStage: entered}

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))

	var got State
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	tm, ok := got.Times["s1:1"]
	if !ok {
		t.Fatal("times has no entry for s1:1")
	}
	if tm.Stage != "working" || !tm.EnteredStage.Equal(entered) {
		t.Errorf("times[s1:1] = %+v, want {working %v}", tm, entered)
	}
}

// A missing or stale entry is filled from the most recent successful move
// into the item's current stage, on a poll.
func TestTimeIsRecoveredFromRunHistory(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	day := filepath.Join(r.Root, "2026-08-28", "120000.000-bbbb")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"120000.000-bbbb","source":"s1","itemId":"s1:1","kind":"move",
	          "from":"backlog","to":"working","outcome":"success","finishedAt":"2026-08-28T12:00:05Z"}`
	if err := os.WriteFile(filepath.Join(day, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}})

	got, ok := s.times["s1:1"]
	if !ok {
		t.Fatal("no time entry recovered")
	}
	if got.Stage != "working" {
		t.Errorf("stage = %q, want working", got.Stage)
	}
	want, _ := time.Parse(time.RFC3339, "2026-08-28T12:00:05Z")
	if !got.EnteredStage.Equal(want) {
		t.Errorf("enteredStage = %v, want %v", got.EnteredStage, want)
	}
}

// A stale entry — the item has moved on since — is refilled, not trusted.
func TestStaleTimeEntryIsRefilledFromRunHistory(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.times["s1:1"] = ItemTime{Stage: "backlog", EnteredStage: time.Now().Add(-time.Hour)}
	day := filepath.Join(r.Root, "2026-08-28", "120000.000-cccc")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"120000.000-cccc","source":"s1","itemId":"s1:1","kind":"move",
	          "from":"backlog","to":"working","outcome":"success","finishedAt":"2026-08-28T12:05:00Z"}`
	if err := os.WriteFile(filepath.Join(day, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}})

	if got := s.times["s1:1"].Stage; got != "working" {
		t.Errorf("stage = %q, want working — the item has moved on since the stale entry", got)
	}
}

// No successful move into the current stage survives: no entry is invented.
func TestNoTimeEntryWhenNoMoveSurvivesInHistory(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.recallBlocks([]model.Item{{ID: "s1:9", Source: "s1", Stage: "working"}})
	if _, ok := s.times["s1:9"]; ok {
		t.Error("a time entry was invented with no run history behind it")
	}
}

// An item that has left the board — done and rolled off, or no longer
// listed — has its entry dropped on the next poll, the same pruning Blocks
// gets.
func TestPruningDropsTimeEntryWhenItemLeavesTheBoard(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.times["s1:1"] = ItemTime{Stage: "working", EnteredStage: time.Now()}
	s.refresh(t.Context())
	if _, ok := s.times["s1:1"]; ok {
		t.Error("a time entry survived for an item no longer on the board")
	}
}

// While a stage script is running, its Active entry carries a non-zero
// startedAt.
func TestActiveCarriesStartedAtWhileRunning(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\nsleep 0.3\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
stages:
  - name: backlog
    onSuccess: working
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
	r := runner.New(filepath.Join(dir, "runs"))

	s := New(cfg, r)
	s.ctx = t.Context()
	before := time.Now()
	if !s.eng.Locks().TryAcquire("s1", "working") {
		t.Fatal("could not take the lock the test needs")
	}
	s.working.Store("s1:1", struct{}{})
	s.inFlight.Add(1)
	go s.transition(t.Context(), model.Item{ID: "s1:1", Source: "s1", Stage: "backlog"}, "working")

	waitFor(t, "the run to appear active", func() bool { return len(s.activeList()) > 0 })
	got := s.activeList()[0]
	if got.StartedAt.IsZero() {
		t.Fatal("startedAt is zero while the stage script is running")
	}
	if got.StartedAt.Before(before) {
		t.Errorf("startedAt = %v, want at or after %v", got.StartedAt, before)
	}
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })
}

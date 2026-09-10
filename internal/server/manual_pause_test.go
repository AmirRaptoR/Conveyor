package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// twoSourcesFor lays down two independent sources, s1 and s2, whose "working"
// stage blocks in flight until the test releases it — so an item can be held
// mid-run while a pause is applied around it.
func twoSourcesFor(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), `#!/bin/sh
touch "$STARTED"
while [ ! -f "$RELEASE" ]; do sleep 0.02; done
`)
	for _, s := range []string{"repo1", "repo2"} {
		if err := os.MkdirAll(filepath.Join(dir, s), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 100ms
concurrency:
  perSource: 2
  perStage: 2
  global: 4
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
    workdir: ./repo1
    scripts:
      work:
        script: ./work.sh
        params:
          STARTED: `+filepath.Join(dir, "started1")+`
          RELEASE: `+filepath.Join(dir, "release1")+`
  - name: s2
    provider: fake
    workdir: ./repo2
    scripts:
      work:
        script: ./work.sh
        params:
          STARTED: `+filepath.Join(dir, "started2")+`
          RELEASE: `+filepath.Join(dir, "release2")+`
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), dir
}

// A pause on one source must not stop dispatch to a different one — and must
// never touch a run already in flight, whichever source it belongs to.
func TestManualPauseBlocksNewStartsButNotActiveWork(t *testing.T) {
	cfg, r, dir := twoSourcesFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{
		{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "on s1"},
		{ID: "s2:1", Ref: "1", Source: "s2", Stage: "backlog", Title: "on s2"},
	}

	// s1 already has a run in flight before anything is paused.
	if n := s.launch(s.ctx); n != 2 {
		t.Fatalf("launch() started %d, want 2 (nothing paused yet)", n)
	}
	waitFor(t, "s1's run to start", func() bool { _, err := os.Stat(filepath.Join(dir, "started1")); return err == nil })
	waitFor(t, "s2's run to start", func() bool { _, err := os.Stat(filepath.Join(dir, "started2")); return err == nil })

	if err := s.pauseManual("s1", "operator freeze", "amir"); err != nil {
		t.Fatal(err)
	}

	// The active run on s1 is untouched: no release file yet, so if it had
	// been killed the transition would already have returned.
	time.Sleep(50 * time.Millisecond)
	if _, ok := s.active.Load("s1:1"); !ok {
		t.Error("the paused source's already-running item was affected by the pause")
	}

	// A fresh item on the paused source must not be picked up by any
	// dispatch path: not the scheduler,
	s.state.Items = append(s.state.Items, model.Item{ID: "s1:2", Ref: "2", Source: "s1", Stage: "backlog", Title: "new work"})
	if n := s.launch(s.ctx); n != 0 {
		t.Errorf("launch() started %d item(s) while s1 was paused, want 0", n)
	}
	// nor the manual-start endpoint,
	if got := s.claim(model.Item{ID: "s1:2", Ref: "2", Source: "s1", Stage: "backlog"}, "working", false); got != claimManuallyPaused {
		t.Errorf("claim() = %v, want claimManuallyPaused", got)
	}
	// nor the tick button, which is never allowed to override an operator's
	// own pause the way it overrides a paused agent's quota.
	if got := s.claim(model.Item{ID: "s1:2", Ref: "2", Source: "s1", Stage: "backlog"}, "working", true); got != claimManuallyPaused {
		t.Errorf("claim() with overridePause=true = %v, want claimManuallyPaused (a manual pause is never overridden)", got)
	}

	// Let both runs finish so the test does not leak goroutines.
	if err := os.WriteFile(filepath.Join(dir, "release1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release2"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both runs to finish", func() bool { return s.inFlight.Load() == 0 })
}

// A global pause holds every source back; resuming restores exactly the
// scheduler's normal gates, never bypassing a busy slot or a still-marked item.
func TestGlobalPauseAndResumeGoThroughTheSameGates(t *testing.T) {
	cfg, r, dir := twoSourcesFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{
		{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"},
		{ID: "s2:1", Ref: "1", Source: "s2", Stage: "backlog"},
	}

	if err := s.pauseManual("", "board-wide freeze", "amir"); err != nil {
		t.Fatal(err)
	}
	if n := s.launch(s.ctx); n != 0 {
		t.Fatalf("launch() started %d under a global pause, want 0", n)
	}

	if err := s.resumeManual(""); err != nil {
		t.Fatal(err)
	}
	if n := s.launch(s.ctx); n != 2 {
		t.Fatalf("launch() started %d after resume, want 2", n)
	}
	waitFor(t, "both to start", func() bool {
		_, err1 := os.Stat(filepath.Join(dir, "started1"))
		_, err2 := os.Stat(filepath.Join(dir, "started2"))
		return err1 == nil && err2 == nil
	})

	// Resuming must not bypass an already-busy slot: launching again finds
	// both items already working and starts nothing more.
	if n := s.launch(s.ctx); n != 0 {
		t.Errorf("launch() started %d more once both slots were already busy, want 0", n)
	}

	_ = os.WriteFile(filepath.Join(dir, "release1"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "release2"), nil, 0o644)
	waitFor(t, "both runs to finish", func() bool { return s.inFlight.Load() == 0 })
}

// Unlike an agent's own quota pause, a manual pause is a person's decision and
// must survive a restart — the whole point of persisting it to disk.
func TestManualPauseSurvivesARestart(t *testing.T) {
	cfg, r, _ := twoSourcesFor(t)
	s1 := New(cfg, r)
	s1.ctx = context.Background()
	if err := s1.pauseManual("s2", "flaky upstream", "amir"); err != nil {
		t.Fatal(err)
	}

	s2 := New(cfg, r) // a fresh process, same data dir
	s2.ctx = context.Background()
	if !s2.manuallyPaused("s2") {
		t.Error("a manual pause did not survive a restart")
	}
	if s2.manuallyPaused("s1") {
		t.Error("a pause scoped to s2 leaked onto s1 after a restart")
	}
}

// The HTTP surface: scope validation, a required reason, and the audit trail
// (who asked, and why) that makes a pause traceable rather than a mystery.
func TestPauseAndResumeAPI(t *testing.T) {
	cfg, r, _ := twoSourcesFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.mode = ModeAuto

	post := func(path string, body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.SetBasicAuth("amir", "whatever") // parsed regardless of s.verify, since these handlers are called directly
		w := httptest.NewRecorder()
		switch path {
		case "/api/pause":
			s.handlePause(w, req)
		case "/api/resume":
			s.handleResume(w, req)
		}
		return w
	}

	if w := post("/api/pause", map[string]any{"scope": "", "reason": ""}); w.Code != http.StatusBadRequest {
		t.Errorf("pause with no reason = %d, want 400", w.Code)
	}
	if w := post("/api/pause", map[string]any{"scope": "no-such-source", "reason": "x"}); w.Code != http.StatusBadRequest {
		t.Errorf("pause on an unknown source = %d, want 400", w.Code)
	}
	if w := post("/api/pause", map[string]any{"scope": "s1", "reason": "operator freeze"}); w.Code != http.StatusNoContent {
		t.Fatalf("pause on s1 = %d, want 204: %s", w.Code, w.Body)
	}

	mp, ok := s.manualPauses.Get("s1")
	if !ok {
		t.Fatal("no pause recorded for s1")
	}
	if mp.Reason != "operator freeze" || mp.By != "amir" {
		t.Errorf("recorded pause = %+v, want reason %q by %q", mp, "operator freeze", "amir")
	}

	if w := post("/api/resume", map[string]any{"scope": "s1"}); w.Code != http.StatusNoContent {
		t.Errorf("resume on s1 = %d, want 204", w.Code)
	}
	if _, ok := s.manualPauses.Get("s1"); ok {
		t.Error("s1 still paused after resume")
	}
	// Resuming a scope that was never paused is not an error.
	if w := post("/api/resume", map[string]any{"scope": "s2"}); w.Code != http.StatusNoContent {
		t.Errorf("resume on an unpaused scope = %d, want 204", w.Code)
	}
}

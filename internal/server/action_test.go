package server

import (
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

// boardWithAction is boardFor with one stage that declares a manual action.
func boardWithAction(t *testing.T) (*config.Config, *runner.Runner) {
	t.Helper()
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\nexit 0\n")
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
    actions:
      - name: merge-now
        label: Merge now
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
	return cfg, runner.New(filepath.Join(dir, "runs"))
}

func postAction(t *testing.T, s *Server, id, action string) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader(`{"action":"` + action + `"}`)
	r := httptest.NewRequest("POST", "/api/items/"+id+"/action", body)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.handleAction(w, r)
	return w
}

// Pressing an action arms one word for the next run of that stage, and clears
// the item's deferral — a stage that exited 10 to wait is exactly the one this
// exists to hurry, and leaving it resting would mean the button did nothing
// until the next listing.
func TestAnActionIsArmedForTheNextRunAndEndsTheWait(t *testing.T) {
	cfg, r := boardWithAction(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}}
	s.resting["s1:1"] = true
	s.restingAt["s1:1"] = time.Now()
	s.waiting["s1:1"] = model.Waiting{Why: "quiet period"}

	if w := postAction(t, s, "s1:1", "merge-now"); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d (%s), want 204", w.Code, w.Body.String())
	}
	if got := s.answers.Get("s1:1").Manual; got != "merge-now" {
		t.Errorf("armed action = %q, want merge-now", got)
	}
	if s.resting["s1:1"] {
		t.Error("the item is still resting; the button would do nothing until the next listing")
	}
	if _, ok := s.waiting["s1:1"]; ok {
		t.Error("the countdown outlived the wait it was counting down to")
	}
}

// Only what the stage declares. Anything else is a stale page or a
// hand-written request, and either way the script would not know the word.
func TestAnUndeclaredActionIsRefused(t *testing.T) {
	cfg, r := boardWithAction(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}}
	if w := postAction(t, s, "s1:1", "deploy-to-prod"); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if got := s.answers.Get("s1:1").Manual; got != "" {
		t.Errorf("armed %q; an undeclared action must arm nothing", got)
	}
}

// An action declared by some other stage is not an action for this item: the
// word only ever reaches the stage the item is actually in.
func TestAnActionFromAnotherStageIsRefused(t *testing.T) {
	cfg, r := boardWithAction(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog"}}
	if w := postAction(t, s, "s1:1", "merge-now"); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// An answer somebody typed and an action they then pressed are two things said
// about the same stop. Neither gesture may quietly discard the other.
func TestAnActionAndAnAnswerBothSurvive(t *testing.T) {
	cfg, r := boardWithAction(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}}
	if err := s.answers.Set("s1:1", model.Resume{Answer: "use the second option", Session: "abc"}); err != nil {
		t.Fatal(err)
	}
	if w := postAction(t, s, "s1:1", "merge-now"); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	got := s.answers.Get("s1:1")
	if got.Manual != "merge-now" || got.Answer != "use the second option" || got.Session != "abc" {
		t.Errorf("resume = %+v, want the answer, the session and the action together", got)
	}
}

// The stage's actions reach the board, because the board offers exactly what
// the config declares and invents nothing.
func TestActionsAreHandedToTheBoard(t *testing.T) {
	cfg, r := boardWithAction(t)
	s := New(cfg, r)
	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var st State
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	for _, sv := range st.Stages {
		if sv.Name != "working" {
			if len(sv.Actions) != 0 {
				t.Errorf("stage %q offers actions it never declared", sv.Name)
			}
			continue
		}
		if len(sv.Actions) != 1 || sv.Actions[0].Name != "merge-now" || sv.Actions[0].Label != "Merge now" {
			t.Errorf("working actions = %+v, want the one it declares", sv.Actions)
		}
	}
}

// A script saying what it is waiting for is the difference between a resting
// item and a stuck one, which look identical on a board otherwise.
func TestWaitingIsReadOffTheRunsResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.json"),
		[]byte(`{"waiting":{"until":"2026-09-08T12:00:00Z","why":"the PR must stay quiet"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := waitingAt(dir)
	if !ok || got.Why != "the PR must stay quiet" || got.Until.IsZero() {
		t.Fatalf("waitingAt = %+v, %v; want both fields", got, ok)
	}

	// A wait with no deadline is ordinary — waiting for a review, for a
	// person, for a check that takes as long as it takes.
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "result.json"),
		[]byte(`{"waiting":{"why":"a human is reviewing it"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := waitingAt(bare); !ok || !got.Until.IsZero() {
		t.Errorf("waitingAt = %+v, %v; want a wait with no clock", got, ok)
	}

	// Most stages that exit 10 have nothing to add, and that is not an error.
	quiet := t.TempDir()
	if err := os.WriteFile(filepath.Join(quiet, "result.json"), []byte(`{"blocked":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := waitingAt(quiet); ok {
		t.Error("a result that said nothing about waiting was read as a wait")
	}
}

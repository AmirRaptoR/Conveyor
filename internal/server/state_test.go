package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

func boardFor(t *testing.T) (*config.Config, *runner.Runner) {
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

// The board draws items in the order it receives them, so /api/state has to
// hand them over in the order the scheduler will reach them — not in the order
// the list scripts happened to print them.
func TestStateIsHandedOverInPickOrder(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	listed := []model.Item{
		{ID: "s1:32", Source: "s1", Stage: "backlog", Title: "listed first"},
		{ID: "s1:109", Source: "s1", Stage: "backlog", Title: "dragged to the top"},
		{ID: "s1:4", Source: "s1", Stage: "done", Title: "finished"},
	}
	s.state.Items = listed
	s.state.Order = []string{"s1:109"}

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))

	var got State
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"s1:109", "s1:32", "s1:4"}
	for i, id := range want {
		if got.Items[i].ID != id {
			t.Fatalf("items = %v, want %v", ids(got.Items), want)
		}
	}
	// And the cached listing is untouched: the scheduler reads the same slice.
	if s.state.Items[0].ID != "s1:32" {
		t.Errorf("the cached listing was reordered: %v", ids(s.state.Items))
	}
}

func ids(items []model.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// A drag out of the backlog starts the work there and then. The endpoint can
// only start the transition the scheduler would have started anyway.
func TestStartRunsTheNextTransitionNow(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog", Title: "waiting"}}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:1/start", strings.NewReader(`{"stage":"working"}`))
	req.SetPathValue("id", "s1:1")
	s.handleStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", w.Code, w.Body.String())
	}
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })
	s.mu.RLock()
	got := s.state.Items[0].Stage
	s.mu.RUnlock()
	if got != "done" {
		t.Errorf("stage = %q, want done — backlog is a queue, so working ran and routed on", got)
	}
}

// A drop is a decision about when, not about which stage: hand-skipping to a
// later stage is a deploy nobody reviewed.
func TestStartCannotSkipAStage(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog"}}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:1/start", strings.NewReader(`{"stage":"done"}`))
	req.SetPathValue("id", "s1:1")
	s.handleStart(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "working") {
		t.Errorf("refusal = %q, want it to name the stage that is actually next", w.Body.String())
	}
}

// A busy slot is a refusal, and the refusal says what is holding it.
func TestStartRefusesABusySourceByName(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog"}}
	if !s.eng.Locks().TryAcquire("s1", "working") {
		t.Fatal("could not take the lock the test needs held")
	}
	s.setActive("s1:9", &Active{Source: "s1", Stage: "working", ItemID: "s1:9", Title: "in hand"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:1/start", strings.NewReader(`{"stage":"working"}`))
	req.SetPathValue("id", "s1:1")
	s.handleStart(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "s1:9") {
		t.Errorf("refusal = %q, want it to name what s1 is busy with", body)
	}
}

// A marked item is waiting for a person; dragging it does not override that.
func TestStartRefusesAMarkedItem(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:1/start", nil)
	req.SetPathValue("id", "s1:1")
	s.handleStart(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "waiting for a person") {
		t.Errorf("refusal = %q, want it to say a person has to clear the mark", body)
	}
}

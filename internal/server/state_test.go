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

// The mark is only half the story; the board has to be able to say what the
// item is waiting for, or a red card sends you to the logs to find out.
func TestUnblockClearsTheMarkWhereTheItemStands(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Reason: "needs a decision", Stage: "working"}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:1/unblock", nil)
	req.SetPathValue("id", "s1:1")
	s.handleUnblock(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d (%s), want 204", w.Code, w.Body.String())
	}
	s.mu.RLock()
	it, note := s.state.Items[0], s.blocks["s1:1"]
	s.mu.RUnlock()
	if it.Blocked {
		t.Error("the item is still marked")
	}
	if it.Stage != "working" {
		t.Errorf("stage = %q, want working — clearing a mark moves nothing", it.Stage)
	}
	if note.Reason != "" {
		t.Errorf("the note outlived the mark: %q", note.Reason)
	}
}

// The whole board at once, for after the thing that stopped it is fixed.
func TestUnblockAllHandsBackEveryMarkedItem(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:2", Source: "s1", Stage: "backlog"},
		{ID: "s1:3", Source: "s1", Stage: "working", Blocked: true},
	}
	var held []model.Item
	for _, it := range s.state.Items {
		if it.Blocked {
			held = append(held, it)
		}
	}
	if n := s.unblockAll(t.Context(), held); n != 2 {
		t.Fatalf("unblocked %d, want 2", n)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.state.Items {
		if it.Blocked {
			t.Errorf("%s is still marked", it.ID)
		}
	}
}

// The reason is recovered from the run that wrote it, so a restart does not
// leave a board full of red cards that cannot say why.
func TestBlockReasonsAreRecoveredFromRunHistory(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	day := filepath.Join(r.Root, "2026-08-28", "120000.000-aaaa")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"120000.000-aaaa","source":"s1","itemId":"s1:1","kind":"stage",
	          "to":"working","outcome":"blocked","exitCode":20}`
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(day, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("meta.json", meta)
	write("result.json", `{"blocked":true,"reason":"the checkout is dirty"}`)

	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}})
	if got := s.blocks["s1:1"].Reason; got != "the checkout is dirty" {
		t.Errorf("reason = %q, want the one the run recorded", got)
	}
	if got := s.blocks["s1:1"].RunID; got != "120000.000-aaaa" {
		t.Errorf("runId = %q, want the run that marked it", got)
	}
}

// A mark with no run behind it says so, rather than showing an empty card.
func TestAHandPlacedMarkSaysSo(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.recallBlocks([]model.Item{{ID: "s1:9", Source: "s1", Stage: "working", Blocked: true}})
	if got := s.blocks["s1:9"].Reason; !strings.Contains(got, "outside the pipeline") {
		t.Errorf("reason = %q, want it to say nothing in the history explains it", got)
	}
}

// The stall retry is guarded on "everything", not "something": while one item
// can still move, a mark is a decision, and clearing it spends an agent run to
// be told the same thing again.
func TestStallRetryWaitsWhileAnythingCanStillMove(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:2", Source: "s1", Stage: "backlog"}, // this one can still go
	}
	ctx, cancel := context.WithCancel(t.Context())
	go s.stalled(ctx, 10*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	cancel()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.state.Items[0].Blocked {
		t.Error("the mark was cleared while another item could still move")
	}
}

// And when nothing at all can move, it hands the board back.
func TestStallRetryClearsATotallyStalledBoard(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:2", Source: "s1", Stage: "backlog", Blocked: true},
		{ID: "s1:3", Source: "s1", Stage: "done"}, // finished: not a way forward
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.stalled(ctx, 10*time.Millisecond)
	waitFor(t, "the stalled board to be handed back", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return !s.state.Items[0].Blocked && !s.state.Items[1].Blocked
	})
}

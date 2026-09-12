package server

import (
	"context"
	"encoding/json"
	"fmt"
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

// A mark with no run behind it, but a reason the listing itself supplied (a
// closed issue found sitting in a non-terminal stage, say), shows that reason
// on the card instead of the generic "no history explains this" filler —
// CONTRACTS.md §6.
func TestAListingSuppliedReasonIsShownWithNoRunToExplainIt(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.recallBlocks([]model.Item{{ID: "s1:9", Source: "s1", Stage: "working", Blocked: true,
		BlockReason: "closed while still in a non-terminal stage; a person should reconcile it"}})
	if got := s.blocks["s1:9"].Reason; got != "closed while still in a non-terminal stage; a person should reconcile it" {
		t.Errorf("reason = %q, want the listing's own reason", got)
	}
}

// The listing's account of a mark beats run history, because the provider
// reads it off the item itself: the GitHub adapter writes the reason into a
// marked section of the issue body and reads that same section back, so it is
// this poll's truth. Run history is history — the newest stop of that stage
// may have been cleared and replaced since, and showing it sends the reader
// to a log that explains nothing.
func TestTheListingsOwnReasonBeatsRunHistory(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	writeBlockedRun(t, r.Root, "s1:1", "working", `{"blocked":true,"reason":"an older stop, since cleared"}`)
	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true,
		BlockKind: "status", BlockReason: "why it is actually stopped right now"}})
	if got := s.blocks["s1:1"].Reason; got != "why it is actually stopped right now" {
		t.Errorf("reason = %q, want the listing's own reason", got)
	}
	if got := s.blocks["s1:1"].Kind; got != "status" {
		t.Errorf("kind = %q, want the listing's own kind", got)
	}
}

// Run history still answers when the listing says nothing — a provider with
// nowhere to record a reason, or a mark whose reason predates the section
// move.sh now writes.
func TestRunHistoryStillAnswersWhenTheListingSaysNothing(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	writeBlockedRun(t, r.Root, "s1:1", "working", `{"blocked":true,"reason":"the checkout is dirty"}`)
	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}})
	if got := s.blocks["s1:1"].Reason; got != "the checkout is dirty" {
		t.Errorf("reason = %q, want the run's own reason", got)
	}
}

// A run of a stage the item has since left explains nothing about why it is
// stopped now. The walk is newest-first over the whole store, so without this
// the newest failure anywhere answered for a mark it had nothing to do with.
func TestAMarkIsNotExplainedByARunOfADifferentStage(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	writeBlockedRun(t, r.Root, "s1:1", "some-other-stage", `{"blocked":true,"reason":"a failure three stages ago"}`)
	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}})
	if got := s.blocks["s1:1"].Reason; strings.Contains(got, "three stages ago") {
		t.Errorf("reason = %q, want the other stage's run to be ignored", got)
	}
}

// writeBlockedRun files one finished stage run that marked an item, as the
// runner would have.
func writeBlockedRun(t *testing.T, root, itemID, stage, result string) {
	t.Helper()
	dir := filepath.Join(root, "2026-08-28", "120000.000-"+stage)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"id":"120000.000-%s","source":"s1","itemId":%q,"kind":"stage",
	          "to":%q,"outcome":"blocked","exitCode":20}`, stage, itemID, stage)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(result), 0o644); err != nil {
		t.Fatal(err)
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

// A held item is not a blocked item. Blocked is a mark somebody has to
// clear; held disappears on its own when the dependency moves. The board is
// told which is which, or it lies about what needs a person.
func TestHeldIsReportedAndIsNotAMark(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "backlog"},
		{ID: "s1:2", Source: "s1", Stage: "backlog", DependsOn: []string{"s1:1"}},
	}

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var got State
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	h, ok := got.Held["s1:2"]
	if !ok {
		t.Fatal("the follower was not reported as held")
	}
	if h.By != "s1:1" || h.Stage != "backlog" || h.Target != "working" {
		t.Fatalf("hold = %+v, want by s1:1 in backlog, target working", h)
	}
	if h.Blocked {
		t.Fatal("an unmarked dependency was reported as blocked")
	}
	if _, ok := got.Held["s1:1"]; ok {
		t.Fatal("the item at the head of the sequence was reported as held")
	}
	// blocked:false and no entry in blocks — a hold is never drawn as a mark.
	if got.Items[1].Blocked {
		t.Fatal("a held item must not itself be reported blocked")
	}
	if _, inBlocks := got.Blocks["s1:2"]; inBlocks {
		t.Fatal("a held item must carry no Blocks entry")
	}
}

// held is computed fresh on every request, never cached: once the dependency
// itself moves on, the follower's entry must be gone from the very next
// /api/state — no stale hold left drawn on a card that is free to move.
func TestHeldEntryDisappearsOnceTheDependencyMovesOn(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "backlog"},
		{ID: "s1:2", Source: "s1", Stage: "backlog", DependsOn: []string{"s1:1"}},
	}
	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var first State
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if _, held := first.Held["s1:2"]; !held {
		t.Fatal("setup: the follower should start out held")
	}

	// The dependency moves on to "working" — s1:2 may now share that stage.
	s.mu.Lock()
	s.state.Items[0].Stage = "working"
	s.mu.Unlock()

	w2 := httptest.NewRecorder()
	s.handleState(w2, httptest.NewRequest("GET", "/api/state", nil))
	var second State
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if _, held := second.Held["s1:2"]; held {
		t.Fatal("the follower is still reported held after its dependency moved on")
	}
}

// A held item cannot be dragged to start early either: the start endpoint
// refuses with 409 and names the dependency and its stage.
func TestStartRefusesAHeldItem(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "backlog"},
		{ID: "s1:2", Source: "s1", Stage: "backlog", DependsOn: []string{"s1:1"}},
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:2/start", nil)
	req.SetPathValue("id", "s1:2")
	s.handleStart(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d (%s), want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "s1:1") || !strings.Contains(w.Body.String(), "backlog") {
		t.Errorf("refusal = %q, want it to name the dependency and its stage", w.Body.String())
	}
}

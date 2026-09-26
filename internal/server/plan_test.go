package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/plan"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// --- handlePlanUpdate: card summary only for a stage run naming a stage ----

func TestHandlePlanUpdateRecordsCardEntryForStageRun(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	rev := plan.Revision{V: 1, Rev: 1, At: time.Now(), Todos: []plan.Todo{
		{ID: "a", Text: "step one", Status: plan.StatusInProgress, Active: "doing step one"},
		{ID: "b", Text: "step two", Status: plan.StatusCompleted},
	}}
	s.handlePlanUpdate("r1", "s1:1", runner.PlanUpdate{Revision: rev, HasRevision: true, Kind: "stage", Stage: "working"})

	got, ok := s.plans["s1:1"]
	if !ok {
		t.Fatal("no plan entry recorded for a stage run")
	}
	if got.RunID != "r1" || got.Stage != "working" || got.Total != 2 || got.Completed != 1 {
		t.Errorf("got %+v", got)
	}
	if got.InProgress != "doing step one" {
		t.Errorf("inProgress = %q, want the active-form text", got.InProgress)
	}
}

func TestHandlePlanUpdateRejectedOnlyHasNoVisibleRevision(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.handlePlanUpdate("r1", "s1:1", runner.PlanUpdate{Rejected: 1, Kind: "stage", Stage: "working"})

	if got, ok := s.plans["s1:1"]; ok {
		t.Fatalf("rejected-only update created a visible revision: %+v", got)
	}
	if got := s.planMisses["s1:1"]; got.RunID != "r1" || got.Stage != "working" {
		t.Fatalf("negative cursor = %+v", got)
	}
}

func TestHandlePlanUpdateRejectionKeepsLastGoodRevision(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	rev := plan.Revision{V: 1, Rev: 4, At: time.Now(), Todos: []plan.Todo{{ID: "a", Text: "step", Status: plan.StatusPending}}}
	s.handlePlanUpdate("r1", "s1:1", runner.PlanUpdate{Revision: rev, HasRevision: true, Kind: "stage", Stage: "working"})
	s.handlePlanUpdate("r1", "s1:1", runner.PlanUpdate{Rejected: 1, Kind: "stage", Stage: "working"})

	got, ok := s.plans["s1:1"]
	if !ok || got.Rev != 4 || got.Rejected != 1 {
		t.Fatalf("good revision was not retained with the rejection: %+v, ok=%v", got, ok)
	}
}

// A move, list or status run publishing a revision is recorded in
// its own run directory (served by GET /api/runs/{id}) and never reaches a
// card.
func TestHandlePlanUpdateIgnoresNonStageKindsForTheCard(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	rev := plan.Revision{V: 1, Rev: 1, At: time.Now(), Todos: []plan.Todo{{ID: "a", Text: "x", Status: plan.StatusPending}}}
	for _, kind := range []string{"move", "list", "status", "preflight"} {
		s.handlePlanUpdate("r1", "s1:1", runner.PlanUpdate{Revision: rev, HasRevision: true, Kind: kind, Stage: "working"})
	}
	if _, ok := s.plans["s1:1"]; ok {
		t.Error("a non-stage-kind run's revision reached the item's card entry")
	}
}

// A stage run with no target stage (should not happen, but nothing in the
// callback contract enforces it) is treated the same as a non-stage kind:
// never a card entry.
func TestHandlePlanUpdateIgnoresStageRunWithNoTargetStage(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	rev := plan.Revision{V: 1, Rev: 1, At: time.Now(), Todos: []plan.Todo{{ID: "a", Text: "x", Status: plan.StatusPending}}}
	s.handlePlanUpdate("r1", "s1:1", runner.PlanUpdate{Revision: rev, HasRevision: true, Kind: "stage", Stage: ""})
	if _, ok := s.plans["s1:1"]; ok {
		t.Error("a stage run naming no target stage reached the card")
	}
}

// The SSE plan event is published for every accepted revision and every
// rejection, whatever the run's kind — the panel may be following any run.
func TestHandlePlanUpdatePublishesSSEForEveryKind(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	s.handlePlanUpdate("r1", "s1:1", runner.PlanUpdate{HasRevision: false, Rejected: 1, Kind: "move", Stage: ""})

	select {
	case e := <-ch:
		if e.Kind != "plan" || e.RunID != "r1" || e.ItemID != "s1:1" || e.Rejected == nil || *e.Rejected != 1 || e.Plan != nil {
			t.Errorf("got %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no plan SSE event published")
	}
}

func TestPlanSSEAlwaysIncludesZeroRejected(t *testing.T) {
	zero := 0
	b, err := json.Marshal(event{Kind: "plan", RunID: "r1", Rejected: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"rejected":0`) {
		t.Fatalf("zero rejected count was omitted from SSE JSON: %s", b)
	}
}

// --- /api/state: plans exposure and stale-stage filtering ------------------

func TestStateReturnsPlans(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}}
	s.plans["s1:1"] = PlanView{RunID: "r1", Stage: "working", Rev: 2, Total: 3, Completed: 1}

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var got State
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	pv, ok := got.Plans["s1:1"]
	if !ok {
		t.Fatal("no plan entry in /api/state")
	}
	if pv.RunID != "r1" || pv.Total != 3 || pv.Completed != 1 {
		t.Errorf("got %+v", pv)
	}
}

func TestStatePlanShapeIncludesEmptyProgressAndZeroRejected(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}}
	s.handlePlanUpdate("r1", "s1:1", runner.PlanUpdate{
		Revision:    plan.Revision{V: 1, Rev: 1, At: at, Todos: []plan.Todo{}},
		HasRevision: true, Kind: "stage", Stage: "working",
	})

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	plans := raw["plans"].(map[string]any)
	pv := plans["s1:1"].(map[string]any)
	for key, want := range map[string]any{
		"runId": "r1", "stage": "working", "rev": float64(1),
		"total": float64(0), "completed": float64(0), "inProgress": "", "rejected": float64(0),
	} {
		if got, ok := pv[key]; !ok || got != want {
			t.Errorf("plans[s1:1].%s = %#v, present=%v, want %#v", key, got, ok, want)
		}
	}
}

// A completed stage's plan must never trail an item that has already moved
// on: an entry whose Stage no longer matches the item's current one is
// dropped from the response, though the underlying cache is left alone.
func TestStateDropsPlanEntryForAStaleStage(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "done"}}
	s.plans["s1:1"] = PlanView{RunID: "r1", Stage: "working"}

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var got State
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Plans["s1:1"]; ok {
		t.Error("a plan entry for a stage the item has left was still shown")
	}
	if _, ok := s.plans["s1:1"]; !ok {
		t.Error("the underlying cache entry was mutated by building the response")
	}
}

// --- clearing on dispatch ---------------------------------------------------

// A new transition starting is a clean slate for the card, even before the
// new run's own script has a chance to say otherwise.
func TestPlanClearedOnNewDispatch(t *testing.T) {
	cfg, r := boardFor(t) // work.sh just exits 0 — publishes no plan of its own
	s := New(cfg, r)
	s.ctx = context.Background()
	item := model.Item{ID: "s1:1", Source: "s1", Stage: "working"}
	s.plans[item.ID] = PlanView{RunID: "stale", Stage: "working", Total: 5}
	s.planMisses[item.ID] = planCursor{RunID: "older-empty", Stage: "working"}

	s.runOne(context.Background(), item, "working")

	if _, ok := s.plans[item.ID]; ok {
		t.Error("a stale plan entry survived a fresh dispatch that published nothing")
	}
	if _, ok := s.planMisses[item.ID]; ok {
		t.Error("a negative plan entry survived the dispatch and stage change")
	}
}

// --- restart recovery --------------------------------------------------

func writeRunMeta(t *testing.T, r *runner.Runner, day, runID, itemID, kind, from, to, outcome, finishedAt string) string {
	t.Helper()
	dir := filepath.Join(r.Root, day, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"` + runID + `","source":"s1","itemId":"` + itemID + `","kind":"` + kind + `",
	          "from":"` + from + `","to":"` + to + `","outcome":"` + outcome + `","finishedAt":"` + finishedAt + `"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writePlanFile(t *testing.T, dir string, lines ...string) {
	t.Helper()
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A missing or stale plan entry is recovered from the newest kind:"stage" run
// of the item, in the stage it currently occupies.
func TestPlanIsRecoveredFromRunHistory(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	dir := writeRunMeta(t, r, "2026-08-28", "120000.000-aaaa", "s1:1", "stage", "backlog", "working", "success", "2026-08-28T12:00:05Z")
	writePlanFile(t, dir,
		`{"v":1,"rev":1,"at":"2026-08-28T12:00:01Z","todos":[{"id":"a","text":"x","status":"in_progress"}]}`,
		`{"v":1,"rev":2,"at":"2026-08-28T12:00:02Z","todos":[{"id":"a","text":"x","status":"completed"}]}`,
	)

	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}})

	got, ok := s.plans["s1:1"]
	if !ok {
		t.Fatal("no plan entry recovered")
	}
	if got.RunID != "120000.000-aaaa" || got.Stage != "working" || got.Rev != 2 || got.Completed != 1 || got.Total != 1 {
		t.Errorf("got %+v", got)
	}
}

// The newest kind:"stage" run in the current stage wins over an older one,
// even when the older one had a plan and the newer one does not.
func TestPlanRecoveryPrefersNewestRunEvenWhenItHasNoPlan(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	oldDir := writeRunMeta(t, r, "2026-08-28", "110000.000-aaaa", "s1:1", "stage", "backlog", "working", "success", "2026-08-28T11:00:00Z")
	writePlanFile(t, oldDir, `{"v":1,"rev":1,"at":"2026-08-28T11:00:00Z","todos":[{"id":"a","text":"x","status":"pending"}]}`)
	// Newer run, same stage, no plan.jsonl content of its own.
	newDir := writeRunMeta(t, r, "2026-08-28", "120000.000-bbbb", "s1:1", "stage", "working", "working", "failure", "2026-08-28T12:00:00Z")
	writePlanFile(t, newDir)

	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}})

	if _, ok := s.plans["s1:1"]; ok {
		t.Error("recovery searched past the newest matching run instead of stopping at it")
	}
}

// A run whose target stage differs from the item's current one is not a
// candidate at all.
func TestPlanRecoverySkipsWrongStageRuns(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	dir := writeRunMeta(t, r, "2026-08-28", "120000.000-cccc", "s1:1", "stage", "backlog", "reviewing", "success", "2026-08-28T12:00:00Z")
	writePlanFile(t, dir, `{"v":1,"rev":1,"at":"2026-08-28T12:00:00Z","todos":[{"id":"a","text":"x","status":"pending"}]}`)

	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}})

	if _, ok := s.plans["s1:1"]; ok {
		t.Error("a run targeting a different stage was recovered as the item's plan")
	}
}

// No plan.jsonl at all — a run that finished before this feature shipped —
// yields no entry.
func TestPlanRecoveryNoPlanFileYieldsNoEntry(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	writeRunMeta(t, r, "2026-08-28", "120000.000-dddd", "s1:1", "stage", "backlog", "working", "success", "2026-08-28T12:00:00Z")

	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}})

	if _, ok := s.plans["s1:1"]; ok {
		t.Error("an entry was invented for a run with no plan.jsonl")
	}
}

// A plan.jsonl holding only rejected lines is the same as no plan.
func TestPlanRecoveryOnlyRejectedLinesYieldsNoEntry(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	dir := writeRunMeta(t, r, "2026-08-28", "120000.000-eeee", "s1:1", "stage", "backlog", "working", "success", "2026-08-28T12:00:00Z")
	writePlanFile(t, dir, `not json at all`)

	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}})

	if _, ok := s.plans["s1:1"]; ok {
		t.Error("a plan.jsonl with only rejected lines was recovered as an entry")
	}
}

func TestPlanRecoveryNewestRejectedRunClearsStalePositiveAndCachesMiss(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	oldDir := writeRunMeta(t, r, "2026-08-28", "110000.000-old", "s1:1", "stage", "backlog", "working", "success", "2026-08-28T11:00:00Z")
	writePlanFile(t, oldDir, `{"v":1,"rev":1,"at":"2026-08-28T11:00:00Z","todos":[{"id":"a","text":"old","status":"pending"}]}`)
	newDir := writeRunMeta(t, r, "2026-08-28", "120000.000-new", "s1:1", "stage", "working", "working", "failure", "2026-08-28T12:00:00Z")
	writePlanFile(t, newDir, `not json`)
	s.plans["s1:1"] = PlanView{RunID: "stale", Stage: "backlog", Rev: 9}

	item := model.Item{ID: "s1:1", Source: "s1", Stage: "working"}
	s.recallBlocks([]model.Item{item})
	if got, ok := s.plans[item.ID]; ok {
		t.Fatalf("newest rejected-only run did not clear stale positive state: %+v", got)
	}
	if got := s.planMisses[item.ID]; got.RunID != "120000.000-new" || got.Stage != "working" {
		t.Fatalf("negative cursor = %+v", got)
	}

	// If recovery scans again, this newly appended valid revision would
	// become visible. The negative cursor makes the newest run's empty verdict
	// durable for the process instead.
	f, err := os.OpenFile(filepath.Join(newDir, "plan.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"v":1,"rev":2,"at":"2026-08-28T12:00:01Z","todos":[]}` + "\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	s.recallBlocks([]model.Item{item})
	if got, ok := s.plans[item.ID]; ok {
		t.Fatalf("negative recovery result was rescanned: %+v", got)
	}
}

func TestPlanRecoveryDoesNotPassADispatchTombstone(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	dir := writeRunMeta(t, r, "2026-08-28", "120000.000-old", "s1:1", "stage", "backlog", "working", "success", "2026-08-28T12:00:00Z")
	writePlanFile(t, dir, `{"v":1,"rev":1,"at":"2026-08-28T12:00:00Z","todos":[{"id":"a","text":"historical","status":"pending"}]}`)

	// runOne installs this before the engine starts any provider or stage
	// script. Recovery while that transition is working must not resurrect
	// the previous run's plan into the cleared card.
	s.mu.Lock()
	s.planMisses["s1:1"] = planCursor{Stage: "working"}
	s.planGeneration["s1:1"]++
	s.mu.Unlock()
	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}})
	if got, ok := s.plans["s1:1"]; ok {
		t.Fatalf("recovery passed a dispatch tombstone: %+v", got)
	}
}

// Recovery never overwrites a newer live entry: a revision arriving from a
// running transition while the history walk is in flight wins.
func TestPlanRecoveryNeverOverwritesANewerLiveEntry(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	itemID := "s1:1"
	storeGeneration := s.runStoreGen
	itemGenerations := map[string]uint64{itemID: s.planGeneration[itemID]}
	stages := map[string]string{itemID: "working"}
	found := map[string]PlanView{itemID: {RunID: "historical", Stage: "working", Rev: 1}}

	// This callback lands after recovery took its generation snapshot but
	// before that scan merges what it read.
	live := plan.Revision{V: 1, Rev: 9, At: time.Now(), Todos: []plan.Todo{{ID: "a", Text: "live", Status: plan.StatusPending}}}
	s.handlePlanUpdate("live-run", itemID, runner.PlanUpdate{Revision: live, HasRevision: true, Kind: "stage", Stage: "working"})
	s.mergeRecoveredPlans(storeGeneration, itemGenerations, stages, found, map[string]string{itemID: "historical"})

	if got := s.plans[itemID]; got.RunID != "live-run" || got.Rev != 9 {
		t.Errorf("recovery overwrote a newer live entry: got %+v", got)
	}
}

// --- GET /api/runs/{id} -----------------------------------------------------

func TestHandleRunCarriesPlan(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	dir := filepath.Join(r.Root, "2026-08-28", "120000.000-gggg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"120000.000-gggg","source":"s1","kind":"stage","to":"working","outcome":"success"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	writePlanFile(t, dir, `{"v":1,"rev":1,"at":"2026-08-28T12:00:00Z","todos":[{"id":"a","text":"x","status":"completed"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "log.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/runs/120000.000-gggg", nil)
	req.SetPathValue("id", "120000.000-gggg")
	w := httptest.NewRecorder()
	s.handleRun(w, req)

	var got RunMeta
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Plan == nil {
		t.Fatal("no plan carried on the run")
	}
	if got.Plan.Accepted != 1 || got.Plan.Revision == nil || got.Plan.Revision.Rev != 1 {
		t.Errorf("got %+v", got.Plan)
	}
}

// A run whose plan.jsonl holds only rejected lines still reports the
// rejected count — a misbehaving adapter is visible without opening a file.
func TestHandleRunCarriesRejectedCountWithNoAcceptedRevision(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	dir := filepath.Join(r.Root, "2026-08-28", "120000.000-hhhh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"120000.000-hhhh","source":"s1","kind":"stage","to":"working","outcome":"success"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	writePlanFile(t, dir, `not json`)
	if err := os.WriteFile(filepath.Join(dir, "log.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/runs/120000.000-hhhh", nil)
	req.SetPathValue("id", "120000.000-hhhh")
	w := httptest.NewRecorder()
	s.handleRun(w, req)

	var got RunMeta
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Plan == nil || got.Plan.Rejected != 1 || got.Plan.Accepted != 0 || got.Plan.Revision != nil {
		t.Errorf("got %+v", got.Plan)
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	planJSON := raw["plan"].(map[string]any)
	if _, ok := planJSON["revision"]; ok {
		t.Fatalf("rejected-only plan exposed a synthetic revision: %#v", planJSON)
	}
}

func TestHandleRunActivePartialIsPendingButFinishedPartialIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name, runID, outcome string
		wantPlan             bool
	}{
		{name: "active", runID: "120000.000-active", outcome: "running", wantPlan: false},
		{name: "finished", runID: "120000.000-finish", outcome: "success", wantPlan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, r := boardFor(t)
			s := New(cfg, r)
			dir := filepath.Join(r.Root, "2026-08-28", tc.runID)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			meta := `{"id":"` + tc.runID + `","source":"s1","kind":"stage","to":"working","outcome":"` + tc.outcome + `"}`
			if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "plan.jsonl"), []byte(`{"v":1,"rev":1`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "log.txt"), nil, 0o600); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("GET", "/api/runs/"+tc.runID, nil)
			req.SetPathValue("id", tc.runID)
			w := httptest.NewRecorder()
			s.handleRun(w, req)
			var got RunMeta
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if tc.wantPlan {
				if got.Plan == nil || got.Plan.Rejected != 1 || got.Plan.Revision != nil {
					t.Fatalf("finished partial plan = %+v", got.Plan)
				}
			} else if got.Plan != nil {
				t.Fatalf("active partial line was prematurely finalized: %+v", got.Plan)
			}
		})
	}
}

func TestHandleRunCarriesAcceptedEmptyTodos(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	dir := filepath.Join(r.Root, "2026-08-28", "120000.000-empty")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"id":"120000.000-empty","source":"s1","kind":"stage","to":"working","outcome":"success"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writePlanFile(t, dir, `{"v":1,"rev":1,"at":"2026-08-28T12:00:00Z","todos":[]}`)
	if err := os.WriteFile(filepath.Join(dir, "log.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/runs/120000.000-empty", nil)
	req.SetPathValue("id", "120000.000-empty")
	w := httptest.NewRecorder()
	s.handleRun(w, req)
	var got RunMeta
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Plan == nil || got.Plan.Revision == nil || got.Plan.Revision.Todos == nil || len(got.Plan.Revision.Todos) != 0 {
		t.Fatalf("accepted empty todos were not represented as an empty revision: %+v", got.Plan)
	}
}

// --- retention eviction -----------------------------------------------------

func TestRetentionSweepEvictsCachedPlanEntry(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	old := time.Now().Add(-90 * 24 * time.Hour).UTC()
	day := old.Format("2006-01-02")
	runID := "010000.000-iiii"
	dir := filepath.Join(r.Root, day, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"` + runID + `","source":"s1","itemId":"s1:1","kind":"stage","to":"working","outcome":"success"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	s.plans["s1:1"] = PlanView{RunID: runID, Stage: "working", Rev: 1}
	s.planMisses["s1:2"] = planCursor{RunID: runID, Stage: "working"}
	storeGeneration := s.runStoreGen

	allowRunRetention(s)
	s.runSweep(os.Stderr)
	s.mergeRecoveredPlans(
		storeGeneration,
		map[string]uint64{"s1:3": 0},
		map[string]string{"s1:3": "working"},
		map[string]PlanView{"s1:3": {RunID: runID, Stage: "working", Rev: 1}},
		map[string]string{"s1:3": runID},
	)

	if _, ok := s.plans["s1:1"]; ok {
		t.Error("a cached plan entry survived the sweep that deleted its run directory")
	}
	if _, ok := s.planMisses["s1:2"]; ok {
		t.Error("a negative recovery entry survived the sweep that deleted its run directory")
	}
	if _, ok := s.plans["s1:3"]; ok {
		t.Error("a recovery scan reinserted a run after retention deleted it")
	}
}

func TestRetentionWaitsForRunStoreReaders(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	old := time.Now().Add(-90 * 24 * time.Hour).UTC()
	dir := writeRunMeta(t, r, old.Format("2006-01-02"), "010000.000-lock", "s1:1", "stage", "backlog", "working", "success", old.Format(time.RFC3339))

	s.runStoreMu.RLock()
	done := make(chan struct{})
	go func() {
		allowRunRetention(s)
		s.runSweep(os.Stderr)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("retention completed while an API/recovery read lock was held")
	case <-time.After(25 * time.Millisecond):
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("retention deleted a run during a protected read: %v", err)
	}
	s.runStoreMu.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retention did not continue after readers released the run store")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expired run still exists after retention: %v", err)
	}
}

// --- pruning on board-leave --------------------------------------------------

func TestPlanEntryPrunedWhenItemLeavesTheBoard(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.plans["s1:9"] = PlanView{RunID: "r1", Stage: "working"}
	s.planMisses["s1:8"] = planCursor{RunID: "r2", Stage: "working"}
	s.state.Items = nil // s1:9 is not on the board at all
	s.refresh(s.ctx)

	s.mu.RLock()
	_, held := s.plans["s1:9"]
	s.mu.RUnlock()
	if held {
		t.Error("plan entry for an off-board item survived a refresh")
	}
	if _, held := s.planMisses["s1:8"]; held {
		t.Error("negative plan entry for an off-board item survived a refresh")
	}
}

func TestConcurrentServerPlanUpdatesStayItemScoped(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.mu.Lock()
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "working"},
		{ID: "s2:1", Source: "s1", Stage: "working"},
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, tc := range []struct{ itemID, runID, text string }{
		{"s1:1", "r1", "one"},
		{"s2:1", "r2", "two"},
	} {
		tc := tc
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rev := 1; rev <= 100; rev++ {
				s.handlePlanUpdate(tc.runID, tc.itemID, runner.PlanUpdate{
					Revision:    plan.Revision{V: 1, Rev: rev, At: time.Now(), Todos: []plan.Todo{{ID: "a", Text: tc.text, Status: plan.StatusInProgress}}},
					HasRevision: true, Kind: "stage", Stage: "working",
				})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s.handleState(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/state", nil))
		}
	}()
	wg.Wait()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := s.plans["s1:1"]; got.RunID != "r1" || got.Rev != 100 || got.InProgress != "one" {
		t.Errorf("first item plan crossed streams: %+v", got)
	}
	if got := s.plans["s2:1"]; got.RunID != "r2" || got.Rev != 100 || got.InProgress != "two" {
		t.Errorf("second item plan crossed streams: %+v", got)
	}
}

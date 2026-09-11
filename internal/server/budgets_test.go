package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// manyItemsOneSourceFor is a board with one source, one blocking "working"
// stage and generous concurrency — enough distinct items to drive a
// concurrent-claim race without the (source, stage) slot itself being the
// bottleneck, so a budget ceiling is the only thing under test.
func manyItemsOneSourceFor(t *testing.T, n int) (*config.Config, *runner.Runner) {
	t.Helper()
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`version: 1
concurrency:
  perSource: %d
  perStage: %d
  global: %d
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
`, n+1, n+1, n+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\nexit 0\n")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs"))
}

// F02-shaped: a claim refused for a spent execution budget must roll back
// exactly what it already took — the item claim and the (source, stage)
// slot — the same "changed nothing" guarantee a slot or item refusal keeps.
func TestClaimBudgetExhaustedRollsBackTheSlotAndTheClaim(t *testing.T) {
	cfg, r := manyItemsOneSourceFor(t, 4)
	cfg.Budgets = config.Budgets{MaxRunsPerItem: 1}
	s := New(cfg, r)
	s.ctx = context.Background()
	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}

	// Spend the item's one allowed run directly, bypassing claim, so the
	// claim under test is refused only on the budget.
	if ok, _, err := s.budgets.Reserve(item.ID, budgetDay(time.Now()), 1, 0); err != nil || !ok {
		t.Fatalf("seeding the item's one run = ok=%v err=%v, want granted", ok, err)
	}
	beforeSrc, beforeStage, beforeGlobal, _, _, _ := s.eng.Locks().Snapshot()

	if got := s.claim(item, "working", false); got != claimBudgetExhausted {
		t.Fatalf("claim() = %v, want claimBudgetExhausted", got)
	}
	if _, held := s.working.Load(item.ID); held {
		t.Error("working still holds the item after a budget refusal — the item claim was not rolled back")
	}
	afterSrc, afterStage, afterGlobal, _, _, _ := s.eng.Locks().Snapshot()
	if afterSrc["s1"] != beforeSrc["s1"] || afterStage["working"] != beforeStage["working"] || afterGlobal != beforeGlobal {
		t.Errorf("locks changed across a budget-refused claim: before src=%d stage=%d global=%d, after src=%d stage=%d global=%d",
			beforeSrc["s1"], beforeStage["working"], beforeGlobal, afterSrc["s1"], afterStage["working"], afterGlobal)
	}
}

// Unlike a paused agent's quota, the tick button's overridePause must not
// reach past a spent execution budget: it is an operator-defined ceiling,
// not a fact the outside world reports back on its own.
func TestClaimBudgetExhaustedIsNeverOverriddenByOverridePause(t *testing.T) {
	cfg, r := manyItemsOneSourceFor(t, 4)
	cfg.Budgets = config.Budgets{MaxRunsPerItem: 1}
	s := New(cfg, r)
	s.ctx = context.Background()
	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}

	if ok, _, err := s.budgets.Reserve(item.ID, budgetDay(time.Now()), 1, 0); err != nil || !ok {
		t.Fatal("could not seed the item's one run")
	}
	if got := s.claim(item, "working", true); got != claimBudgetExhausted {
		t.Errorf("claim() with overridePause=true = %v, want claimBudgetExhausted (a budget ceiling is never overridden by the tick button)", got)
	}
}

// A per-item ceiling rules out only that item — launch() still starts every
// other candidate, the same as a busy slot ruling out one item and not the
// whole pass.
func TestLaunchSkipsAnItemPastItsBudgetButStartsOthers(t *testing.T) {
	cfg, r, dir := twoSourcesFor(t)
	cfg.Budgets = config.Budgets{MaxRunsPerItem: 1}
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{
		{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"},
		{ID: "s2:1", Ref: "1", Source: "s2", Stage: "backlog"},
	}
	// s1:1 already spent its one allowed run; s2:1 has not.
	if ok, _, err := s.budgets.Reserve("s1:1", budgetDay(time.Now()), 1, 0); err != nil || !ok {
		t.Fatal("could not seed s1:1's one run")
	}

	if n := s.launch(s.ctx); n != 1 {
		t.Fatalf("launch() started %d, want 1 (only s2:1, s1:1 is past its budget)", n)
	}
	waitFor(t, "s2:1 to start", func() bool { _, running := s.active.Load("s2:1"); return running })
	if _, running := s.active.Load("s1:1"); running {
		t.Error("s1:1 was launched despite having spent its execution budget")
	}

	if err := os.WriteFile(filepath.Join(dir, "release2"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "s2:1's run to finish", func() bool { return s.inFlight.Load() == 0 })
}

// A daily ceiling is board-wide: two items on two different sources still
// share the one day's allowance.
func TestBudgetDailyCeilingIsSharedAcrossSources(t *testing.T) {
	cfg, r, _ := twoSourcesFor(t)
	cfg.Budgets = config.Budgets{MaxRunsPerDay: 1}
	s := New(cfg, r)
	s.ctx = context.Background()

	item1 := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}
	item2 := model.Item{ID: "s2:1", Ref: "1", Source: "s2", Stage: "backlog"}

	if got := s.claim(item1, "working", false); got != claimAccepted {
		t.Fatalf("claim() for the first item of the day = %v, want claimAccepted", got)
	}
	if got := s.claim(item2, "working", false); got != claimBudgetExhausted {
		t.Fatalf("claim() for a second item the same day = %v, want claimBudgetExhausted (the daily ceiling is board-wide, not per source)", got)
	}
}

// The check-then-start race #39's acceptance criteria call out, exercised
// through claim() itself rather than the store directly: concurrent callers
// racing one day's ceiling must never together claim more than it allows.
func TestConcurrentClaimsCannotOverspendTheDailyBudget(t *testing.T) {
	const ceiling = 5
	const attempts = 30
	cfg, r := manyItemsOneSourceFor(t, attempts)
	cfg.Budgets = config.Budgets{MaxRunsPerDay: ceiling}
	s := New(cfg, r)
	s.ctx = context.Background()

	var accepted int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			item := model.Item{ID: fmt.Sprintf("s1:%d", i), Ref: fmt.Sprintf("%d", i), Source: "s1", Stage: "backlog"}
			if s.claim(item, "working", false) == claimAccepted {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if accepted != ceiling {
		t.Fatalf("accepted = %d concurrent claims, want exactly %d", accepted, ceiling)
	}
	if got := s.budgets.DayUsage(budgetDay(time.Now())); got != ceiling {
		t.Errorf("DayUsage = %d after the race, want %d", got, ceiling)
	}
}

// The HTTP surface: an override lets one item run past its spent budget, and
// records who asked and why; Restore reimposes the ceiling exactly as
// Resume reimposes a manual pause's gates.
func TestBudgetOverrideAndRestoreAPI(t *testing.T) {
	cfg, r, _ := twoSourcesFor(t)
	cfg.Budgets = config.Budgets{MaxRunsPerItem: 1}
	s := New(cfg, r)
	s.ctx = context.Background()
	s.mode = ModeAuto

	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}
	// releaseClaim mimics transition's own cleanup (schedule.go) — claim only
	// reserves, so a test reusing one item across several claims must give it
	// back itself, exactly as a finished run's defers would.
	releaseClaim := func() {
		s.working.Delete(item.ID)
		s.eng.Locks().Release(item.Source, "working", s.cfg.ResourcesFor(item.Source, "working")...)
	}

	if got := s.claim(item, "working", false); got != claimAccepted {
		t.Fatalf("first claim() = %v, want claimAccepted", got)
	}
	releaseClaim()
	if got := s.claim(item, "working", false); got != claimBudgetExhausted {
		t.Fatalf("second claim() = %v, want claimBudgetExhausted", got)
	}

	post := func(path string, body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.SetPathValue("id", item.ID)
		req.SetBasicAuth("amir", "whatever")
		w := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(path, "/budget-override"):
			s.handleBudgetOverride(w, req)
		case strings.HasSuffix(path, "/budget-restore"):
			s.handleBudgetRestore(w, req)
		}
		return w
	}

	if w := post("/api/items/s1:1/budget-override", map[string]any{"reason": ""}); w.Code != http.StatusBadRequest {
		t.Errorf("override with no reason = %d, want 400", w.Code)
	}
	if w := post("/api/items/s1:1/budget-override", map[string]any{"reason": "let it retry once more"}); w.Code != http.StatusNoContent {
		t.Fatalf("override = %d, want 204: %s", w.Code, w.Body)
	}

	_, ov := s.budgets.Usage(item.ID)
	if ov == nil || ov.Reason != "let it retry once more" || ov.By != "amir" {
		t.Errorf("recorded override = %+v, want reason %q by %q", ov, "let it retry once more", "amir")
	}

	if got := s.claim(item, "working", false); got != claimAccepted {
		t.Fatalf("claim() after override = %v, want claimAccepted", got)
	}
	releaseClaim()

	if w := post("/api/items/s1:1/budget-restore", nil); w.Code != http.StatusNoContent {
		t.Errorf("restore = %d, want 204", w.Code)
	}
	if _, ov := s.budgets.Usage(item.ID); ov != nil {
		t.Error("override still present after restore")
	}
	if got := s.claim(item, "working", false); got != claimBudgetExhausted {
		t.Errorf("claim() after restore = %v, want claimBudgetExhausted (usage already at 2, ceiling is 1)", got)
	}

	// Restoring an item with no override is not an error.
	if w := post("/api/items/s2:1/budget-restore", nil); w.Code != http.StatusNoContent {
		t.Errorf("restore on an item with no override = %d, want 204", w.Code)
	}
}

// A manual start refused for a spent budget must say so plainly, the same
// as a manual pause or a busy slot does.
func TestManualStartRefusesOnASpentBudgetWithAClearMessage(t *testing.T) {
	cfg, r, _ := twoSourcesFor(t)
	cfg.Budgets = config.Budgets{MaxRunsPerItem: 1}
	s := New(cfg, r)
	s.ctx = context.Background()
	s.mode = ModeAuto
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}}

	if ok, _, err := s.budgets.Reserve("s1:1", budgetDay(time.Now()), 1, 0); err != nil || !ok {
		t.Fatal("could not seed s1:1's one run")
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:1/start", nil)
	req.SetPathValue("id", "s1:1")
	s.handleStart(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "budget") {
		t.Errorf("refusal = %q, want it to mention the execution budget", w.Body.String())
	}
}

// Usage must survive a restart the way a manual pause does (#39): an
// operator-defined ceiling is a decision, not a fact rediscovered fresh.
func TestBudgetUsageSurvivesARestart(t *testing.T) {
	cfg, r, _ := twoSourcesFor(t)
	cfg.Budgets = config.Budgets{MaxRunsPerItem: 2}
	s1 := New(cfg, r)
	s1.ctx = context.Background()
	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}
	if got := s1.claim(item, "working", false); got != claimAccepted {
		t.Fatalf("claim() = %v, want claimAccepted", got)
	}

	s2 := New(cfg, r) // a fresh process, same data dir
	s2.ctx = context.Background()
	if runs, _ := s2.budgets.Usage(item.ID); runs != 1 {
		t.Errorf("usage after restart = %d, want 1", runs)
	}
	if got := s2.claim(item, "working", false); got != claimAccepted {
		t.Fatalf("claim() after restart = %v, want claimAccepted (ceiling is 2, one run already spent)", got)
	}
	s2.working.Delete(item.ID)
	s2.eng.Locks().Release(item.Source, "working", s2.cfg.ResourcesFor(item.Source, "working")...)
	if got := s2.claim(item, "working", false); got != claimBudgetExhausted {
		t.Errorf("third claim() after restart = %v, want claimBudgetExhausted", got)
	}
}

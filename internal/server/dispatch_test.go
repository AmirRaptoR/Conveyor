package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// slowBoardFor is boardFor with a stage script that holds its slot for a
// moment, so a test can catch two dispatch paths both wanting the same item
// while the first is still in flight.
func slowBoardFor(t *testing.T, sleep string) (*config.Config, *runner.Runner) {
	t.Helper()
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\nsleep "+sleep+"\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
concurrency:
  perSource: 2
  perStage: 2
  global: 2
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
	return cfg, runner.New(filepath.Join(dir, "runs"))
}

// F02: with every capacity above one, a manual start, a second manual start
// and a scheduler wake racing the same item must still produce exactly one
// stage invocation, one active entry at a time, and a clean release of every
// claim taken — not "one runs, the rest are refused by a slot that happens to
// be sized right".
func TestOneClaimIsAtomicAcrossDispatchPaths(t *testing.T) {
	cfg, r := slowBoardFor(t, "0.2")
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "race me"}}

	var maxActive int32
	stop := make(chan struct{})
	var monitor sync.WaitGroup
	monitor.Add(1)
	go func() {
		defer monitor.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			n := 0
			for _, a := range s.activeList() {
				if a.ItemID == "s1:1" {
					n++
				}
			}
			for {
				cur := atomic.LoadInt32(&maxActive)
				if int32(n) <= cur || atomic.CompareAndSwapInt32(&maxActive, cur, int32(n)) {
					break
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	start := make(chan struct{})
	var wg sync.WaitGroup
	doStart := func() {
		defer wg.Done()
		<-start
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/items/s1:1/start", nil)
		req.SetPathValue("id", "s1:1")
		s.handleStart(w, req)
	}
	wg.Add(3)
	go doStart()
	go doStart()
	go func() {
		defer wg.Done()
		<-start
		s.launch(s.ctx)
	}()
	close(start)
	wg.Wait()

	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })
	close(stop)
	monitor.Wait()

	s.mu.RLock()
	stage := s.state.Items[0].Stage
	s.mu.RUnlock()
	if stage != "done" {
		t.Errorf("stage = %q, want done — exactly one run should have happened and routed on", stage)
	}
	if maxActive > 1 {
		t.Errorf("saw %d active entries for s1:1 at once, want at most 1", maxActive)
	}
	if _, running := s.working.Load("s1:1"); running {
		t.Error("working still holds s1:1 after every claimant finished")
	}
	bySrc, byStage, held, _, _, _ := s.eng.Locks().Snapshot()
	if len(bySrc) != 0 || len(byStage) != 0 || held != 0 {
		t.Errorf("locks not fully released: bySource=%v byStage=%v held=%d", bySrc, byStage, held)
	}
}

// A second start for an item already running must say so — not repeat
// whyBusy's slot-holder wording, which would be false when the slot itself is
// far from full and only this item's own claim refuses.
func TestStartRefusesAnItemAlreadyRunning(t *testing.T) {
	cfg, r := slowBoardFor(t, "0.3")
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}}

	w1 := httptest.NewRecorder()
	req1 := httptest.NewRequest("POST", "/api/items/s1:1/start", nil)
	req1.SetPathValue("id", "s1:1")
	s.handleStart(w1, req1)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first start status = %d (%s), want 202", w1.Code, w1.Body.String())
	}

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/items/s1:1/start", nil)
	req2.SetPathValue("id", "s1:1")
	s.handleStart(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second start status = %d, want 409", w2.Code)
	}
	body := w2.Body.String()
	if !strings.Contains(body, "already running") || !strings.Contains(body, "working") {
		t.Errorf("refusal = %q, want it to say s1:1 is already running in working", body)
	}
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })
}

// F02 + the pause feature (CLAUDE.md "the tick button still overrides"): a
// manual start meets a paused agent exactly like a busy slot, but the tick
// button dispatches anyway.
func TestAdvanceOverridesAPausedAgentButStartDoesNot(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "runs on the agent"}}
	s.pauseFor("fake", time.Time{}, "out of quota", false)

	if !s.advance(s.ctx) {
		t.Fatal("advance refused to dispatch while the agent was paused; the tick button must override")
	}
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })
	s.mu.RLock()
	stage := s.state.Items[0].Stage
	s.mu.RUnlock()
	if stage != "done" {
		t.Errorf("stage = %q, want done — the tick button must dispatch even to a paused agent", stage)
	}

	// The same agent, still paused: a manual start meets the wall the tick
	// button was allowed to ignore.
	s.state.Items = []model.Item{{ID: "s1:2", Ref: "2", Source: "s1", Stage: "backlog", Title: "runs on the agent"}}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:2/start", nil)
	req.SetPathValue("id", "s1:2")
	s.handleStart(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — a manual start must not override a paused agent", w.Code)
	}
	if !strings.Contains(w.Body.String(), "fake") {
		t.Errorf("refusal = %q, want it to name the paused agent", w.Body.String())
	}
}

// F02: claim's own atomicity. A refusal must roll back exactly what it
// already took — nothing more, nothing less — whether it is refused because
// the item is already claimed or because the (source, stage) slot has none
// free, checked directly against Locks().Snapshot() and a working probe
// rather than inferred from a whole dispatch's end state.
func TestClaimRollsBackExactlyWhatItTook(t *testing.T) {
	cfg, r := slowBoardFor(t, "0.1")
	s := New(cfg, r)
	s.ctx = context.Background()
	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}

	// Exhaust the (source, stage) slot directly (capacity 2), bypassing
	// claim, so the claim under test is refused only on the slot.
	if !s.eng.Locks().TryAcquire("s1", "working") || !s.eng.Locks().TryAcquire("s1", "working") {
		t.Fatal("could not exhaust the slot ahead of the claim under test")
	}
	beforeSrc, beforeStage, beforeGlobal, _, _, _ := s.eng.Locks().Snapshot()

	if got := s.claim(item, "working", false); got != claimSlotBusy {
		t.Fatalf("claim() = %v, want claimSlotBusy", got)
	}
	if _, held := s.working.Load(item.ID); held {
		t.Error("working still holds the item after a slot refusal — the item claim was not rolled back")
	}
	afterSrc, afterStage, afterGlobal, _, _, _ := s.eng.Locks().Snapshot()
	if afterSrc["s1"] != beforeSrc["s1"] || afterStage["working"] != beforeStage["working"] || afterGlobal != beforeGlobal {
		t.Errorf("locks changed across a refused claim: before src=%d stage=%d global=%d, after src=%d stage=%d global=%d",
			beforeSrc["s1"], beforeStage["working"], beforeGlobal, afterSrc["s1"], afterStage["working"], afterGlobal)
	}
	s.eng.Locks().Release("s1", "working")
	s.eng.Locks().Release("s1", "working")

	// The item already claimed: the slot must never even be touched, so a
	// second refusal reason must leave Locks completely untouched too.
	s.working.Store(item.ID, struct{}{})
	beforeSrc2, beforeStage2, beforeGlobal2, _, _, _ := s.eng.Locks().Snapshot()
	if got := s.claim(item, "working", false); got != claimItemBusy {
		t.Fatalf("claim() = %v, want claimItemBusy", got)
	}
	afterSrc2, afterStage2, afterGlobal2, _, _, _ := s.eng.Locks().Snapshot()
	if afterSrc2["s1"] != beforeSrc2["s1"] || afterStage2["working"] != beforeStage2["working"] || afterGlobal2 != beforeGlobal2 {
		t.Error("claim() acquired the slot even though the item claim was already refused")
	}
	s.working.Delete(item.ID)
}

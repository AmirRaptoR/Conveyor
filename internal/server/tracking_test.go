package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
)

func TestTrackingReconciliationMarksInvalidOnceAndClearsItWhenRepaired(t *testing.T) {
	cfg, r := boardFor(t)
	moves := cfg.DataDir() + "/tracking-moves"
	writeScript(t, cfg.Sources[0].Move, "#!/bin/sh\ncat >/dev/null\necho x >>\""+moves+"\"\n")
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{
		ID: "s1:10", Ref: "10", Source: "s1", Stage: "backlog", Title: "tracker", Tracking: true,
	}}

	if n := s.reconcileTracking(s.ctx); n != 1 {
		t.Fatalf("first reconciliation wrote %d changes, want 1", n)
	}
	if n := s.reconcileTracking(s.ctx); n != 0 {
		t.Fatalf("unchanged invalid tracker rewrote %d marks, want 0", n)
	}
	s.mu.RLock()
	marked := s.state.Items[0]
	block := s.blocks[marked.ID]
	s.mu.RUnlock()
	if !marked.Blocked || marked.BlockKind != trackingKind || block.Kind != trackingKind || block.Reason == "" {
		t.Fatalf("invalid tracker state = %+v block = %+v", marked, block)
	}

	s.mu.Lock()
	s.state.Items[0].Children = []string{"s1:99"}
	s.mu.Unlock()
	if n := s.reconcileTracking(s.ctx); n != 1 {
		t.Fatalf("changed invalid reason wrote %d changes, want one update", n)
	}
	if n := s.reconcileTracking(s.ctx); n != 0 {
		t.Fatalf("unchanged updated reason rewrote %d marks, want 0", n)
	}

	s.mu.Lock()
	s.state.Items[0].Children = []string{"s1:11"}
	s.state.Items = append(s.state.Items, model.Item{
		ID: "s1:11", Ref: "11", Source: "s1", Stage: "done", Title: "child", Parent: "s1:10",
	})
	s.mu.Unlock()
	if n := s.reconcileTracking(s.ctx); n != 1 {
		t.Fatalf("repair reconciliation wrote %d changes, want one clear", n)
	}
	s.mu.RLock()
	repaired := s.state.Items[0]
	_, stillBlocked := s.blocks[repaired.ID]
	items := append([]model.Item(nil), s.state.Items...)
	s.mu.RUnlock()
	if repaired.Blocked || repaired.BlockKind != "" || stillBlocked {
		t.Fatalf("repaired tracker remained marked: %+v block=%v", repaired, stillBlocked)
	}
	deps := pipeline.NewDeps(cfg, items)
	if target, ok := pipeline.Target(cfg, &items[0], deps); !ok || target != "done" {
		t.Fatalf("repaired complete tracker target = %q, %v; want done", target, ok)
	}
	if got := countLines(moves); got != 3 {
		t.Fatalf("provider received %d writes, want one mark, one update and one clear", got)
	}
}

func TestTrackingReconciliationMigratesOnlyLifecycleOwnedMarks(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{
		{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Tracking: true, Children: []string{"s1:11"}, Blocked: true},
		{ID: "s1:2", Ref: "2", Source: "s1", Stage: "backlog", Tracking: true, Children: []string{"s1:12"}, Blocked: true},
		{ID: "s1:3", Ref: "3", Source: "s1", Stage: "backlog", Tracking: true, Children: []string{"s1:13"}, Blocked: true},
		{ID: "s1:4", Ref: "4", Source: "s1", Stage: "backlog", Tracking: true, Children: []string{"s1:14"}, Blocked: true},
		{ID: "s1:11", Ref: "11", Source: "s1", Stage: "done", Parent: "s1:1"},
		{ID: "s1:12", Ref: "12", Source: "s1", Stage: "done", Parent: "s1:2"},
		{ID: "s1:13", Ref: "13", Source: "s1", Stage: "done", Parent: "s1:3"},
		{ID: "s1:14", Ref: "14", Source: "s1", Stage: "done", Parent: "s1:4"},
	}
	s.blocks = map[string]Block{
		"s1:1": {Kind: noOutputKind, Reason: "tracker made no pull request"},
		"s1:2": {Kind: trackingKind, Reason: "old lifecycle error"},
		"s1:3": {Kind: "decision", Reason: "unrelated question", Asked: true},
		"s1:4": {Kind: "error", Reason: "unrelated fault"},
	}

	if n := s.reconcileTracking(s.ctx); n != 2 {
		t.Fatalf("reconciliation changed %d marks, want legacy and tracking only", n)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range []string{"s1:1", "s1:2"} {
		if _, found := s.blocks[id]; found {
			t.Errorf("lifecycle-owned mark %s survived", id)
		}
	}
	for _, id := range []string{"s1:3", "s1:4"} {
		if _, found := s.blocks[id]; !found {
			t.Errorf("unrelated mark %s was cleared", id)
		}
	}
}

func TestTrackingReconciliationNeverWritesFromObserveOrStaleState(t *testing.T) {
	cfg, r := boardFor(t)
	moves := cfg.DataDir() + "/tracking-moves"
	writeScript(t, cfg.Sources[0].Move, "#!/bin/sh\ncat >/dev/null\necho x >>\""+moves+"\"\n")
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:10", Ref: "10", Source: "s1", Stage: "backlog", Tracking: true}}

	s.mode = ModeObserve
	if n := s.reconcileTracking(s.ctx); n != 0 {
		t.Fatalf("observe mode wrote %d tracking changes", n)
	}
	s.mode = ModeAuto
	s.listErr["s1"] = "listing unavailable"
	if n := s.reconcileTracking(s.ctx); n != 0 {
		t.Fatalf("stale source wrote %d tracking changes", n)
	}
	if b, err := os.ReadFile(moves); err == nil && strings.TrimSpace(string(b)) != "" {
		t.Fatalf("provider was called despite guards: %q", b)
	}
}

func TestTrackingReconciliationClearsItsMarkWhenOptOutIsRemoved(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{
		ID: "s1:10", Ref: "10", Source: "s1", Stage: "backlog", Title: "ordinary again", Blocked: true,
	}}
	s.blocks = map[string]Block{"s1:10": {Kind: trackingKind, Reason: "declares no children"}}
	if n := s.reconcileTracking(s.ctx); n != 1 {
		t.Fatalf("opt-out reconciliation changed %d marks, want 1", n)
	}
	if s.state.Items[0].Blocked {
		t.Fatal("tracking mark survived after explicit tracking opt-in was removed")
	}
}

func TestTerminalTrackingContradictionIsVisibleWithoutRepeatedProviderWrites(t *testing.T) {
	cfg, r := boardFor(t)
	moves := cfg.DataDir() + "/tracking-moves"
	writeScript(t, cfg.Sources[0].Move, "#!/bin/sh\ncat >/dev/null\necho x >>\""+moves+"\"\n")
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{
		{ID: "s1:10", Ref: "10", Source: "s1", Stage: "done", Title: "tracker", Tracking: true, Children: []string{"s1:11"}},
		{ID: "s1:11", Ref: "11", Source: "s1", Stage: "backlog", Title: "child", Parent: "s1:10"},
	}
	if n := s.reconcileTracking(s.ctx); n != 0 {
		t.Fatalf("terminal contradiction wrote %d provider changes, want 0", n)
	}
	if b, err := os.ReadFile(moves); err == nil && strings.TrimSpace(string(b)) != "" {
		t.Fatalf("terminal contradiction wrote provider state: %q", b)
	}

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var got State
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	tracked := got.Tracking["s1:10"]
	if tracked.State != pipeline.TrackingInvalid || !strings.Contains(tracked.Reason, "unfinished") {
		t.Fatalf("tracking state = %+v, want visible terminal contradiction", tracked)
	}
}

func TestSettledOpenTrackerClosesOnlyWithLifecycleProof(t *testing.T) {
	cfg, r := boardFor(t)
	payload := cfg.DataDir() + "/tracking-payload"
	writeScript(t, cfg.Sources[0].Move, "#!/bin/sh\ncat >\""+payload+"\"\n")
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{
		{ID: "s1:10", Ref: "10", Source: "s1", Stage: "done", Title: "tracker", Tracking: true, Children: []string{"s1:11"}, Blocked: true, BlockKind: statusKind},
		{ID: "s1:11", Ref: "11", Source: "s1", Stage: "done", Title: "child", Parent: "s1:10"},
	}
	s.blocks = map[string]Block{"s1:10": {Kind: statusKind, Reason: "open in terminal stage"}}
	if n := s.reconcileTracking(s.ctx); n != 1 {
		t.Fatalf("settled tracker reconciliation changed %d items, want 1", n)
	}
	b, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	var input model.StageInput
	if err := json.Unmarshal(b, &input); err != nil {
		t.Fatal(err)
	}
	if !input.Terminal || !input.TrackingComplete || input.Blocked {
		t.Fatalf("settled tracker move = %+v, want explicit unmarked completion", input)
	}
	if s.state.Items[0].Blocked {
		t.Fatal("settled tracker remained marked after completion")
	}
}

func TestTrackingReconciliationSkipsAWorkingTracker(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:10", Ref: "10", Source: "s1", Stage: "backlog", Tracking: true}}
	s.working.Store("s1:10", struct{}{})
	defer s.working.Delete("s1:10")
	if n := s.reconcileTracking(s.ctx); n != 0 {
		t.Fatalf("working tracker reconciliation wrote %d changes", n)
	}
	if s.state.Items[0].Blocked {
		t.Fatal("working tracker was marked from a concurrent lifecycle snapshot")
	}
}

func TestFreshProviderMarkReplacesCachedTrackingOwnership(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	items := []model.Item{{
		ID: "s1:10", Ref: "10", Source: "s1", Stage: "backlog", Tracking: true,
		Blocked: true, BlockKind: "decision", BlockReason: "a person must choose",
	}}
	s.blocks = map[string]Block{"s1:10": {Kind: trackingKind, Reason: "old lifecycle mark"}}
	s.recallBlocks(items)
	s.state.Items = items
	if block := s.blocks["s1:10"]; block.Kind != "decision" || block.Reason != "a person must choose" {
		t.Fatalf("fresh provider mark did not replace cached ownership: %+v", block)
	}
	if n := s.reconcileTracking(s.ctx); n != 0 {
		t.Fatalf("reconciliation overwrote a fresh decision with %d writes", n)
	}
}

func TestRefreshReconcilesAnInvalidTrackingItem(t *testing.T) {
	cfg, r := boardFor(t)
	writeScript(t, cfg.Sources[0].List, `#!/bin/sh
cat >"$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:10","ref":"10","source":"s1","stage":"backlog","title":"tracker","tracking":true}]
JSON
`)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.refresh(s.ctx)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.state.Items) != 1 || !s.state.Items[0].Blocked {
		t.Fatalf("fresh invalid tracker was not marked: %+v", s.state.Items)
	}
	if block := s.blocks["s1:10"]; block.Kind != trackingKind || block.Reason == "" {
		t.Fatalf("tracking block = %+v, want stable lifecycle reason", block)
	}
}

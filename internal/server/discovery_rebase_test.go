package server

import (
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func TestLateDiscoveryRebaseKeepsTransitionCompletedBeforePublish(t *testing.T) {
	gen := time.Now()
	current := model.Item{
		ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Title: "item",
		Blocked: true, BlockKind: "decision", BlockReason: "needs a person",
	}
	s := &Server{
		state:       State{Items: []model.Item{current}},
		sourceGen:   map[string]time.Time{"s1": gen},
		confirmedAt: map[string]time.Time{"s1:1": gen.Add(time.Second)},
	}
	stale := []model.Item{{
		ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Title: "item",
	}}

	s.mu.Lock()
	got := s.rebaseLateTransitionsLocked(stale)
	s.mu.Unlock()
	if len(got) != 1 || !got[0].Blocked || got[0].BlockKind != "decision" || got[0].BlockReason != "needs a person" {
		t.Fatalf("late rebase = %+v, want the newer blocked transition outcome", got)
	}
}

func TestLateDiscoveryRebaseTrustsListingStartedAfterTransition(t *testing.T) {
	confirmed := time.Now()
	current := model.Item{
		ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Title: "item",
		Blocked: true, BlockKind: "decision", BlockReason: "old question",
	}
	s := &Server{
		state:       State{Items: []model.Item{current}},
		sourceGen:   map[string]time.Time{"s1": confirmed.Add(time.Second)},
		confirmedAt: map[string]time.Time{"s1:1": confirmed},
	}
	fresh := []model.Item{{
		ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Title: "item",
	}}

	s.mu.Lock()
	got := s.rebaseLateTransitionsLocked(fresh)
	s.mu.Unlock()
	if len(got) != 1 || got[0].Blocked || got[0].BlockKind != "" || got[0].BlockReason != "" {
		t.Fatalf("late rebase = %+v, want the newer provider listing", got)
	}
}

func TestLateDiscoveryRebaseKeepsWorkingItemOmittedByListing(t *testing.T) {
	gen := time.Now()
	current := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Title: "item"}
	s := &Server{
		state:       State{Items: []model.Item{current}},
		sourceGen:   map[string]time.Time{"s1": gen},
		confirmedAt: map[string]time.Time{},
	}
	s.working.Store(current.ID, struct{}{})

	s.mu.Lock()
	got := s.rebaseLateTransitionsLocked(nil)
	s.mu.Unlock()
	if len(got) != 1 || got[0].ID != current.ID {
		t.Fatalf("late rebase = %+v, want the in-flight item retained", got)
	}
}

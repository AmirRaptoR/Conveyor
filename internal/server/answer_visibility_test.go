package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// F04: /api/state must expose, per item, whether an unspent answer is held
// and when it was recorded, and — once a run has actually received it — that
// run's id, never before.
func TestAnswerHeldAndConsumedByAreVisibleOnTheBoard(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Reason: "needs a decision", Stage: "working"}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/items/s1:1/unblock", strings.NewReader(`{"answer":"go ahead"}`))
	req.SetPathValue("id", "s1:1")
	s.handleUnblock(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("unblock status = %d (%s), want 204", w.Code, w.Body.String())
	}

	s.mu.RLock()
	info, held := s.answerInfo["s1:1"]
	s.mu.RUnlock()
	if !held {
		t.Fatal("no answer recorded for s1:1 after answering it")
	}
	if info.RecordedAt.IsZero() {
		t.Error("RecordedAt is zero")
	}
	if info.ConsumedBy != "" {
		t.Errorf("ConsumedBy = %q before any run received it, want empty", info.ConsumedBy)
	}

	// The item is unmarked now and sits in "working" with a script; running
	// its transition is what actually hands the answer to a run.
	if !s.advance(s.ctx) {
		t.Fatal("advance did not dispatch the item holding the answer")
	}
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })

	s.mu.RLock()
	info, held = s.answerInfo["s1:1"]
	s.mu.RUnlock()
	if !held {
		t.Fatal("answer info vanished after the run that consumed it")
	}
	if info.ConsumedBy == "" {
		t.Error("ConsumedBy is still empty after a run received the answer")
	}

	runs, err := s.listRuns("s1:1", 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, run := range runs {
		if run.ID == info.ConsumedBy {
			found = true
		}
	}
	if !found {
		t.Errorf("ConsumedBy = %q does not name any of this item's actual runs %v", info.ConsumedBy, runs)
	}
}

// Pruned the same way Times is: an item that leaves the board takes its
// answer-visibility note with it.
func TestAnswerVisibilityIsPrunedWhenTheItemLeavesTheBoard(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.answerInfo["s1:9"] = AnswerView{}
	s.state.Items = nil // s1:9 is not on the board at all
	s.refresh(s.ctx)

	s.mu.RLock()
	_, held := s.answerInfo["s1:9"]
	s.mu.RUnlock()
	if held {
		t.Error("answer info for an off-board item survived a refresh")
	}
}

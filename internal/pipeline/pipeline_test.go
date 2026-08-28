package pipeline

import (
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// Stages can form a cycle: a review that rejects sends work back to be
// reimplemented, and the implementing stage succeeds on every pass round. With
// attempts counted per item alone, that success cleared the reviewing stage's
// tally, maxAttempts never fired, and two models could disagree forever.
func TestMaxAttemptsBoundsAReworkLoop(t *testing.T) {
	impl := &config.Stage{Name: "in-progress", Script: "implement", OnSuccess: "testing", OnFailure: "blocked"}
	review := &config.Stage{
		Name: "testing", Script: "review",
		OnSuccess: "done", OnFailure: "in-progress", OnBlocked: "blocked", MaxAttempts: 3,
	}
	e := New(&config.Config{}, nil)
	const item = "x:1"

	var last string
	for i := 1; i <= 3; i++ {
		last = e.route(review, model.OutcomeFailure, item, &Transition{})
		if i < 3 {
			if last != "in-progress" {
				t.Fatalf("round %d: review sent the item to %q, want in-progress", i, last)
			}
			// The rework pass succeeds — and must not reset the review's count.
			if got := e.route(impl, model.OutcomeSuccess, item, &Transition{}); got != "testing" {
				t.Fatalf("round %d: implement sent the item to %q, want testing", i, got)
			}
		}
	}
	if last != "blocked" {
		t.Fatalf("after %d rejections the item went to %q, want blocked", review.MaxAttempts, last)
	}
}

// Passing review resets that stage's own count, so a later rework starts fresh.
func TestSuccessClearsItsOwnStage(t *testing.T) {
	review := &config.Stage{Name: "testing", OnSuccess: "done", OnFailure: "in-progress", OnBlocked: "blocked", MaxAttempts: 2}
	e := New(&config.Config{}, nil)

	if got := e.route(review, model.OutcomeFailure, "x:1", &Transition{}); got != "in-progress" {
		t.Fatalf("first rejection went to %q, want in-progress", got)
	}
	e.route(review, model.OutcomeSuccess, "x:1", &Transition{})
	// Counter cleared, so the next rejection is attempt 1 again, not the limit.
	if got := e.route(review, model.OutcomeFailure, "x:1", &Transition{}); got != "in-progress" {
		t.Fatalf("after a pass, the next rejection went to %q, want in-progress", got)
	}
}

// One item hitting its limit must not spend another item's budget.
func TestAttemptsArePerItem(t *testing.T) {
	s := &config.Stage{Name: "testing", OnFailure: "in-progress", OnBlocked: "blocked", MaxAttempts: 2}
	e := New(&config.Config{}, nil)

	e.route(s, model.OutcomeFailure, "x:1", &Transition{})
	if got := e.route(s, model.OutcomeFailure, "x:1", &Transition{}); got != "blocked" {
		t.Fatalf("x:1 second failure went to %q, want blocked", got)
	}
	if got := e.route(s, model.OutcomeFailure, "x:2", &Transition{}); got != "in-progress" {
		t.Fatalf("x:2 first failure went to %q, want in-progress", got)
	}
}

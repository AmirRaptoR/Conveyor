package pipeline

import (
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/source"
)

// ran is a finished run with the given outcome, as route reads it.
func ran(o model.Outcome, code int) *runner.Result {
	return &runner.Result{Run: model.Run{Outcome: o, ExitCode: code}}
}

// Stages can form a cycle: a review that rejects sends work back to be
// reimplemented, and the implementing stage succeeds on every pass round. With
// attempts counted per item alone, that success cleared the reviewing stage's
// tally, maxAttempts never fired, and two models could disagree forever.
func TestMaxAttemptsBoundsAReworkLoop(t *testing.T) {
	impl := &config.Stage{Name: "in-progress", Script: "implement", OnSuccess: "testing"}
	review := &config.Stage{Name: "testing", Script: "review", OnSuccess: "done", MaxAttempts: 3}
	e := New(&config.Config{}, nil)
	const item = "x:1"

	var mark source.Mark
	for i := 1; i <= 3; i++ {
		_, mark = e.route(review, ran(model.OutcomeFailure, 1), item, &Transition{})
		if i < 3 {
			if mark.Blocked {
				t.Fatalf("round %d: review marked the item on attempt %d of 3", i, i)
			}
			// The rework pass succeeds — and must not reset the review's count.
			if next, _ := e.route(impl, ran(model.OutcomeSuccess, 0), item, &Transition{}); next != "testing" {
				t.Fatalf("round %d: implement sent the item to %q, want testing", i, next)
			}
		}
	}
	if !mark.Blocked {
		t.Fatalf("after %d rejections the item was not marked", review.MaxAttempts)
	}
}

// Passing review resets that stage's own count, so a later rework starts fresh.
func TestSuccessClearsItsOwnStage(t *testing.T) {
	review := &config.Stage{Name: "testing", OnSuccess: "done", MaxAttempts: 2}
	e := New(&config.Config{}, nil)

	if _, mark := e.route(review, ran(model.OutcomeFailure, 1), "x:1", &Transition{}); mark.Blocked {
		t.Fatal("the first rejection of two marked the item")
	}
	e.route(review, ran(model.OutcomeSuccess, 0), "x:1", &Transition{})
	// Counter cleared, so the next rejection is attempt 1 again, not the limit.
	if _, mark := e.route(review, ran(model.OutcomeFailure, 1), "x:1", &Transition{}); mark.Blocked {
		t.Fatal("after a pass, the next rejection marked the item on attempt 1")
	}
}

// One item hitting its limit must not spend another item's budget.
func TestAttemptsArePerItem(t *testing.T) {
	s := &config.Stage{Name: "testing", OnSuccess: "done", MaxAttempts: 2}
	e := New(&config.Config{}, nil)

	e.route(s, ran(model.OutcomeFailure, 1), "x:1", &Transition{})
	if _, mark := e.route(s, ran(model.OutcomeFailure, 1), "x:1", &Transition{}); !mark.Blocked {
		t.Fatal("x:1 was not marked on its second failure")
	}
	if _, mark := e.route(s, ran(model.OutcomeFailure, 1), "x:2", &Transition{}); mark.Blocked {
		t.Fatal("x:2 was marked on its first failure; it has a budget of its own")
	}
}

// Unset maxAttempts means one. A failure that neither routed nor marked would
// be re-run on every poll forever, which is what the old onFailure prevented.
func TestOneAttemptByDefault(t *testing.T) {
	s := &config.Stage{Name: "deploying", OnSuccess: "done"}
	e := New(&config.Config{}, nil)
	next, mark := e.route(s, ran(model.OutcomeFailure, 2), "x:1", &Transition{})
	if !mark.Blocked {
		t.Error("the first failure of a stage with no maxAttempts did not mark the item")
	}
	if next != "" {
		t.Errorf("a marked item was sent to %q; it stays where it stopped", next)
	}
}

// Exit 20 is a decision, not a fault: no retry could change the answer, so it
// marks immediately however much budget the stage has.
func TestBlockedMarksImmediately(t *testing.T) {
	s := &config.Stage{Name: "refining", OnSuccess: "ready", MaxAttempts: 5}
	e := New(&config.Config{}, nil)
	res := ran(model.OutcomeBlocked, 20)
	res.Data = []byte(`{"blocked":true,"reason":"needs a product call"}`)

	next, mark := e.route(s, res, "x:1", &Transition{})
	if !mark.Blocked || next != "" {
		t.Fatalf("route = (%q, %+v), want marked in place", next, mark)
	}
	if mark.Reason != "needs a product call" {
		t.Errorf("reason = %q, want the one the script wrote", mark.Reason)
	}
}

// The reason is a convention, not a requirement: a script that just exits 20
// still blocks, and the mark still says something useful.
func TestBlockedWithoutAReason(t *testing.T) {
	e := New(&config.Config{}, nil)
	_, mark := e.route(&config.Stage{Name: "refining", OnSuccess: "ready"},
		ran(model.OutcomeBlocked, 20), "x:1", &Transition{})
	if !mark.Blocked || mark.Reason == "" {
		t.Fatalf("mark = %+v, want blocked with some reason", mark)
	}
}

// The mark is what keeps the scheduler off an item. Without this the stage it
// stopped in is a stage it is sitting in, which reads as an interrupted job and
// is re-run on every single poll.
func TestAMarkedItemIsNeverPicked(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "refining"},
		{Name: "refining", Script: "refine", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	held := model.Item{ID: "a:1", Source: "a", Stage: "refining", Title: "needs a human", Blocked: true}
	if _, ok := Target(cfg, &held); ok {
		t.Error("a marked item was given a target")
	}
	if got, _ := Pick(cfg, []model.Item{held}, nil); got != nil {
		t.Errorf("picked %v, want nothing — it is waiting for a person", got)
	}

	// And clearing the mark hands the job straight back, where it stopped.
	held.Blocked = false
	got, target := Pick(cfg, []model.Item{held}, nil)
	if got == nil || target != "refining" {
		t.Fatalf("after unblocking: picked %v for %q, want a:1 re-run in refining", got, target)
	}
}

// An item found inside a stage that runs a script is an unfinished job: the
// provider already says it is there. It must be picked up before new work,
// however the queue is arranged, or it sits wearing a status it is not in.
func TestRecoveryOutranksTheOrderedBacklog(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "refining"},
		{Name: "refining", Script: "refine", OnSuccess: "ready"},
		{Name: "ready", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "backlog", Title: "top of the queue"},
		{ID: "a:2", Source: "a", Stage: "refining", Title: "interrupted mid-stage"},
	}
	// a:1 is first in the manual order and would otherwise win outright.
	got, target := Pick(cfg, items, []string{"a:1", "a:2"})
	if got == nil || got.ID != "a:2" {
		t.Fatalf("picked %v, want the interrupted a:2", got)
	}
	if target != "refining" {
		t.Errorf("target = %q, want refining (a re-run, not a move)", target)
	}
}

// With nothing to recover, the manual order decides as before.
func TestOrderStillDecidesWithoutRecovery(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "refining"},
		{Name: "refining", Script: "refine", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	p0 := 0
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "backlog", Title: "dragged to the top"},
		{ID: "a:2", Source: "a", Stage: "backlog", Title: "urgent but lower", Priority: &p0},
	}
	got, _ := Pick(cfg, items, []string{"a:1", "a:2"})
	if got == nil || got.ID != "a:1" {
		t.Fatalf("picked %v, want a:1 — the order beats priority", got)
	}
}

// The board draws items in the order the server hands them over, so that order
// has to be the order the scheduler would reach them in — one Pick after
// another. A card sitting above another it will be worked after is a board that
// lies about what happens next.
func TestOrderIsRepeatedPick(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "refining"},
		{Name: "refining", Script: "refine", OnSuccess: "ready"},
		{Name: "ready", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	p0, p2 := 0, 2
	items := []model.Item{
		{ID: "a:32", Source: "a", Stage: "backlog", Title: "listed first", Priority: &p2},
		{ID: "a:109", Source: "a", Stage: "backlog", Title: "listed second", Priority: &p2},
		{ID: "b:7", Source: "b", Stage: "backlog", Title: "urgent", Priority: &p0},
		{ID: "b:8", Source: "b", Stage: "refining", Title: "interrupted"},
		{ID: "a:4", Source: "a", Stage: "ready", Title: "dragged to the top"},
	}
	order := []string{"a:4"}

	// Repeated Pick, removing each winner: the order the scheduler works them.
	var want []string
	rest := append([]model.Item(nil), items...)
	for len(rest) > 0 {
		got, _ := Pick(cfg, rest, order)
		if got == nil {
			break
		}
		want = append(want, got.ID)
		out := rest[:0]
		for _, it := range rest {
			if it.ID != want[len(want)-1] {
				out = append(out, it)
			}
		}
		rest = out
	}

	sorted := Order(cfg, items, order)
	var got []string
	for _, it := range sorted[:len(want)] {
		got = append(got, it.ID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Order = %v, want %v (the sequence Pick produces)", got, want)
	}
	if len(sorted) != len(items) {
		t.Errorf("Order returned %d items, want all %d", len(sorted), len(items))
	}
}

// Order carries everything, including what is not in the queue at all. Those
// go to the back in listing order: they make no claim on a place in it.
func TestUnworkableItemsKeepListingOrderAtTheBack(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "refining"},
		{Name: "refining", Script: "refine", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "done", Title: "finished"},
		{ID: "a:2", Source: "a", Stage: "refining", Title: "waiting for a human", Blocked: true},
		{ID: "a:3", Source: "a", Stage: "backlog", Title: "workable"},
	}
	got := Order(cfg, items, nil)
	if got[0].ID != "a:3" {
		t.Errorf("first = %q, want a:3 — the only one with anywhere to go", got[0].ID)
	}
	if got[1].ID != "a:1" || got[2].ID != "a:2" {
		t.Errorf("tail = %q,%q, want a:1,a:2 in listing order", got[1].ID, got[2].ID)
	}
}

// Order must not disturb the caller's slice: the server sorts on the way out
// while the scheduler is reading the same listing.
func TestOrderLeavesTheListingAlone(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	p0 := 0
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "backlog"},
		{ID: "a:2", Source: "a", Stage: "backlog", Priority: &p0},
	}
	Order(cfg, items, nil)
	if items[0].ID != "a:1" || items[1].ID != "a:2" {
		t.Errorf("listing was reordered in place: %q,%q", items[0].ID, items[1].ID)
	}
}

package pipeline

import (
	"strings"
	"testing"
	"time"

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
	if _, ok := Target(cfg, &held, Deps{}); ok {
		t.Error("a marked item was given a target")
	}
	if got, _ := Pick(cfg, []model.Item{held}, nil, Deps{}); got != nil {
		t.Errorf("picked %v, want nothing — it is waiting for a person", got)
	}

	// And clearing the mark hands the job straight back, where it stopped.
	held.Blocked = false
	got, target := Pick(cfg, []model.Item{held}, nil, Deps{})
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
	got, target := Pick(cfg, items, []string{"a:1", "a:2"}, Deps{})
	if got == nil || got.ID != "a:2" {
		t.Fatalf("picked %v, want the interrupted a:2", got)
	}
	if target != "refining" {
		t.Errorf("target = %q, want refining (a re-run, not a move)", target)
	}
}

// The line is worked from its far end backwards: an item two stages from done
// is picked before one that has not started, however the backlog is arranged.
// A pipeline exists to finish items, and a scheduler that always reaches for
// the head of the backlog widens the band of half-finished work instead.
func TestLaterStagesAreWorkedFirst(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "refining"},
		{Name: "refining", Script: "refine", OnSuccess: "ready"},
		{Name: "ready", OnSuccess: "in-progress"},
		{Name: "in-progress", Script: "implement", OnSuccess: "review"},
		{Name: "review", Script: "review", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	p0 := 0
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "backlog", Title: "top of the queue, most urgent", Priority: &p0},
		{ID: "a:2", Source: "a", Stage: "refining", Title: "interrupted early on"},
		{ID: "a:3", Source: "a", Stage: "ready", Title: "waiting to be built"},
		{ID: "a:4", Source: "a", Stage: "in-progress", Title: "nearly reviewable"},
	}
	// a:1 is first in the manual order, priority 0, and listed first: it wins
	// every rung below depth. a:2 is an interrupted run, which used to outrank
	// everything.
	order := []string{"a:1", "a:2", "a:3", "a:4"}

	want := []string{"a:4", "a:3", "a:2", "a:1"}
	var got []string
	for _, it := range Order(cfg, items, order, Deps{}) {
		got = append(got, it.ID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Order = %v, want %v — furthest along first", got, want)
	}

	first, target := Pick(cfg, items, order, Deps{})
	if first == nil || first.ID != "a:4" {
		t.Fatalf("picked %v, want a:4 — the item closest to done", first)
	}
	if target != "in-progress" {
		t.Errorf("target = %q, want in-progress (a re-run of the stage it is in)", target)
	}
}

// Depth is measured on the stage an item is heading INTO, so a queue and the
// stage it feeds are one position in the line, not two. That is what keeps
// recovery meaningful: an interrupted run only ever ties with the queue behind
// it, and it wins that tie — nothing is stranded wearing a status it is not in.
func TestRecoveryWinsAtEqualDepth(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "ready", OnSuccess: "in-progress"},
		{Name: "in-progress", Script: "implement", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "ready", Title: "next up, dragged to the top"},
		{ID: "a:2", Source: "a", Stage: "in-progress", Title: "interrupted mid-stage"},
	}
	got, target := Pick(cfg, items, []string{"a:1", "a:2"}, Deps{})
	if got == nil || got.ID != "a:2" {
		t.Fatalf("picked %v, want the interrupted a:2", got)
	}
	if target != "in-progress" {
		t.Errorf("target = %q, want in-progress (a re-run, not a move)", target)
	}
}

// Within one stage nothing changed: the manual order is still the human lever,
// which is where it belongs — a choice about what to start next, not a way to
// jump work already in flight.
func TestOrderStillDecidesWithinAStage(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "refining"},
		{Name: "refining", Script: "refine", OnSuccess: "ready"},
		{Name: "ready", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	p0 := 0
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "ready", Title: "further along, unordered"},
		{ID: "a:2", Source: "a", Stage: "backlog", Title: "dragged to the top"},
		{ID: "a:3", Source: "a", Stage: "backlog", Title: "urgent but lower", Priority: &p0},
	}
	var got []string
	for _, it := range Order(cfg, items, []string{"a:2", "a:3"}, Deps{}) {
		got = append(got, it.ID)
	}
	if strings.Join(got, ",") != "a:1,a:2,a:3" {
		t.Errorf("Order = %v, want a:1,a:2,a:3 — depth first, then the order inside the stage", got)
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
	got, _ := Pick(cfg, items, []string{"a:1", "a:2"}, Deps{})
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
		got, _ := Pick(cfg, rest, order, Deps{})
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

	sorted := Order(cfg, items, order, Deps{})
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
	got := Order(cfg, items, nil, Deps{})
	if got[0].ID != "a:3" {
		t.Errorf("first = %q, want a:3 — the only one with anywhere to go", got[0].ID)
	}
	if got[1].ID != "a:1" || got[2].ID != "a:2" {
		t.Errorf("tail = %q,%q, want a:1,a:2 in listing order", got[1].ID, got[2].ID)
	}
}

// The done column has to read newest-finished first. Before this, a terminal
// item still climbed the scheduling rungs meant for workable items — the
// manual order named it once, dragged there while it was still in flight, and
// it never came back down after it finished.
func TestTerminalItemsOrderByFinishedAtNotManualOrder(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "refining"},
		{Name: "refining", Script: "refine", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	items := []model.Item{
		{ID: "a:8", Source: "a", Stage: "done", Title: "closed 2026-09-01", FinishedAt: "2026-09-01T00:00:00Z"},
		{ID: "a:21", Source: "a", Stage: "done", Title: "closed 2026-09-05", FinishedAt: "2026-09-05T00:00:00Z"},
		{ID: "a:14", Source: "a", Stage: "done", Title: "closed 2026-09-03", FinishedAt: "2026-09-03T00:00:00Z"},
	}
	// a:8 finished first of the three but is the one named in the persisted
	// manual order — exactly the drag that used to hoist it to the top.
	var got []string
	for _, it := range Order(cfg, items, []string{"a:8"}, Deps{}) {
		got = append(got, it.ID)
	}
	want := []string{"a:21", "a:14", "a:8"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Order = %v, want %v — newest-finished first, manual order ignored in a terminal stage", got, want)
	}
}

// Priority is a scheduling rung too, and a terminal item is not scheduled.
func TestTerminalItemsIgnorePriority(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{{Name: "done", Terminal: true}}}
	p0 := 0
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "done", Title: "urgent but stale", Priority: &p0, FinishedAt: "2026-09-01T00:00:00Z"},
		{ID: "a:2", Source: "a", Stage: "done", Title: "unranked but recent", FinishedAt: "2026-09-05T00:00:00Z"},
	}
	got := Order(cfg, items, nil, Deps{})
	if got[0].ID != "a:2" {
		t.Errorf("first = %q, want a:2 — finished more recently; priority 0 does not outrank it", got[0].ID)
	}
}

// Server.refresh concatenates each source's own listing in config order, so
// nothing but Order merges finished work across repositories into one ledger.
func TestTerminalItemsMergeAcrossSources(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{{Name: "done", Terminal: true}}}
	// Each source's own newest-first block, one after another — the shape
	// Server.refresh builds.
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "done", FinishedAt: "2026-09-05T00:00:00Z"},
		{ID: "a:2", Source: "a", Stage: "done", FinishedAt: "2026-09-03T00:00:00Z"},
		{ID: "b:1", Source: "b", Stage: "done", FinishedAt: "2026-09-04T00:00:00Z"},
		{ID: "b:2", Source: "b", Stage: "done", FinishedAt: "2026-09-02T00:00:00Z"},
	}
	var got []string
	for _, it := range Order(cfg, items, nil, Deps{}) {
		got = append(got, it.ID)
	}
	want := []string{"a:1", "b:1", "a:2", "b:2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Order = %v, want %v — interleaved strictly newest-finished first", got, want)
	}
}

// A provider may send nothing, or send something that does not parse: neither
// is an error, and both sort after every terminal item with a usable
// timestamp, keeping their own listing order rather than being reshuffled.
func TestTerminalItemsWithNoUsableFinishSortLast(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{{Name: "done", Terminal: true}}}
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "done", Title: "not RFC 3339", FinishedAt: "yesterday"},
		{ID: "a:2", Source: "a", Stage: "done", Title: "no finishedAt at all"},
		{ID: "a:3", Source: "a", Stage: "done", Title: "has one", FinishedAt: "2026-09-01T00:00:00Z"},
	}
	var got []string
	for _, it := range Order(cfg, items, nil, Deps{}) {
		got = append(got, it.ID)
	}
	want := []string{"a:3", "a:1", "a:2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Order = %v, want %v — unusable timestamps sort last, in listing order", got, want)
	}
}

// Two items finishing at the same instant is not a tie the comparator may
// leave ambiguous: sort.SliceStable needs a strict weak ordering, so it falls
// back to listing order like every other tie below it.
func TestTerminalItemsWithEqualFinishedAtKeepListingOrder(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{{Name: "done", Terminal: true}}}
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "done", FinishedAt: "2026-09-01T00:00:00Z"},
		{ID: "a:2", Source: "a", Stage: "done", FinishedAt: "2026-09-01T00:00:00Z"},
	}
	var got []string
	for _, it := range Order(cfg, items, nil, Deps{}) {
		got = append(got, it.ID)
	}
	want := []string{"a:1", "a:2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Order = %v, want %v — an equal finish time keeps listing order", got, want)
	}
}

// Rung 1 survives untouched: a workable item beats every unworkable one,
// however recently a terminal item finished.
func TestWorkableItemOutranksAFinishedTerminalItem(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "done", Title: "finished just now", FinishedAt: "2026-09-05T00:00:00Z"},
		{ID: "a:2", Source: "a", Stage: "backlog", Title: "not started, no timestamp at all"},
	}
	got, target := Pick(cfg, items, nil, Deps{})
	if got == nil || got.ID != "a:2" {
		t.Fatalf("picked %v, want a:2 — the only workable item", got)
	}
	if target != "done" {
		t.Errorf("target = %q, want done", target)
	}
	if ordered := Order(cfg, items, nil, Deps{}); ordered[0].ID != "a:2" {
		t.Errorf("first = %q, want a:2 — workable outranks a finished terminal item", ordered[0].ID)
	}
}

// The other unworkable items — marked, and resting in a queue with no exit —
// are untouched by this change: they keep the rungs they have today, and sort
// after every terminal item, which is what keeps the doctor sweep's order
// (a second consumer of Order, over marked items only) exactly as it is now.
func TestOtherUnworkableItemsSortAfterTerminalOnes(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "refining"},
		{Name: "refining", Script: "refine", OnSuccess: "ready"},
		{Name: "ready"}, // a queue with no exit: items rest here
		{Name: "done", Terminal: true},
	}}
	items := []model.Item{
		{ID: "a:1", Source: "a", Stage: "ready", Title: "resting, no exit"},
		{ID: "a:2", Source: "a", Stage: "refining", Title: "waiting for a human", Blocked: true},
		{ID: "a:3", Source: "a", Stage: "done", Title: "finished", FinishedAt: "2026-09-01T00:00:00Z"},
	}
	var got []string
	for _, it := range Order(cfg, items, nil, Deps{}) {
		got = append(got, it.ID)
	}
	want := []string{"a:3", "a:1", "a:2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Order = %v, want %v — terminal first, then today's rungs unchanged", got, want)
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
	Order(cfg, items, nil, Deps{})
	if items[0].ID != "a:1" || items[1].ID != "a:2" {
		t.Errorf("listing was reordered in place: %q,%q", items[0].ID, items[1].ID)
	}
}

// The kind is what the board shows and what a provider files as a label, so
// every mark has one — the script's word when it gave one, and the run
// record's when it did not.
func TestEveryMarkHasAKind(t *testing.T) {
	for _, tc := range []struct {
		what string
		run  model.Run
		data string
		kind string
	}{
		{"a bare exit 20 is a decision", model.Run{Outcome: model.OutcomeBlocked, ExitCode: 20}, "", "decision"},
		{"a failure is an error", model.Run{Outcome: model.OutcomeFailure, ExitCode: 1}, "", "error"},
		{"a timeout says so", model.Run{Outcome: model.OutcomeTimeout}, "", "timeout"},
		{"the script's own word wins", model.Run{Outcome: model.OutcomeBlocked, ExitCode: 20},
			`{"blocked":true,"kind":"Limit","reason":"out of quota"}`, "limit"},
		{"a kind that would not fit a label is dropped", model.Run{Outcome: model.OutcomeBlocked, ExitCode: 20},
			`{"blocked":true,"kind":"the agent decided that it could not proceed here","reason":"x"}`, "decision"},
		{"and so is one with punctuation in it", model.Run{Outcome: model.OutcomeFailure, ExitCode: 1},
			`{"blocked":true,"kind":"needs:decision"}`, "error"},
	} {
		got := Marked("refining", tc.run, []byte(tc.data), time.Minute, 0)
		if got.Kind != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.what, got.Kind, tc.kind)
		}
		if got.Reason == "" {
			t.Errorf("%s: no reason at all", tc.what)
		}
		if !got.Blocked {
			t.Errorf("%s: not marked", tc.what)
		}
	}
}

// A timeout is a statement about the work, and the same work costs different
// amounts per repository: caravan-v2's integration suite ran for 15m44s where
// midgame's whole mobile suite takes 38 seconds. One stage number for both is
// either too tight for the first or no guard at all for the second.
func TestASourceMayOverrideAStageTimeout(t *testing.T) {
	stage := &config.Stage{Name: "review", Script: "review", Timeout: config.Duration(35 * time.Minute)}
	slow := config.Source{Name: "caravan-v2", Scripts: map[string]config.ScriptSpec{
		"review": {Agent: "claude", Timeout: config.Duration(75 * time.Minute)},
	}}
	fast := config.Source{Name: "midgame", Scripts: map[string]config.ScriptSpec{
		"review": {Agent: "claude"},
	}}

	if got := timeoutFor(slow, stage); got != 75*time.Minute {
		t.Errorf("caravan-v2 review = %s, want 75m — the source's own number", got)
	}
	if got := timeoutFor(fast, stage); got != 35*time.Minute {
		t.Errorf("midgame review = %s, want the stage's 35m", got)
	}
	// A source that names no script for the stage keeps the stage's number
	// rather than losing its guard to a zero value.
	if got := timeoutFor(config.Source{Name: "other"}, stage); got != 35*time.Minute {
		t.Errorf("unlisted source = %s, want the stage's 35m", got)
	}
}

// An inline `run:` stage exists for the case where every source does the same
// thing, so there is no per-source entry that could carry an override.
func TestAnInlineStageKeepsItsOwnTimeout(t *testing.T) {
	stage := &config.Stage{Name: "merged", Run: "#!/bin/sh\ntrue\n", Timeout: config.Duration(2 * time.Minute)}
	src := config.Source{Name: "s", Scripts: map[string]config.ScriptSpec{
		"": {Timeout: config.Duration(time.Hour)},
	}}
	if got := timeoutFor(src, stage); got != 2*time.Minute {
		t.Errorf("inline stage = %s, want the stage's 2m", got)
	}
}

// The gate belongs at rung 1 — "has anywhere to go at all" — so one statement
// of the rule covers the scheduler, the board's ordering and the drag
// endpoint at once. A held item has nowhere to go, and Target says so.
func TestTargetRefusesAnOvertakingMove(t *testing.T) {
	cfg := line()
	one := model.Item{ID: "s:1", Stage: "ready"}
	two := model.Item{ID: "s:2", Stage: "ready", DependsOn: []string{"s:1"}}
	d := NewDeps(cfg, []model.Item{one, two})

	if _, ok := Target(cfg, &one, d); !ok {
		t.Fatal("the item at the head of the sequence was refused")
	}
	if target, ok := Target(cfg, &two, d); ok {
		t.Fatalf("the follower was sent to %q while its dependency sat in ready", target)
	}
}

// Pick is handed a FILTERED slice by the scheduler's own launch loop — a
// dependency that is running or marked has already been dropped from it.
// Deps must therefore come from the full listing, or the one item that
// should hold its followers is exactly the one Pick cannot see.
func TestPickHoldsEvenWhenTheDependencyIsNotInTheSlice(t *testing.T) {
	cfg := line()
	one := model.Item{ID: "s:1", Stage: "ready"}
	two := model.Item{ID: "s:2", Stage: "ready", DependsOn: []string{"s:1"}}
	d := NewDeps(cfg, []model.Item{one, two}) // full listing

	// `free` excludes s:1 — it is already being worked.
	got, _ := Pick(cfg, []model.Item{two}, nil, d)
	if got != nil {
		t.Fatalf("Pick chose %s past a dependency missing from the slice", got.ID)
	}
}

// A held item is not in the queue, so it sorts with the rest of the tail —
// but it keeps its relative place among its siblings rather than being
// scrambled, because better() falls through to the same rungs Pick would use.
func TestOrderPutsHeldItemsBehindTheOnesThatCanRun(t *testing.T) {
	cfg := line()
	// Listing order is s:1, s:2, s:3 — the criterion is "manual order, then
	// priority, then listing position" for two held siblings, so with
	// neither manual order nor priority set, listing position decides and
	// s:2 (listed first) keeps its place ahead of s:3.
	items := []model.Item{
		{ID: "s:1", Stage: "ready"},
		{ID: "s:2", Stage: "ready", DependsOn: []string{"s:1"}},
		{ID: "s:3", Stage: "ready", DependsOn: []string{"s:2"}},
	}
	d := NewDeps(cfg, items)
	got := Order(cfg, items, nil, d)
	if got[0].ID != "s:1" {
		t.Fatalf("the runnable item sorted %s first, want s:1", got[0].ID)
	}
	if got[1].ID != "s:2" || got[2].ID != "s:3" {
		t.Fatalf("held items lost their listing order: %s then %s", got[1].ID, got[2].ID)
	}
}

// Two held siblings still answer to manual order first, the same as two
// workable ones do — being held changes nowhere to go, not which rung breaks
// the tie between them.
func TestOrderBreaksATieBetweenHeldSiblingsByManualOrderThenPriority(t *testing.T) {
	cfg := line()
	dep := model.Item{ID: "s:0", Stage: "ready"} // holds both followers below
	two := model.Item{ID: "s:2", Stage: "ready", DependsOn: []string{"s:0"}}
	three := model.Item{ID: "s:3", Stage: "ready", DependsOn: []string{"s:0"}}
	items := []model.Item{dep, two, three} // listing order: s:2 before s:3

	d := NewDeps(cfg, items)
	// Manual order names s:3 first, against listing order — it must still win.
	got := Order(cfg, items, []string{"s:3", "s:2"}, d)
	if got[1].ID != "s:3" || got[2].ID != "s:2" {
		t.Fatalf("manual order lost between held siblings: got %s then %s", got[1].ID, got[2].ID)
	}

	// With no manual order at all, priority breaks the tie instead.
	p0, p1 := 0, 1
	two.Priority, three.Priority = &p1, &p0
	items = []model.Item{dep, two, three}
	d = NewDeps(cfg, items)
	got = Order(cfg, items, nil, d)
	if got[1].ID != "s:3" || got[2].ID != "s:2" {
		t.Fatalf("priority lost between held siblings: got %s then %s", got[1].ID, got[2].ID)
	}
}

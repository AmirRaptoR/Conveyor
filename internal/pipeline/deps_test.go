package pipeline

import (
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// line is the pipeline these tests reason about: five stages, the last
// terminal, in the order the config declares them. Depth is that position.
func line() *config.Config {
	return &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "ready"},
		{Name: "ready", OnSuccess: "in-progress"},
		{Name: "in-progress", Script: "implement", OnSuccess: "review"},
		{Name: "review", Script: "review", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
}

func item(id, stage string, deps ...string) model.Item {
	return model.Item{ID: id, Stage: stage, DependsOn: deps}
}

func marked(it model.Item) model.Item { it.Blocked = true; return it }

// The rule, stated as a table. "A" is the dependency; "B" is what depends on
// it and wants to enter `target`.
func TestHeldNeverLetsAFollowerPassItsDependency(t *testing.T) {
	for _, tc := range []struct {
		name   string
		a      model.Item
		target string
		want   bool
		wantBy string
	}{
		{
			name:   "behind: the dependency is further along, so the move is free",
			a:      item("s:1", "review"),
			target: "in-progress",
			want:   false,
		},
		{
			name:   "level: a follower may share the stage its dependency is in",
			a:      item("s:1", "in-progress"),
			target: "in-progress",
			want:   false,
		},
		{
			name:   "ahead: a follower may never pass its dependency",
			a:      item("s:1", "ready"),
			target: "in-progress",
			want:   true,
			wantBy: "s:1",
		},
		{
			name:   "marked at the shared stage: not into a station that is stuck",
			a:      marked(item("s:1", "in-progress")),
			target: "in-progress",
			want:   true,
			wantBy: "s:1",
		},
		{
			name:   "marked further along: the follower is still free to move up",
			a:      marked(item("s:1", "review")),
			target: "in-progress",
			want:   false,
		},
		{
			name:   "terminal: a finished dependency is maximum depth and holds nobody",
			a:      item("s:1", "done"),
			target: "review",
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := item("s:2", "ready", "s:1")
			d := NewDeps(line(), []model.Item{tc.a, b})
			hold, held := d.Held(&b, tc.target)
			if held != tc.want {
				t.Fatalf("Held(%s -> %s) = %v, want %v", b.ID, tc.target, held, tc.want)
			}
			if held && hold.By != tc.wantBy {
				t.Fatalf("held by %q, want %q", hold.By, tc.wantBy)
			}
		})
	}
}

// A chain needs no transitive closure: every link is enforced, so child 3
// held behind child 2 which is held behind child 1 falls out of the same one
// rule, and child 3 declares only its immediate predecessor.
func TestAChainNeedsNoClosure(t *testing.T) {
	one := item("s:1", "ready")
	two := item("s:2", "ready", "s:1")
	three := item("s:3", "ready", "s:2")
	d := NewDeps(line(), []model.Item{one, two, three})

	if _, held := d.Held(&two, "in-progress"); !held {
		t.Fatal("child 2 was allowed past child 1")
	}
	if _, held := d.Held(&three, "in-progress"); !held {
		t.Fatal("child 3 was allowed past child 2")
	}
	if got := len(three.DependsOn); got != 1 {
		t.Fatalf("the fixture declares %d dependencies, want 1", got)
	}
}

// Fail open, every way an edge can be wrong. A typo must never wedge the
// line, and each case must leave the follower workable — not merely
// crash-free.
func TestBadEdgesFailOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []model.Item
	}{
		{
			name:  "an id nobody listed (un-onboarded, another repo, a typo)",
			items: []model.Item{item("s:2", "ready", "s:999")},
		},
		{
			name:  "an item depending on itself",
			items: []model.Item{item("s:2", "ready", "s:2")},
		},
		{
			name: "a dependency sitting in a stage the config does not declare",
			items: []model.Item{
				item("s:1", "somewhere-else"),
				item("s:2", "ready", "s:1"),
			},
		},
		{
			name:  "an empty-string id",
			items: []model.Item{item("s:2", "ready", "")},
		},
		{
			name:  "duplicate ids in one DependsOn",
			items: []model.Item{item("s:2", "ready", "s:999", "s:999")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDeps(line(), tc.items)
			b := tc.items[len(tc.items)-1]
			if _, held := d.Held(&b, "in-progress"); held {
				t.Fatal("a bad edge held the item instead of being dropped")
			}
			if target, ok := Target(line(), &b, d); !ok {
				t.Fatalf("the follower has no target at all: %q", target)
			}
		})
	}
}

// Duplicate ids naming a real dependency must not double-count or otherwise
// change the answer — held is still true, exactly as with one copy.
func TestDuplicateIDsCollapseToOneEdge(t *testing.T) {
	one := item("s:1", "ready")
	two := item("s:2", "ready", "s:1", "s:1")
	d := NewDeps(line(), []model.Item{one, two})
	hold, held := d.Held(&two, "in-progress")
	if !held || hold.By != "s:1" {
		t.Fatalf("Held = %+v, %v; want held by s:1", hold, held)
	}
}

// Several dependencies hold until all of them permit. The most restrictive —
// the one at the lowest depth — is the one reported, and ties are broken by
// listing order so the report is stable across polls with unchanged input.
func TestMultipleDependenciesReportTheMostRestrictive(t *testing.T) {
	// s:1 is further back (ready, depth 1) than s:2's other dependency s:0
	// (in-progress, depth 2): s:1 is the more restrictive one.
	zero := item("s:0", "in-progress")
	one := item("s:1", "ready")
	two := item("s:2", "ready", "s:0", "s:1")
	d := NewDeps(line(), []model.Item{zero, one, two})

	hold, held := d.Held(&two, "in-progress")
	if !held || hold.By != "s:1" {
		t.Fatalf("Held = %+v, %v; want held by s:1 (lower depth = more restrictive)", hold, held)
	}
}

// A tie in depth is broken by listing order, so the reported holder does not
// flap between two dependencies that hold equally.
func TestATiedHoldIsBrokenByListingOrder(t *testing.T) {
	oneB := item("s:1b", "ready") // listed first
	one := item("s:1", "ready")  // listed second
	two := item("s:2", "ready", "s:1", "s:1b")
	d := NewDeps(line(), []model.Item{oneB, one, two})

	hold, held := d.Held(&two, "in-progress")
	if !held || hold.By != "s:1b" {
		t.Fatalf("Held = %+v, %v; want held by s:1b (listed first)", hold, held)
	}
}

// A cycle is a person's mistake in an issue body. Holding every item in it
// forever would be a pipeline wedged by a typo, so the edges go and the
// board is told, once, naming every item involved.
func TestCyclesAreDroppedAndReported(t *testing.T) {
	two := item("s:2", "ready", "s:1")
	three := item("s:3", "ready", "s:2")
	one := item("s:1", "ready", "s:3") // closes the loop
	d := NewDeps(line(), []model.Item{one, two, three})

	for _, it := range []model.Item{one, two, three} {
		if _, held := d.Held(&it, "in-progress"); held {
			t.Fatalf("%s was held by an edge that is part of a cycle", it.ID)
		}
	}
	if len(d.Cycles) != 1 {
		t.Fatalf("reported %d cycles, want 1: %v", len(d.Cycles), d.Cycles)
	}
	for _, id := range []string{"s:1", "s:2", "s:3"} {
		if !strings.Contains(d.Cycles[0], id) {
			t.Fatalf("the warning does not name %s: %q", id, d.Cycles[0])
		}
	}
}

// A cycle must not take unrelated edges down with it.
func TestACycleDoesNotDisarmTheRestOfTheGraph(t *testing.T) {
	one := item("s:1", "ready", "s:2", "s:4") // A->B (cycle) and A->C (valid)
	two := item("s:2", "ready", "s:1")        // B->A: closes the cycle with s:1
	four := item("s:4", "ready")
	five := item("s:5", "ready", "s:4") // innocent, and behind s:4
	d := NewDeps(line(), []model.Item{one, two, four, five})

	if _, held := d.Held(&five, "in-progress"); !held {
		t.Fatal("an edge with nothing to do with the cycle was dropped too")
	}
	// The A->C edge in "A->B, B->A and A->C" still gates A behind C: only the
	// edges inside the cycle (A<->B) are dropped, A->C survives.
	if _, held := d.Held(&one, "in-progress"); !held {
		t.Fatal("the edge outside the cycle (s:1 -> s:4) was dropped along with it")
	}
}

// A 3-cycle is dropped the same way a 2-cycle is.
func TestThreeCycleIsDroppedAndReported(t *testing.T) {
	a := item("a:1", "ready", "a:3")
	b := item("a:2", "ready", "a:1")
	c := item("a:3", "ready", "a:2")
	d := NewDeps(line(), []model.Item{a, b, c})
	for _, it := range []model.Item{a, b, c} {
		if _, held := d.Held(&it, "in-progress"); held {
			t.Fatalf("%s was held by a 3-cycle edge", it.ID)
		}
	}
	if len(d.Cycles) != 1 {
		t.Fatalf("reported %d cycles, want 1", len(d.Cycles))
	}
}

// The zero value holds nothing. Callers that have no listing to build from —
// and every existing test — must keep working.
func TestTheZeroDepsHoldsNothing(t *testing.T) {
	b := item("s:2", "ready", "s:1")
	if _, held := (Deps{}).Held(&b, "in-progress"); held {
		t.Fatal("the zero Deps held an item")
	}
}

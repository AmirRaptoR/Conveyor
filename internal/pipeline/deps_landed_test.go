package pipeline

import (
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// landedLine is `line()` with the implement stage asking for more than
// sharing: a follower may enter it only once every dependency has reached
// `merged`. The stages after review exist so "reached" and "passed" are both
// exercised.
func landedLine() *config.Config {
	return &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "ready"},
		{Name: "ready", OnSuccess: "in-progress"},
		{Name: "in-progress", Script: "implement", OnSuccess: "review", DependenciesAt: "merged"},
		{Name: "review", Script: "review", OnSuccess: "merged"},
		{Name: "merged", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
}

// The bar moves from the target stage to the one the config names: a
// dependency still being implemented or reviewed holds its follower out of
// implement, and only one that has landed lets it in.
func TestDependenciesAtHoldsUntilTheNamedStage(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    model.Item
		want bool
	}{
		{"still in ready", item("s:1", "ready"), true},
		{"sharing in-progress is no longer enough", item("s:1", "in-progress"), true},
		{"in review: on a branch, not on main", item("s:1", "review"), true},
		{"reached merged", item("s:1", "merged"), false},
		{"marked in merged: that station did not finish", marked(item("s:1", "merged")), true},
		{"past merged", item("s:1", "done"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := item("s:2", "ready", "s:1")
			d := NewDeps(landedLine(), []model.Item{tc.a, b})
			hold, held := d.Held(&b, "in-progress")
			if held != tc.want {
				t.Fatalf("Held(%s -> in-progress) with dependency in %s = %v, want %v", b.ID, tc.a.Stage, held, tc.want)
			}
			if held {
				if hold.Until != "merged" {
					t.Fatalf("hold.Until = %q, want merged", hold.Until)
				}
				if hold.Target != "in-progress" {
					t.Fatalf("hold.Target = %q, want in-progress", hold.Target)
				}
			}
		})
	}
}

// Only the stage that declares it is affected. Entering review still follows
// the default rule, so a follower whose dependency is already in review may
// join it there — the bar is per stage, not per pipeline.
func TestDependenciesAtLeavesOtherStagesOnTheDefaultRule(t *testing.T) {
	a := item("s:1", "review")
	b := item("s:2", "in-progress", "s:1")
	d := NewDeps(landedLine(), []model.Item{a, b})
	if _, held := d.Held(&b, "review"); held {
		t.Fatal("review declares no dependenciesAt, so sharing it must still be allowed")
	}
}

// Target reads the same rule, so the scheduler, the drag endpoint and the
// board agree: a follower resting in ready has nowhere to go while its
// dependency is in review, and somewhere to go the moment it lands.
func TestTargetHonoursDependenciesAt(t *testing.T) {
	cfg := landedLine()
	b := item("s:2", "ready", "s:1")
	if _, ok := Target(cfg, &b, NewDeps(cfg, []model.Item{item("s:1", "review"), b})); ok {
		t.Fatal("Target let a follower into in-progress while its dependency was in review")
	}
	next, ok := Target(cfg, &b, NewDeps(cfg, []model.Item{item("s:1", "merged"), b}))
	if !ok || next != "in-progress" {
		t.Fatalf("Target = %q, %v; want in-progress once the dependency has merged", next, ok)
	}
}

// Under the default rule Until names the target itself, so a reader of the
// hold never has to know whether the stage declared anything.
func TestUntilIsTheTargetUnderTheDefaultRule(t *testing.T) {
	b := item("s:2", "ready", "s:1")
	d := NewDeps(line(), []model.Item{item("s:1", "ready"), b})
	hold, held := d.Held(&b, "in-progress")
	if !held {
		t.Fatal("expected the follower to be held")
	}
	if hold.Until != "in-progress" {
		t.Fatalf("hold.Until = %q, want in-progress", hold.Until)
	}
}

package pipeline

import (
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func tracker(id, stage string, children ...string) model.Item {
	return model.Item{ID: id, Source: "s", Stage: stage, Tracking: true, Children: children}
}

func child(id, stage, parent string) model.Item {
	return model.Item{ID: id, Source: "s", Stage: stage, Parent: parent}
}

func TestTrackingLifecycleEmptyPartialCompleteAndSettled(t *testing.T) {
	cfg := line()
	for _, tc := range []struct {
		name   string
		items  []model.Item
		state  TrackingState
		target string
	}{
		{name: "empty", items: []model.Item{tracker("s:10", "backlog")}, state: TrackingInvalid},
		{name: "partial", items: []model.Item{
			tracker("s:10", "backlog", "s:11", "s:12"),
			child("s:11", "done", "s:10"), child("s:12", "review", "s:10"),
		}, state: TrackingPartial},
		{name: "complete", items: []model.Item{
			tracker("s:10", "backlog", "s:11", "s:12"),
			child("s:11", "done", "s:10"), child("s:12", "done", "s:10"),
		}, state: TrackingComplete, target: "done"},
		{name: "settled", items: []model.Item{
			tracker("s:10", "done", "s:11"), child("s:11", "done", "s:10"),
		}, state: TrackingSettled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDeps(cfg, tc.items)
			got := d.Track(&tc.items[0])
			if got.State != tc.state || got.Target != tc.target {
				t.Fatalf("Track = %+v, want state %q target %q", got, tc.state, tc.target)
			}
			if tc.state == TrackingInvalid && got.Reason == "" {
				t.Fatal("invalid tracker has no actionable reason")
			}
		})
	}
}

func TestTrackingLifecycleInvalidDeclarationsAreDeterministic(t *testing.T) {
	cfg := line()
	for _, tc := range []struct {
		name  string
		items []model.Item
		want  string
	}{
		{name: "missing child", items: []model.Item{tracker("s:10", "backlog", "s:99")}, want: "missing child s:99"},
		{name: "self child", items: []model.Item{tracker("s:10", "backlog", "s:10")}, want: "names itself"},
		{name: "unqualified child", items: []model.Item{tracker("s:10", "backlog", "11")}, want: "unqualified child id"},
		{name: "duplicate child", items: []model.Item{
			tracker("s:10", "backlog", "s:11", "s:11"), child("s:11", "done", "s:10"),
		}, want: "names child s:11 more than once"},
		{name: "cross-source child", items: []model.Item{
			tracker("s:10", "backlog", "other:11"),
			{ID: "other:11", Source: "other", Stage: "done", Parent: "s:10"},
		}, want: "cross-source child other:11"},
		{name: "non-reciprocal child", items: []model.Item{
			tracker("s:10", "backlog", "s:11"), child("s:11", "done", "s:other"),
		}, want: "names parent s:other"},
		{name: "unknown child stage", items: []model.Item{
			tracker("s:10", "backlog", "s:11"), child("s:11", "elsewhere", "s:10"),
		}, want: "stage \"elsewhere\" is not declared"},
		{name: "terminal tracker with unfinished child", items: []model.Item{
			tracker("s:10", "done", "s:11"), child("s:11", "review", "s:10"),
		}, want: "terminal in done while child s:11 is unfinished in review"},
		{name: "terminal child is still marked", items: []model.Item{
			tracker("s:10", "backlog", "s:11"),
			{ID: "s:11", Source: "s", Stage: "done", Parent: "s:10", Blocked: true},
		}, want: "child s:11 marked in terminal stage done"},
		{name: "provider child set is incomplete", items: []model.Item{
			{ID: "s:10", Source: "s", Stage: "backlog", Tracking: true, TrackingError: "pagination was capped", Children: []string{"s:11"}},
			child("s:11", "done", "s:10"),
		}, want: "incomplete child set: pagination was capped"},
		{name: "parent cycle", items: []model.Item{
			{ID: "s:10", Source: "s", Stage: "backlog", Tracking: true, Parent: "s:11", Children: []string{"s:11"}},
			{ID: "s:11", Source: "s", Stage: "backlog", Parent: "s:10", Children: []string{"s:10"}},
		}, want: "parent cycle: s:10 -> s:11 -> s:10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NewDeps(cfg, tc.items).Track(&tc.items[0])
			if got.State != TrackingInvalid || !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("Track = %+v, want invalid reason containing %q", got, tc.want)
			}
			// Re-evaluating unchanged listing data must produce the exact same
			// provider mark text, or every poll would rewrite it.
			again := NewDeps(cfg, tc.items).Track(&tc.items[0])
			if again.Reason != got.Reason {
				t.Fatalf("reason changed across evaluations: %q then %q", got.Reason, again.Reason)
			}
		})
	}
}

func TestTrackingItemsBypassOrdinaryStagesAndOnlyCompleteOnesTargetTerminal(t *testing.T) {
	cfg := line()
	partial := []model.Item{
		tracker("s:10", "in-progress", "s:11"), child("s:11", "review", "s:10"),
	}
	d := NewDeps(cfg, partial)
	if target, ok := Target(cfg, &partial[0], d); ok {
		t.Fatalf("partial tracker was scheduled into %q", target)
	}
	if got, target := Pick(cfg, partial[:1], nil, d); got != nil {
		t.Fatalf("Pick chose partial tracker %s for %q", got.ID, target)
	}

	complete := []model.Item{
		tracker("s:10", "in-progress", "s:11"), child("s:11", "done", "s:10"),
	}
	d = NewDeps(cfg, complete)
	if target, ok := Target(cfg, &complete[0], d); !ok || target != "done" {
		t.Fatalf("complete tracker Target = %q, %v; want done, true", target, ok)
	}
	if got, target := Pick(cfg, complete[:1], nil, d); got == nil || got.ID != "s:10" || target != "done" {
		t.Fatalf("Pick = %+v, %q; want tracker -> done", got, target)
	}

	// A tracking item without the full-listing evaluator fails closed rather
	// than falling back to the ordinary stage graph.
	if target, ok := Target(cfg, &complete[0], Deps{}); ok {
		t.Fatalf("tracker without listing context was scheduled into %q", target)
	}
}

func TestChildrenDoNotImplicitlyMakeOrdinaryWorkATracker(t *testing.T) {
	cfg := line()
	it := model.Item{ID: "s:10", Source: "s", Stage: "ready", Children: []string{"s:11"}}
	child := child("s:11", "done", "s:10")
	d := NewDeps(cfg, []model.Item{it, child})
	if got := d.Track(&it); got.State != "" {
		t.Fatalf("ordinary item acquired tracking lifecycle %+v", got)
	}
	if target, ok := Target(cfg, &it, d); !ok || target != "in-progress" {
		t.Fatalf("ordinary item with children Target = %q, %v; want normal in-progress route", target, ok)
	}
}

func TestCompleteTrackerStillHonorsDeclaredDependencies(t *testing.T) {
	cfg := line()
	items := []model.Item{
		{ID: "s:9", Source: "s", Stage: "backlog"},
		{ID: "s:10", Source: "s", Stage: "backlog", Tracking: true, Children: []string{"s:11"}, DependsOn: []string{"s:9"}},
		child("s:11", "done", "s:10"),
	}
	d := NewDeps(cfg, items)
	if target, ok := Target(cfg, &items[1], d); ok {
		t.Fatalf("complete tracker bypassed dependency hold and targeted %q", target)
	}
	items[0].Stage = "done"
	d = NewDeps(cfg, items)
	if target, ok := Target(cfg, &items[1], d); !ok || target != "done" {
		t.Fatalf("released complete tracker Target = %q, %v; want done", target, ok)
	}
}

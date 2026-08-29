package pipeline

import "testing"

// A source maps to a git worktree: two agents in one checkout corrupt each
// other, so this holds no matter how the other axes are set.
func TestOneItemPerSource(t *testing.T) {
	l := NewLocks(10, 10, 1)
	if !l.TryAcquire("midgame", "refining") {
		t.Fatal("first acquire refused")
	}
	if l.TryAcquire("midgame", "in-progress") {
		t.Error("a second transition for the same source was allowed")
	}
	l.Release("midgame", "refining")
	if !l.TryAcquire("midgame", "in-progress") {
		t.Error("the source stayed locked after release")
	}
}

// The point of the change: different sources work different stages at once.
func TestDifferentSourcesRunDifferentStages(t *testing.T) {
	l := NewLocks(4, 1, 1)
	for _, c := range []struct{ src, stage string }{
		{"midgame", "refining"}, {"quesshi", "in-progress"}, {"caravan", "testing"},
	} {
		if !l.TryAcquire(c.src, c.stage) {
			t.Fatalf("%s in %s was refused; stages should run in parallel", c.src, c.stage)
		}
	}
}

// A station works one item at a time, so a second source wanting the same
// stage waits even though its own worktree is free.
func TestPerStageBound(t *testing.T) {
	l := NewLocks(10, 1, 1)
	if !l.TryAcquire("midgame", "refining") {
		t.Fatal("first acquire refused")
	}
	if l.TryAcquire("quesshi", "refining") {
		t.Error("two items entered the same stage with perStage 1")
	}
	l.Release("midgame", "refining")
	if !l.TryAcquire("quesshi", "refining") {
		t.Error("the stage stayed locked after release")
	}
}

func TestPerStageAboveOne(t *testing.T) {
	l := NewLocks(10, 2, 1)
	if !l.TryAcquire("a", "refining") || !l.TryAcquire("b", "refining") {
		t.Fatal("perStage 2 refused a second item")
	}
	if l.TryAcquire("c", "refining") {
		t.Error("perStage 2 allowed a third")
	}
}

// Global caps the total however the other axes are set, because every slot is
// an agent.
func TestGlobalCap(t *testing.T) {
	l := NewLocks(2, 10, 1)
	if !l.TryAcquire("a", "one") || !l.TryAcquire("b", "two") {
		t.Fatal("global 2 refused the first two")
	}
	if l.TryAcquire("c", "three") {
		t.Error("global cap exceeded")
	}
}

// Taking one axis while refused another would leave a slot held by nothing,
// and two schedulers doing that deadlock each other.
func TestRefusedAcquireTakesNothing(t *testing.T) {
	l := NewLocks(1, 10, 1)
	if !l.TryAcquire("a", "one") {
		t.Fatal("first acquire refused")
	}
	if l.TryAcquire("b", "two") { // refused by the global cap
		t.Fatal("global cap exceeded")
	}
	l.Release("a", "one")
	// If the refused attempt had kept b's source or stage slot, this fails.
	if !l.TryAcquire("b", "two") {
		t.Error("a refused acquire left slots held")
	}
	if l.Busy("c", "three") {
		t.Error("an unrelated transition reported busy")
	}
}

// perSource above 1 is the point of giving each item its own worktree: two
// items of one repository run at once, and the third still waits. Before this
// the source axis was a boolean, so a raised limit would have been silently
// ignored and the config would have lied.
func TestPerSourceCountsInsteadOfLatching(t *testing.T) {
	l := NewLocks(10, 10, 2)

	if !l.TryAcquire("repo", "a") || !l.TryAcquire("repo", "b") {
		t.Fatal("two items of one source must both start when perSource is 2")
	}
	if l.TryAcquire("repo", "c") {
		t.Error("a third item started; perSource 2 was not enforced")
	}
	if !l.TryAcquire("other", "c") {
		t.Error("a different source was refused; the limit is per source, not global")
	}

	// Released one at a time, not all at once: a boolean map used to forget the
	// whole source on the first release, letting the next pass double-book it.
	l.Release("repo", "a")
	if !l.TryAcquire("repo", "c") {
		t.Error("releasing one of two did not free exactly one slot")
	}
	if l.TryAcquire("repo", "d") {
		t.Error("releasing one slot freed two")
	}
}

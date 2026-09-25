package registry

import "testing"

func TestOpenThenLookup(t *testing.T) {
	r := New()
	r.Open("item1", Entry{RunID: "run1", Dir: "/tmp/run1", Stage: "implement"})
	e, ok := r.Lookup("item1")
	if !ok || e.RunID != "run1" || e.Dir != "/tmp/run1" || e.Stage != "implement" {
		t.Fatalf("unexpected lookup result: %+v ok=%v", e, ok)
	}
}

func TestLookupMissingItem(t *testing.T) {
	r := New()
	_, ok := r.Lookup("nope")
	if ok {
		t.Fatalf("expected no entry for an unregistered item")
	}
}

func TestCloseRemovesMatchingRun(t *testing.T) {
	r := New()
	r.Open("item1", Entry{RunID: "run1"})
	r.Close("item1", "run1")
	if _, ok := r.Lookup("item1"); ok {
		t.Fatalf("expected the entry gone after Close")
	}
}

// A stale Close naming a run that is no longer the current one for this
// item must not clobber a newer entry — the gap between a stage process
// exiting and the transition confirming its move is exactly one place a
// second run could already have started for the same item.
func TestCloseIgnoresStaleRunID(t *testing.T) {
	r := New()
	r.Open("item1", Entry{RunID: "run1"})
	r.Open("item1", Entry{RunID: "run2"}) // a second run started for the same item
	r.Close("item1", "run1")              // the first run's belated close
	e, ok := r.Lookup("item1")
	if !ok || e.RunID != "run2" {
		t.Fatalf("expected run2's entry to survive a stale close for run1, got %+v ok=%v", e, ok)
	}
}

func TestCloseOnUnknownItemIsNoop(t *testing.T) {
	r := New()
	r.Close("nope", "run1") // must not panic
}

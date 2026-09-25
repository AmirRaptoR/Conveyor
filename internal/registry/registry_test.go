package registry

import (
	"sync"
	"testing"
	"time"
)

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

func TestLeaseMakesOperationAtomicWithClose(t *testing.T) {
	r := New()
	r.Open("item1", Entry{RunID: "run1"})
	lease, ok := r.Acquire("item1", "run1")
	if !ok {
		t.Fatal("Acquire refused a live run")
	}
	closed := make(chan struct{})
	go func() {
		r.Close("item1", "run1")
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close passed an in-flight operation")
	case <-time.After(20 * time.Millisecond):
	}
	lease.Release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after the lease released")
	}
	if _, ok := r.Acquire("item1", "run1"); ok {
		t.Fatal("Acquire succeeded after Close")
	}
}

func TestLeasesForDifferentItemsDoNotSerialize(t *testing.T) {
	r := New()
	r.Open("one", Entry{RunID: "r1"})
	r.Open("two", Entry{RunID: "r2"})
	first, _ := r.Acquire("one", "r1")
	defer first.Release()
	got := make(chan *Lease, 1)
	go func() {
		lease, _ := r.Acquire("two", "r2")
		got <- lease
	}()
	select {
	case second := <-got:
		if second == nil {
			t.Fatal("second item was refused")
		}
		second.Release()
	case <-time.After(time.Second):
		t.Fatal("different items serialized behind one lease")
	}
}

func TestConcurrentAcquireSameItemSerializes(t *testing.T) {
	r := New()
	r.Open("item", Entry{RunID: "run"})
	var wg sync.WaitGroup
	var mu sync.Mutex
	in, maxIn := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, ok := r.Acquire("item", "run")
			if !ok {
				t.Error("Acquire refused live entry")
				return
			}
			mu.Lock()
			in++
			if in > maxIn {
				maxIn = in
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			in--
			mu.Unlock()
			lease.Release()
		}()
	}
	wg.Wait()
	if maxIn != 1 {
		t.Fatalf("max concurrent operations = %d", maxIn)
	}
}

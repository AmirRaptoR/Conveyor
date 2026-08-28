package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOrderRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.json")
	o := OpenOrder(path)
	if len(o.IDs()) != 0 {
		t.Fatalf("a fresh order should be empty, got %v", o.IDs())
	}
	if err := o.Set([]string{"a:1", "a:2"}); err != nil {
		t.Fatal(err)
	}
	// Survives a restart, which is the entire point of persisting it.
	if got := OpenOrder(path).IDs(); len(got) != 2 || got[0] != "a:1" {
		t.Errorf("reopened order = %v, want [a:1 a:2]", got)
	}
}

// A client sending a stale list must not be able to make one item outrank
// itself, so duplicates collapse to their first position.
func TestSetDropsDuplicatesAndBlanks(t *testing.T) {
	o := OpenOrder(filepath.Join(t.TempDir(), "order.json"))
	if err := o.Set([]string{"a:1", "", "a:2", "a:1", "a:3"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a:1", "a:2", "a:3"}
	got := o.IDs()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// IDs hands out a copy: a caller mutating the slice it got must not silently
// reorder the queue for everyone else.
func TestIDsIsACopy(t *testing.T) {
	o := OpenOrder(filepath.Join(t.TempDir(), "order.json"))
	_ = o.Set([]string{"a:1", "a:2"})
	got := o.IDs()
	got[0] = "tampered"
	if o.IDs()[0] != "a:1" {
		t.Error("mutating the returned slice changed the stored order")
	}
}

// Losing a manual arrangement is a nuisance; refusing to start over it would
// strand every repository the pipeline was working.
func TestCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := OpenOrder(path).IDs(); len(got) != 0 {
		t.Errorf("corrupt order = %v, want empty", got)
	}
}

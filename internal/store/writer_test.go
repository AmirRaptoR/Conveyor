package store

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// F07: IDs() must not be blocked for the duration of a write. It takes only
// the data lock, so it must return promptly even while a write holds the
// separate writer lock — simulated directly here rather than by trying to
// make a real fsync slow, since the property under test is "two locks, not
// one", not any particular disk's speed.
func TestOrderIDsIsNotBlockedByAConcurrentWrite(t *testing.T) {
	o := OpenOrder(filepath.Join(t.TempDir(), "order.json"))
	if err := o.Set([]string{"a:1"}); err != nil {
		t.Fatal(err)
	}

	o.writeMu.Lock()
	defer o.writeMu.Unlock()

	done := make(chan struct{})
	go func() {
		o.IDs()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("IDs() blocked behind the writer lock; a slow write would stall the scheduler's hot path")
	}
}

// The same property, for Answers.Get.
func TestAnswersGetIsNotBlockedByAConcurrentWrite(t *testing.T) {
	a := OpenAnswers(filepath.Join(t.TempDir(), "answers.json"))
	if err := a.Set("id1", model.Resume{Answer: "hi"}); err != nil {
		t.Fatal(err)
	}

	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	done := make(chan struct{})
	go func() {
		a.Get("id1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Get() blocked behind the writer lock; a slow write would stall the scheduler's hot path")
	}
}

// Two concurrent Set calls must never corrupt the file: the writer lock
// serialises them, so the surviving state — on disk and in memory — is one of
// the two complete snapshots, never a mix, and never disagrees with itself
// after a reopen.
func TestConcurrentOrderSetsNeverCorruptTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.json")
	o := OpenOrder(path)
	a, b := []string{"a:1", "a:2"}, []string{"b:1", "b:2", "b:3"}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs <- o.Set(a) }()
	go func() { defer wg.Done(); errs <- o.Set(b) }()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Set returned %v", err)
		}
	}

	got := o.IDs()
	if !reflect.DeepEqual(got, a) && !reflect.DeepEqual(got, b) {
		t.Fatalf("final ids = %v, want exactly one of %v or %v", got, a, b)
	}
	reopened := OpenOrder(path).IDs()
	if !reflect.DeepEqual(reopened, got) {
		t.Errorf("disk = %v, memory = %v; a competing write left them disagreeing", reopened, got)
	}
}

// A write that cannot be persisted must not be believed: Set returns the
// error and leaves memory exactly as it was, matching what is still on disk.
func TestOrderSetPropagatesWriteFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// blocker is a file, so a path beneath it can never be created.
	o := OpenOrder(filepath.Join(blocker, "order.json"))
	if err := o.Set([]string{"a:1"}); err == nil {
		t.Fatal("Set over an unwritable path returned a nil error")
	}
	if got := o.IDs(); len(got) != 0 {
		t.Errorf("ids = %v after a failed write, want unchanged (empty)", got)
	}
}

// The write itself can succeed and the rename that publishes it can still
// fail (the target is a directory, a cross-device link, whatever the
// filesystem objects to) — that must be covered distinctly from a write that
// never got as far as a temp file, and must leave memory just as untouched.
func TestOrderSetPropagatesRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "order.json")
	// The rename target is a directory, so the rename itself fails even
	// though writing the temp file next to it succeeds.
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	o := OpenOrder(path)
	if err := o.Set([]string{"a:1"}); err == nil {
		t.Fatal("Set whose rename fails returned a nil error")
	}
	if got := o.IDs(); len(got) != 0 {
		t.Errorf("ids = %v after a failed rename, want unchanged (empty)", got)
	}
	// No stray temp file left behind for a crash to find later.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "order.json" {
			t.Errorf("leftover entry %q after a failed rename", e.Name())
		}
	}
}

// Answers.Set: the same guarantee — persisted before it is visible, and a
// failed write changes nothing in memory.
func TestAnswersSetPropagatesWriteFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := OpenAnswers(filepath.Join(blocker, "answers.json"))
	if err := a.Set("id1", model.Resume{Answer: "hi"}); err == nil {
		t.Fatal("Set over an unwritable path returned a nil error")
	}
	if got := a.Get("id1"); got.Answer != "" {
		t.Errorf("Get = %+v after a failed write, want nothing recorded", got)
	}
}

// Take must propagate a write failure rather than discarding it, and must not
// report the answer spent when the write did not happen — a restart must find
// it still on disk, matching what every caller was told.
func TestAnswersTakePropagatesWriteFailureAndDoesNotSpend(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "answers.json")
	a := OpenAnswers(path)
	r := model.Resume{Answer: "keep it", Session: "sess-1"}
	if err := a.Set("id1", r); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)

	spent, err := a.Take("id1", r)
	if err == nil {
		t.Fatal("Take with a failing write returned a nil error")
	}
	if spent {
		t.Error("Take reported the answer spent despite the write failing")
	}
	if got := a.Get("id1"); got != r {
		t.Errorf("Get = %+v after a failed Take, want the original answer still held: %+v", got, r)
	}

	os.Chmod(dir, 0o755)
	if got := OpenAnswers(path).Get("id1"); got != r {
		t.Errorf("reopened = %+v, want the original answer still on disk: %+v", got, r)
	}
}

// Take is compare-and-delete: an answer recorded while a run was in flight
// with an older one is not that run's to spend.
func TestTakeIsCompareAndDelete(t *testing.T) {
	a := OpenAnswers(filepath.Join(t.TempDir(), "answers.json"))
	old := model.Resume{Answer: "first", Session: "s1"}
	if err := a.Set("id1", old); err != nil {
		t.Fatal(err)
	}
	fresh := model.Resume{Answer: "second", Session: "s1"}
	if err := a.Set("id1", fresh); err != nil {
		t.Fatal(err)
	}

	// A run that read the old value before the new one landed must not spend
	// the new one.
	spent, err := a.Take("id1", old)
	if err != nil {
		t.Fatal(err)
	}
	if spent {
		t.Error("Take spent an answer that had already been replaced")
	}
	if got := a.Get("id1"); got != fresh {
		t.Errorf("Get = %+v, want the fresher answer to have survived: %+v", got, fresh)
	}

	spent, err = a.Take("id1", fresh)
	if err != nil || !spent {
		t.Fatalf("Take(fresh) = (%v, %v), want (true, nil)", spent, err)
	}
	if got := a.Get("id1"); got.Answer != "" {
		t.Errorf("Get = %+v after Take, want it spent", got)
	}
}

package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestReserveGrantsUntilThePerItemCeilingThenRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	b := OpenBudgets(path)

	for i := 0; i < 3; i++ {
		ok, reason, err := b.Reserve("item-1", "2026-09-11", 3, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("Reserve #%d refused (%s), want granted", i+1, reason)
		}
	}
	ok, reason, err := b.Reserve("item-1", "2026-09-11", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok || reason != "item" {
		t.Fatalf("Reserve past the per-item ceiling = ok=%v reason=%q, want refused with reason \"item\"", ok, reason)
	}

	// A different item has its own ledger and is untouched by item-1's ceiling.
	if ok, _, err := b.Reserve("item-2", "2026-09-11", 3, 0); err != nil || !ok {
		t.Fatalf("Reserve for a different item = ok=%v err=%v, want granted", ok, err)
	}
}

func TestReserveGrantsUntilThePerDayCeilingThenRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	b := OpenBudgets(path)

	// Two different items sharing one day's board-wide ceiling.
	if ok, _, err := b.Reserve("item-1", "2026-09-11", 0, 2); err != nil || !ok {
		t.Fatalf("Reserve #1 = ok=%v err=%v, want granted", ok, err)
	}
	if ok, _, err := b.Reserve("item-2", "2026-09-11", 0, 2); err != nil || !ok {
		t.Fatalf("Reserve #2 = ok=%v err=%v, want granted", ok, err)
	}
	ok, reason, err := b.Reserve("item-3", "2026-09-11", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ok || reason != "day" {
		t.Fatalf("Reserve past the daily ceiling = ok=%v reason=%q, want refused with reason \"day\"", ok, reason)
	}
}

// The daily-window boundary: yesterday's spend must never count against
// today's ceiling, and today's ledger must start back at zero the moment the
// day the caller names actually changes — Reserve never reads a clock itself,
// so this is exercised directly by naming the boundary.
func TestReserveRollsOverAtTheDayBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	b := OpenBudgets(path)

	for i := 0; i < 2; i++ {
		if ok, _, err := b.Reserve("item-1", "2026-09-11", 0, 2); err != nil || !ok {
			t.Fatalf("Reserve on day 1 (#%d) = ok=%v err=%v, want granted", i+1, ok, err)
		}
	}
	if ok, reason, err := b.Reserve("item-2", "2026-09-11", 0, 2); err != nil || ok {
		t.Fatalf("Reserve past day 1's ceiling = ok=%v reason=%q err=%v, want refused", ok, reason, err)
	}
	if got := b.DayUsage("2026-09-11"); got != 2 {
		t.Errorf("DayUsage(2026-09-11) = %d, want 2", got)
	}

	// The next day starts at zero, even though the ceiling and the items
	// spending against it are unchanged.
	if ok, _, err := b.Reserve("item-2", "2026-09-12", 0, 2); err != nil || !ok {
		t.Fatalf("Reserve on day 2 = ok=%v err=%v, want granted (a fresh day, not yesterday's exhausted one)", ok, err)
	}
	if got := b.DayUsage("2026-09-12"); got != 1 {
		t.Errorf("DayUsage(2026-09-12) = %d, want 1", got)
	}
	// Asking about the day that just ended reports zero now: nothing is kept
	// past the day currently tracked, because nothing ever reads an old one.
	if got := b.DayUsage("2026-09-11"); got != 0 {
		t.Errorf("DayUsage(2026-09-11) after rolling into day 2 = %d, want 0", got)
	}

	// A per-item ceiling is unaffected by the day rolling over — item-1's
	// two runs on day 1 still count against it on day 2.
	if ok, reason, err := b.Reserve("item-1", "2026-09-12", 2, 0); err != nil || ok {
		t.Fatalf("Reserve for item-1 against its lifetime ceiling on day 2 = ok=%v reason=%q err=%v, want refused (it already spent 2)", ok, reason, err)
	}
}

// The check-then-increment race #39 calls out: N goroutines all racing the
// same day's ceiling must never grant more than the ceiling allows, however
// they interleave.
func TestReserveCannotOverspendTheDailyCeilingUnderConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	b := OpenBudgets(path)

	const ceiling = 5
	const attempts = 40
	var granted int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, _, err := b.Reserve("shared-day", "2026-09-11", 0, ceiling)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if granted != ceiling {
		t.Fatalf("granted = %d concurrent reservations, want exactly %d", granted, ceiling)
	}
	if got := b.DayUsage("2026-09-11"); got != ceiling {
		t.Errorf("DayUsage = %d after the race, want %d", got, ceiling)
	}
}

func TestOverrideLiftsBothCeilingsAndRestoreReimposesThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	b := OpenBudgets(path)

	if ok, _, err := b.Reserve("item-1", "2026-09-11", 1, 1); err != nil || !ok {
		t.Fatalf("first Reserve = ok=%v err=%v, want granted", ok, err)
	}
	if ok, _, err := b.Reserve("item-1", "2026-09-11", 1, 1); err != nil || ok {
		t.Fatalf("Reserve past the ceiling = ok=%v err=%v, want refused", ok, err)
	}

	at := time.Now().Truncate(time.Second)
	if err := b.Override("item-1", BudgetOverride{Reason: "operator says go", By: "amir", At: at}); err != nil {
		t.Fatal(err)
	}
	ok, _, err := b.Reserve("item-1", "2026-09-11", 1, 1)
	if err != nil || !ok {
		t.Fatalf("Reserve after Override = ok=%v err=%v, want granted despite the ceiling", ok, err)
	}
	runs, ov := b.Usage("item-1")
	if runs != 2 {
		t.Errorf("Usage runs = %d, want 2 (usage keeps counting while overridden)", runs)
	}
	if ov == nil || ov.Reason != "operator says go" || ov.By != "amir" || !ov.At.Equal(at) {
		t.Errorf("Usage override = %+v, want the recorded override", ov)
	}

	if err := b.Restore("item-1"); err != nil {
		t.Fatal(err)
	}
	if _, ov := b.Usage("item-1"); ov != nil {
		t.Errorf("override still present after Restore: %+v", ov)
	}
	if ok, reason, err := b.Reserve("item-1", "2026-09-11", 1, 1); err != nil || ok {
		t.Fatalf("Reserve after Restore = ok=%v reason=%q err=%v, want refused again (usage already at 2, ceiling is 1)", ok, reason, err)
	}

	// Restoring an item with no override is not an error.
	if err := b.Restore("item-2"); err != nil {
		t.Fatal(err)
	}
}

func TestPeekReflectsReserveWithoutSpending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	b := OpenBudgets(path)

	if ok, _ := b.Peek("item-1", "2026-09-11", 1, 0); !ok {
		t.Error("Peek on a fresh item = false, want true")
	}
	if ok, _, err := b.Reserve("item-1", "2026-09-11", 1, 0); err != nil || !ok {
		t.Fatalf("Reserve = ok=%v err=%v, want granted", ok, err)
	}
	if ok, reason := b.Peek("item-1", "2026-09-11", 1, 0); ok || reason != "item" {
		t.Errorf("Peek after the ceiling is spent = ok=%v reason=%q, want refused", ok, reason)
	}
	if runs, _ := b.Usage("item-1"); runs != 1 {
		t.Errorf("Peek must not spend a run; Usage runs = %d, want 1", runs)
	}
}

func TestBudgetsSurviveAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	b := OpenBudgets(path)

	if _, _, err := b.Reserve("item-1", "2026-09-11", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Reserve("item-1", "2026-09-11", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.Override("item-2", BudgetOverride{Reason: "r", By: "amir", At: time.Now().Truncate(time.Second)}); err != nil {
		t.Fatal(err)
	}

	reopened := OpenBudgets(path)
	if runs, _ := reopened.Usage("item-1"); runs != 2 {
		t.Errorf("item-1 usage after reopen = %d, want 2", runs)
	}
	if _, ov := reopened.Usage("item-2"); ov == nil || ov.Reason != "r" {
		t.Errorf("item-2 override after reopen = %+v, want it to survive", ov)
	}
	if got := reopened.DayUsage("2026-09-11"); got != 2 {
		t.Errorf("DayUsage after reopen = %d, want 2", got)
	}
}

func TestOpenBudgetsStartsEmptyOnAMalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budgets.json")
	if err := writeAtomic(path, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	b := OpenBudgets(path)
	if runs, ov := b.Usage("item-1"); runs != 0 || ov != nil {
		t.Errorf("Usage from a malformed file = runs=%d override=%v, want zero value", runs, ov)
	}
}

// Budgets has no Prune: MaxRunsPerItem is a lifetime ceiling, and "on the
// board" is only ever a bounded recent window (open issues plus the last
// CLOSED_LIMIT closed ones), not a durable existence check. An item that
// scrolls out of that window, or is closed and later reopened, must still
// meet the ceiling it already spent against — nothing here should offer a
// way to reset it back to zero.
func TestBudgetUsageIsNeverPrunedByBoardMembership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budgets.json")
	b := OpenBudgets(path)

	if _, _, err := b.Reserve("item-1", "2026-09-11", 1, 0); err != nil {
		t.Fatal(err)
	}
	// item-1 has since scrolled off the board's visible window entirely —
	// nothing observes that here, because Budgets has nothing that could.
	if ok, reason, err := b.Reserve("item-1", "2026-09-11", 1, 0); err != nil || ok {
		t.Fatalf("Reserve after item-1's lifetime ceiling = ok=%v reason=%q err=%v, want refused", ok, reason, err)
	}

	reopened := OpenBudgets(path)
	if runs, _ := reopened.Usage("item-1"); runs != 1 {
		t.Errorf("item-1 usage after reopen = %d, want 1 (a lifetime ceiling survives, it is never reset)", runs)
	}
}

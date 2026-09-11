package store

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// BudgetOverride is an operator's deliberate decision to let one item keep
// spending past its configured execution ceiling — the audit record CLAUDE.md
// requires of any override: who asked, why, and when. There is exactly one
// kind of scope, unlike ManualPause's global-or-source: a board-wide daily
// ceiling is a fact about today, not a standing decision a Resume undoes, so
// overriding it is always "this item in particular still gets to run", never
// "lift the ceiling for everyone".
type BudgetOverride struct {
	Reason string    `json:"reason"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at"`
}

// itemUsage is one item's own ledger: how many runs it has ever spent — a
// lifetime count that survives a restart, a stage change, even the item
// finishing and reappearing, because MaxRunsPerItem is a ceiling on total
// dispatch and not on consecutive failures the way a stage's own
// MaxAttempts already is — and the override, if an operator ever granted
// one.
type itemUsage struct {
	Runs     int             `json:"runs"`
	Override *BudgetOverride `json:"override,omitempty"`
}

// budgetsData is what Budgets persists. Day/Runs is the whole board's ledger
// for the one day it currently tracks; there is nothing to keep for a day
// that is not today, because nothing ever reads one — see Reserve.
type budgetsData struct {
	Items map[string]itemUsage `json:"items"`
	Day   string               `json:"day,omitempty"`
	Runs  int                  `json:"runs,omitempty"`
}

// Budgets holds how many times each item, and the board as a whole today,
// have actually been dispatched — the ledger claim() spends against, so an
// operator-defined execution ceiling survives a restart the way a manual
// pause does (CLAUDE.md, #39) instead of being silently forgotten.
//
// Same shape and the same two-lock discipline as Pauses and Answers (see the
// package comment): dataMu guards reads, writeMu serialises every
// check-then-increment so two concurrent claims can never both read "under
// the cap" before either records its own spend — the exact race #39's
// acceptance criteria call out.
type Budgets struct {
	path string

	dataMu sync.RWMutex
	data   budgetsData

	writeMu sync.Mutex
}

// OpenBudgets reads the ledger at path, or starts empty. A malformed file is
// an empty ledger rather than a fatal error, the same reason every store here
// does it: refusing to start would strand every repository over one
// unreadable file, and the safe direction to be wrong in is to let the
// board's limits reapply from zero, never to wedge dispatch shut.
func OpenBudgets(path string) *Budgets {
	b := &Budgets{path: path, data: budgetsData{Items: map[string]itemUsage{}}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return b
	}
	_ = json.Unmarshal(raw, &b.data)
	if b.data.Items == nil {
		b.data.Items = map[string]itemUsage{}
	}
	return b
}

// Reserve spends one run against both ceilings for itemID, atomically: the
// check and the increment happen under one lock, so a second caller racing
// the first can never read "under the cap" before the first's spend is
// visible.
//
// day is the caller's idea of "today" (UTC, "2006-01-02"), passed in rather
// than read from time.Now() so a test can drive the boundary directly. A day
// different from the one currently tracked rolls the board counter over to
// zero before checking it — a stale count from a day that has already ended
// answers nothing about today.
//
// maxPerItem or maxPerDay of zero means that ceiling does not apply. An item
// with a live override is refused by neither ceiling — the operator's
// decision stands until Restore clears it — but its usage is still recorded,
// so Usage stays honest about how much the item actually ran while
// overridden.
//
// Reports ok=false with no error and no write when a ceiling is hit: refusal
// is the ordinary case here, not a failure. A non-nil error is only ever a
// disk problem, and on one the reservation is not recorded either way — the
// caller must treat it as not granted, the same as Answers.Take's failed
// write.
func (b *Budgets) Reserve(itemID, day string, maxPerItem, maxPerDay int) (ok bool, reason string, err error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	next := b.snapshot()
	u := next.Items[itemID]
	if u.Override == nil {
		if maxPerItem > 0 && u.Runs >= maxPerItem {
			return false, "item", nil
		}
		if maxPerDay > 0 && next.Day == day && next.Runs >= maxPerDay {
			return false, "day", nil
		}
	}
	if next.Day != day {
		next.Day, next.Runs = day, 0
	}
	u.Runs++
	next.Items[itemID] = u
	next.Runs++
	if err := b.persist(next); err != nil {
		return false, "", err
	}
	b.dataMu.Lock()
	b.data = next
	b.dataMu.Unlock()
	return true, "", nil
}

// Peek reports whether itemID has room under both ceilings right now,
// without spending anything — the same "fact about the world" pre-filter
// manuallyPaused and agentPaused already are for launch's free-item pass.
// Reserve is what actually spends a run, atomically; a race between this
// read and that reservation can only ever cost an extra scheduler pass, never
// overspend a ceiling.
func (b *Budgets) Peek(itemID, day string, maxPerItem, maxPerDay int) (ok bool, reason string) {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	u := b.data.Items[itemID]
	if u.Override != nil {
		return true, ""
	}
	if maxPerItem > 0 && u.Runs >= maxPerItem {
		return false, "item"
	}
	if maxPerDay > 0 && b.data.Day == day && b.data.Runs >= maxPerDay {
		return false, "day"
	}
	return true, ""
}

// Usage reports one item's own ledger: how many runs it has spent, all time,
// and its override, if it has one.
func (b *Budgets) Usage(itemID string) (runs int, override *BudgetOverride) {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	u := b.data.Items[itemID]
	return u.Runs, u.Override
}

// DayUsage reports how many runs the whole board has spent on day (UTC,
// "2006-01-02"). A day other than the one currently tracked reports zero:
// nothing is kept past the current day, so there is nothing else to report.
func (b *Budgets) DayUsage(day string) int {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	if b.data.Day != day {
		return 0
	}
	return b.data.Runs
}

// Override records an operator's deliberate decision to let one item keep
// spending past its configured ceilings, replacing any existing override for
// it. Usage is untouched: an override lifts the ceiling, it does not reset
// what the item has already spent.
func (b *Budgets) Override(itemID string, o BudgetOverride) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	next := b.snapshot()
	u := next.Items[itemID]
	ov := o
	u.Override = &ov
	next.Items[itemID] = u
	if err := b.persist(next); err != nil {
		return err
	}
	b.dataMu.Lock()
	b.data = next
	b.dataMu.Unlock()
	return nil
}

// Restore lifts an item's override, returning it to its configured
// ceilings. Idempotent: restoring an item with no override is not an error,
// the same as resuming a scope ManualPause never paused.
func (b *Budgets) Restore(itemID string) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	next := b.snapshot()
	u, ok := next.Items[itemID]
	if !ok || u.Override == nil {
		return nil
	}
	u.Override = nil
	next.Items[itemID] = u
	if err := b.persist(next); err != nil {
		return err
	}
	b.dataMu.Lock()
	b.data = next
	b.dataMu.Unlock()
	return nil
}

// Prune drops usage records for items no longer on the board — the same
// lifetime discipline Times, TransitionErrors, AnswerInfo and Cancels already
// follow: once an item is gone, there is nothing left for its own ledger to
// explain, and an id GitHub reuses (it never does, but nothing here relies on
// that) simply starts a fresh ledger the way a brand new item would.
func (b *Budgets) Prune(onBoard map[string]bool) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	next := b.snapshot()
	changed := false
	for id := range next.Items {
		if !onBoard[id] {
			delete(next.Items, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := b.persist(next); err != nil {
		return err
	}
	b.dataMu.Lock()
	b.data = next
	b.dataMu.Unlock()
	return nil
}

// snapshot copies the current ledger. Called only from within a writeMu
// critical section, so the copy it starts from cannot change under it before
// persist writes it out.
func (b *Budgets) snapshot() budgetsData {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	items := make(map[string]itemUsage, len(b.data.Items))
	for k, v := range b.data.Items {
		items[k] = v
	}
	return budgetsData{Items: items, Day: b.data.Day, Runs: b.data.Runs}
}

func (b *Budgets) persist(d budgetsData) error {
	bb, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(b.path, append(bb, '\n'))
}

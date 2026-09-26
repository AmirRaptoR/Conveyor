package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const budgetLedgerVersion = 2

const budgetAuditLimit = 20

// BudgetOverride is a one-run operator grant. Stage is part of its scope;
// Remaining is either one or zero, while the record remains as an audit after
// it is spent.
type BudgetOverride struct {
	Stage     string    `json:"stage,omitempty"`
	Reason    string    `json:"reason"`
	By        string    `json:"by,omitempty"`
	At        time.Time `json:"at"`
	Remaining int       `json:"remaining"`
	UsedAt    time.Time `json:"usedAt,omitempty"`
	RevokedAt time.Time `json:"revokedAt,omitempty"`
	RevokedBy string    `json:"revokedBy,omitempty"`
}

// FailureState is the durable deterministic-failure gate for one item. It is
// internal scheduler state, not a provider mark: the item remains in Stage and
// unrelated work remains eligible.
type FailureState struct {
	Stage            string    `json:"stage"`
	Signature        string    `json:"signature"`
	Reason           string    `json:"reason"`
	RunID            string    `json:"runId"`
	Count            int       `json:"count"`
	Held             bool      `json:"held"`
	Quarantined      bool      `json:"quarantined"`
	At               time.Time `json:"at"`
	Evidence         string    `json:"evidence,omitempty"`
	ReleaseCondition string    `json:"releaseCondition"`
	Permits          int       `json:"permits,omitempty"`
}

type itemUsage struct {
	Runs                 int              `json:"runs"`
	Override             *BudgetOverride  `json:"override,omitempty"`
	OverrideHistory      []BudgetOverride `json:"overrideHistory,omitempty"`
	Failure              *FailureState    `json:"failure,omitempty"`
	FailureHistory       []FailureState   `json:"failureHistory,omitempty"`
	ProcessedFailureRuns []string         `json:"processedFailureRuns,omitempty"`
}

type budgetsData struct {
	Version int                  `json:"version"`
	Items   map[string]itemUsage `json:"items"`
	Day     string               `json:"day,omitempty"`
	Runs    int                  `json:"runs,omitempty"`
	Reset   *BudgetReset         `json:"reset,omitempty"`
}

type BudgetReset struct {
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
	Archive string    `json:"archive,omitempty"`
}

// ResetBudgets archives any existing ledger and starts an explicit v2 model-
// run ledger. It is intentionally a command-level operation, never startup
// fallback: old dispatch counts cannot be inferred into model counts.
func ResetBudgets(path, reason string, at time.Time) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", errors.New("reset reason is required")
	}
	archive := ""
	if _, err := os.Stat(path); err == nil {
		archive = path + ".reset-" + at.UTC().Format("20060102T150405.000000000Z")
		if _, err := os.Stat(archive); err == nil {
			return "", fmt.Errorf("budget archive %s already exists", archive)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := os.Rename(path, archive); err != nil {
			return "", fmt.Errorf("archive old budget ledger: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	data := budgetsData{Version: budgetLedgerVersion, Items: map[string]itemUsage{}, Reset: &BudgetReset{
		Reason: reason, At: at.UTC(), Archive: filepath.Base(archive),
	}}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeAtomic(path, append(b, '\n')); err != nil {
		if archive != "" {
			_ = os.Rename(archive, path)
		}
		return "", err
	}
	return archive, nil
}

// Budgets is the model-run ledger and deterministic-failure gate. Its version
// changed when the old all-transition counts became model-only counts; an old
// file cannot be converted truthfully and therefore requires budget-reset.
type Budgets struct {
	path    string
	loadErr error

	dataMu sync.RWMutex
	data   budgetsData

	writeMu sync.Mutex
}

func OpenBudgets(path string) *Budgets {
	b := &Budgets{path: path, data: budgetsData{Version: budgetLedgerVersion, Items: map[string]itemUsage{}}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return b
	}
	if err != nil {
		b.loadErr = fmt.Errorf("read model-run budget ledger: %w", err)
		return b
	}
	var data budgetsData
	if err := json.Unmarshal(raw, &data); err != nil {
		b.loadErr = fmt.Errorf("model-run budget ledger is unreadable; run `conveyor budget-reset -reason <why>` after preserving %s: %w", path, err)
		return b
	}
	if data.Version != budgetLedgerVersion {
		b.loadErr = fmt.Errorf("budget ledger %s is version %d; version %d counts model runs only and cannot infer them from the old dispatch ledger; run `conveyor budget-reset -reason <why>`", path, data.Version, budgetLedgerVersion)
		return b
	}
	if data.Items == nil {
		data.Items = map[string]itemUsage{}
	}
	b.data = data
	return b
}

func (b *Budgets) Err() error { return b.loadErr }

// Reserve is retained for callers that do not need a stage scope.
func (b *Budgets) Reserve(itemID, day string, maxPerItem, maxPerDay int) (bool, string, error) {
	return b.ReserveModel(itemID, "", day, maxPerItem, maxPerDay)
}

// ReserveModel atomically spends one model run. A scoped grant permits exactly
// one run and is consumed in the same write as the reservation.
func (b *Budgets) ReserveModel(itemID, stage, day string, maxPerItem, maxPerDay int) (ok bool, reason string, err error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.loadErr != nil {
		return false, "error", b.loadErr
	}
	next := b.snapshot()
	u := next.Items[itemID]
	grant := u.Override != nil && u.Override.Remaining > 0 && u.Override.Stage == stage
	releasePermit := u.Failure != nil && !u.Failure.Held && u.Failure.Stage == stage && u.Failure.Permits > 0
	if !grant {
		if u.Failure != nil && u.Failure.Held && u.Failure.Stage == stage {
			if u.Failure.Quarantined {
				return false, "quarantine", nil
			}
			return false, "evidence", nil
		}
		if maxPerItem > 0 && u.Runs >= maxPerItem {
			return false, "item", nil
		}
		if maxPerDay > 0 && next.Day == day && next.Runs >= maxPerDay {
			return false, "day", nil
		}
	} else {
		u.Override.Remaining--
		u.Override.UsedAt = time.Now().UTC()
		u.OverrideHistory = replaceLastOverride(u.OverrideHistory, *u.Override)
	}
	if releasePermit {
		u.Failure.Permits--
		u.Failure.Held = true
		u.Failure.ReleaseCondition = fmt.Sprintf("item.updatedAt must change from %q before another automatic attempt", u.Failure.Evidence)
	}
	if next.Day != day {
		next.Day, next.Runs = day, 0
	}
	u.Runs++
	next.Items[itemID] = u
	next.Runs++
	if err := b.commit(next); err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (b *Budgets) Peek(itemID, day string, maxPerItem, maxPerDay int) (bool, string) {
	return b.PeekModel(itemID, "", day, maxPerItem, maxPerDay)
}

func (b *Budgets) PeekModel(itemID, stage, day string, maxPerItem, maxPerDay int) (bool, string) {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	if b.loadErr != nil {
		return false, "error"
	}
	u := b.data.Items[itemID]
	if u.Override != nil && u.Override.Remaining > 0 && u.Override.Stage == stage {
		return true, ""
	}
	if u.Failure != nil && u.Failure.Held && u.Failure.Stage == stage {
		if u.Failure.Quarantined {
			return false, "quarantine"
		}
		return false, "evidence"
	}
	if maxPerItem > 0 && u.Runs >= maxPerItem {
		return false, "item"
	}
	if maxPerDay > 0 && b.data.Day == day && b.data.Runs >= maxPerDay {
		return false, "day"
	}
	return true, ""
}

func (b *Budgets) Usage(itemID string) (int, *BudgetOverride) {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	u := b.data.Items[itemID]
	var ov *BudgetOverride
	if u.Override != nil && u.Override.RevokedAt.IsZero() {
		copy := *u.Override
		ov = &copy
	}
	return u.Runs, ov
}

func (b *Budgets) Failure(itemID string) (FailureState, bool) {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	f := b.data.Items[itemID].Failure
	if f == nil {
		return FailureState{}, false
	}
	return *f, true
}

func (b *Budgets) OverrideHistory(itemID string) []BudgetOverride {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	return append([]BudgetOverride(nil), b.data.Items[itemID].OverrideHistory...)
}

func (b *Budgets) FailureHistory(itemID string) []FailureState {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	return append([]FailureState(nil), b.data.Items[itemID].FailureHistory...)
}

func (b *Budgets) HasFailureRun(itemID, runID string) bool {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	u := b.data.Items[itemID]
	if u.Failure != nil && u.Failure.RunID == runID {
		return true
	}
	for _, failure := range u.FailureHistory {
		if failure.RunID == runID {
			return true
		}
	}
	for _, id := range u.ProcessedFailureRuns {
		if id == runID {
			return true
		}
	}
	return false
}

// ProcessFailureRun records that provider state moved past a completed run
// before it could be entered in the active failure ledger. This closes the
// crash window without making a historical run authoritative if the item later
// returns to the same stage.
func (b *Budgets) ProcessFailureRun(itemID, runID string) error {
	if runID == "" {
		return nil
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.loadErr != nil {
		return b.loadErr
	}
	next := b.snapshot()
	u := next.Items[itemID]
	if u.Failure != nil && u.Failure.RunID == runID {
		return nil
	}
	for _, failure := range u.FailureHistory {
		if failure.RunID == runID {
			return nil
		}
	}
	for _, id := range u.ProcessedFailureRuns {
		if id == runID {
			return nil
		}
	}
	// These are correctness tombstones, not display audit. Evicting an old run
	// lets it become authoritative again if the item later returns to its stage.
	u.ProcessedFailureRuns = append(u.ProcessedFailureRuns, runID)
	next.Items[itemID] = u
	return b.commit(next)
}

func (b *Budgets) DayUsage(day string) int {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	if b.data.Day != day {
		return 0
	}
	return b.data.Runs
}

// Override keeps the old API but scopes the grant to the empty stage.
func (b *Budgets) Override(itemID string, o BudgetOverride) error { return b.Grant(itemID, "", o) }

func (b *Budgets) Grant(itemID, stage string, o BudgetOverride) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.loadErr != nil {
		return b.loadErr
	}
	next := b.snapshot()
	u := next.Items[itemID]
	if u.Override != nil && u.Override.Remaining > 0 {
		prior := *u.Override
		prior.Remaining = 0
		prior.RevokedAt = o.At.UTC()
		prior.RevokedBy = o.By
		u.OverrideHistory = replaceLastOverride(u.OverrideHistory, prior)
	}
	o.Stage, o.Remaining = stage, 1
	o.At = o.At.UTC()
	o.UsedAt = time.Time{}
	o.RevokedAt = time.Time{}
	o.RevokedBy = ""
	u.Override = &o
	u.OverrideHistory = appendBounded(u.OverrideHistory, o, budgetAuditLimit)
	next.Items[itemID] = u
	return b.commit(next)
}

func (b *Budgets) Restore(itemID string) error {
	return b.Revoke(itemID, "", time.Now())
}

func (b *Budgets) Revoke(itemID, by string, at time.Time) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.loadErr != nil {
		return b.loadErr
	}
	next := b.snapshot()
	u, ok := next.Items[itemID]
	if !ok || u.Override == nil || !u.Override.RevokedAt.IsZero() {
		return nil
	}
	o := *u.Override
	o.Remaining = 0
	o.RevokedAt = at.UTC()
	o.RevokedBy = by
	u.Override = &o
	u.OverrideHistory = replaceLastOverride(u.OverrideHistory, o)
	next.Items[itemID] = u
	return b.commit(next)
}

func (b *Budgets) RecordFailure(itemID, stage, signature, reason, runID string, threshold int, at time.Time) (FailureState, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.loadErr != nil {
		return FailureState{}, b.loadErr
	}
	next := b.snapshot()
	u := next.Items[itemID]
	if u.Failure != nil && u.Failure.RunID == runID {
		return *u.Failure, nil
	}
	count := 1
	if u.Failure != nil && u.Failure.Stage == stage && u.Failure.Signature == signature {
		count = u.Failure.Count + 1
	}
	f := FailureState{
		Stage: stage, Signature: signature, Reason: reason, RunID: runID,
		Count: count, Held: true, Quarantined: count >= threshold, At: at.UTC(),
		ReleaseCondition: "waiting for a fresh provider item version to establish the release baseline",
	}
	u.Failure = &f
	next.Items[itemID] = u
	if err := b.commit(next); err != nil {
		return FailureState{}, err
	}
	return f, nil
}

// ReconcileStage removes a failure from the active gate when provider state
// says the item is elsewhere. The record remains in bounded audit history, so
// returning to the old stage does not resurrect stale evidence.
func (b *Budgets) ReconcileStage(itemID, stage string) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.loadErr != nil {
		return b.loadErr
	}
	next := b.snapshot()
	u := next.Items[itemID]
	if u.Failure == nil || u.Failure.Stage == stage {
		return nil
	}
	u.FailureHistory = appendBounded(u.FailureHistory, *u.Failure, budgetAuditLimit)
	u.Failure = nil
	next.Items[itemID] = u
	return b.commit(next)
}

// ObserveExternal establishes the first post-failure provider version as the
// baseline, then releases exactly one attempt only when a later fresh listing
// reports a different version.
func (b *Budgets) ObserveExternal(itemID, stage, evidence string, observedAt time.Time) (bool, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.loadErr != nil {
		return false, b.loadErr
	}
	next := b.snapshot()
	u := next.Items[itemID]
	if u.Failure == nil || !u.Failure.Held || u.Failure.Stage != stage || !observedAt.After(u.Failure.At) {
		return false, nil
	}
	f := *u.Failure
	if evidence == "" {
		if f.ReleaseCondition != "provider supplies no updatedAt; only a scoped operator override can release one run" {
			f.ReleaseCondition = "provider supplies no updatedAt; only a scoped operator override can release one run"
			u.Failure = &f
			next.Items[itemID] = u
			return false, b.commit(next)
		}
		return false, nil
	}
	if f.Evidence == "" {
		f.Evidence = evidence
		f.ReleaseCondition = fmt.Sprintf("item.updatedAt must change from %q", evidence)
		u.Failure = &f
		next.Items[itemID] = u
		return false, b.commit(next)
	}
	if f.Evidence == evidence {
		return false, nil
	}
	f.Held = false
	f.Permits = 1
	f.ReleaseCondition = fmt.Sprintf("one attempt eligible because item.updatedAt changed from %q to %q", f.Evidence, evidence)
	f.Evidence = evidence
	u.Failure = &f
	next.Items[itemID] = u
	return true, b.commit(next)
}

func (b *Budgets) ClearFailure(itemID, stage string) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.loadErr != nil {
		return b.loadErr
	}
	next := b.snapshot()
	u := next.Items[itemID]
	if u.Failure == nil || u.Failure.Stage != stage {
		return nil
	}
	u.FailureHistory = appendBounded(u.FailureHistory, *u.Failure, budgetAuditLimit)
	u.Failure = nil
	next.Items[itemID] = u
	return b.commit(next)
}

func (b *Budgets) snapshot() budgetsData {
	b.dataMu.RLock()
	defer b.dataMu.RUnlock()
	items := make(map[string]itemUsage, len(b.data.Items))
	for k, v := range b.data.Items {
		if v.Override != nil {
			o := *v.Override
			v.Override = &o
		}
		if v.Failure != nil {
			f := *v.Failure
			v.Failure = &f
		}
		v.OverrideHistory = append([]BudgetOverride(nil), v.OverrideHistory...)
		v.FailureHistory = append([]FailureState(nil), v.FailureHistory...)
		v.ProcessedFailureRuns = append([]string(nil), v.ProcessedFailureRuns...)
		items[k] = v
	}
	return budgetsData{Version: budgetLedgerVersion, Items: items, Day: b.data.Day, Runs: b.data.Runs, Reset: b.data.Reset}
}

func replaceLastOverride(history []BudgetOverride, o BudgetOverride) []BudgetOverride {
	if len(history) == 0 {
		return append(history, o)
	}
	history[len(history)-1] = o
	return history
}

func appendBounded[T any](history []T, v T, limit int) []T {
	history = append(history, v)
	if len(history) > limit {
		history = append([]T(nil), history[len(history)-limit:]...)
	}
	return history
}

func (b *Budgets) commit(next budgetsData) error {
	bb, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(b.path, append(bb, '\n')); err != nil {
		return err
	}
	b.dataMu.Lock()
	b.data = next
	b.dataMu.Unlock()
	return nil
}

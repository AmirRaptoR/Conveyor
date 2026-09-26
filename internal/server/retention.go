package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// SweepResult is what one retention pass did, reported in a single summary
// line (CONTRACTS §6): how many runs were deleted, how many bytes reclaimed,
// which survived because something pinned them, and the horizon that
// resulted.
type SweepResult struct {
	Deleted    int
	BytesFreed int64
	// Pinned names each surviving run individually, with why, so pinned
	// evidence cannot pile up unnoticed.
	Pinned []string
	// DeletedRuns is the id of every run directory actually removed, so a
	// caller holding an in-memory index keyed by run id (a cached plan
	// entry, say) can evict exactly what retention just deleted rather than
	// re-deriving it from Deleted's count.
	DeletedRuns []string
	Horizon     time.Time
}

// sweepRoot deletes run directories whose day directory is strictly before
// cutoff's UTC date, except any pinned reports true for. Comparing days, not
// timestamps, lets whole days be skipped without reading a single meta.json —
// conservative by up to 24 hours in the safe direction. meta.json is read
// only for runs in a candidate (expired) day, to evaluate pinning.
//
// A directory left empty by the sweep is removed; one still holding a pinned
// run is not. Errors reading or deleting one directory are collected and
// returned rather than silently skipped, and do not stop the rest of the
// sweep — nor do they cause anything unreadable to be deleted: an entry the
// sweep could not evaluate is kept, on the same reasoning as a stubbed-full
// disk being an operational fault rather than a silent no-op.
func sweepRoot(root string, cutoff time.Time, pinned func(RunMeta) (bool, string)) (SweepResult, error) {
	res := SweepResult{Horizon: cutoff.UTC()}
	cutoffDay := cutoff.UTC().Format("2006-01-02")

	days, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, err
	}

	var errs []error
	for _, day := range days {
		if !day.IsDir() || day.Name() >= cutoffDay {
			continue
		}
		dayDir := filepath.Join(root, day.Name())
		entries, err := os.ReadDir(dayDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", dayDir, err))
			continue
		}
		keepDay := false
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			runDir := filepath.Join(dayDir, e.Name())
			metaPath := filepath.Join(runDir, "meta.json")
			b, err := os.ReadFile(metaPath)
			if err != nil {
				errs = append(errs, fmt.Errorf("read %s: %w", metaPath, err))
				keepDay = true
				continue
			}
			var m RunMeta
			if json.Unmarshal(b, &m) != nil {
				errs = append(errs, fmt.Errorf("parse %s: invalid JSON", metaPath))
				keepDay = true
				continue
			}
			m.ID = e.Name()
			m.Dir = runDir
			if m.Outcome == model.OutcomeRunning {
				keepDay = true
				res.Pinned = append(res.Pinned, fmt.Sprintf("%s (still running)", m.ID))
				continue
			}
			if keep, reason := pinned(m); keep {
				keepDay = true
				res.Pinned = append(res.Pinned, fmt.Sprintf("%s (%s)", m.ID, reason))
				continue
			}
			sz := dirSize(runDir)
			if err := os.RemoveAll(runDir); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", runDir, err))
				keepDay = true
				continue
			}
			res.Deleted++
			res.BytesFreed += sz
			res.DeletedRuns = append(res.DeletedRuns, m.ID)
		}
		if !keepDay {
			if err := os.Remove(dayDir); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove empty day %s: %w", dayDir, err))
			}
		}
	}
	return res, errors.Join(errs...)
}

// dirSize sums file sizes under dir without reading file content.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// pinnedFunc is the retention pin predicate for the board's current state,
// evaluated fresh for each sweep: a run is pinned if it is the run recorded
// against a currently marked item's Block, or a currently-listed item's
// ItemTime — both already pruned to on-board items only every refresh
// (CONTRACTS §6's "evaluated against the board's current items only"). The
// running-outcome rule lives in sweepRoot itself, since it needs no item at
// all.
func (s *Server) pinnedFunc() func(RunMeta) (bool, string) {
	s.mu.RLock()
	byBlock := make(map[string]string, len(s.blocks))
	for itemID, b := range s.blocks {
		if b.RunID != "" {
			byBlock[b.RunID] = itemID
		}
	}
	byTime := make(map[string]string, len(s.times))
	for itemID, t := range s.times {
		if t.RunID != "" {
			byTime[t.RunID] = itemID
		}
	}
	s.mu.RUnlock()
	return func(m RunMeta) (bool, string) {
		if itemID, ok := byBlock[m.ID]; ok {
			return true, fmt.Sprintf("marked evidence for %s", itemID)
		}
		if itemID, ok := byTime[m.ID]; ok {
			return true, fmt.Sprintf("current-stage arrival for %s", itemID)
		}
		return false, ""
	}
}

// runSweep runs one retention pass and logs its summary line to w — always,
// even when nothing was deleted, so a sweep that ran and found nothing to do
// is as visible as one that did.
func (s *Server) runSweep(w io.Writer) {
	now := time.Now()
	s.runStoreMu.Lock()
	ready := s.retentionIsReady()
	res := SweepResult{}
	var err error
	if ready {
		res, err = sweepClasses(s.run.Root, s.cfg, now, s.pinnedFunc())
	}
	tempFreed, tempErr := runner.SweepTemp(s.run.TempRoot, now.Add(-24*time.Hour))
	payloadFreed, payloadErr := s.run.SweepPayloads(now.Add(-24 * time.Hour))
	res.BytesFreed += tempFreed + payloadFreed
	err = errors.Join(err, tempErr, payloadErr)

	s.mu.Lock()
	if res.Deleted > 0 {
		s.everSwept = true
		s.runStoreGen++
	}
	if ready {
		s.sweepHorizon = res.Horizon
	}
	s.lastCleanup = now
	if len(res.DeletedRuns) > 0 {
		deleted := make(map[string]bool, len(res.DeletedRuns))
		for _, id := range res.DeletedRuns {
			deleted[id] = true
		}
		// A swept run yields no plan: evict the cached card entry the same
		// way a recovered one would find nothing, rather than leaving a
		// summary on the board that points at a run directory no longer
		// there to back it up.
		for itemID, p := range s.plans {
			if deleted[p.RunID] {
				delete(s.plans, itemID)
				s.planGeneration[itemID]++
			}
		}
		for itemID, p := range s.planMisses {
			if deleted[p.RunID] {
				delete(s.planMisses, itemID)
				s.planGeneration[itemID]++
			}
		}
		for itemID, v := range s.steering {
			if deleted[v.RunID] {
				delete(s.steering, itemID)
				s.steeringGen[itemID]++
			}
		}
		for itemID, v := range s.steeringMisses {
			if deleted[v.RunID] {
				delete(s.steeringMisses, itemID)
				s.steeringGen[itemID]++
			}
		}
	}
	s.mu.Unlock()
	s.runStoreMu.Unlock()
	storage := s.storageUse()
	s.mu.Lock()
	s.state.Storage = storage
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})

	fmt.Fprintf(w, "conveyor: retention sweep: deleted %d run(s), %d bytes reclaimed, %d pinned, horizon %s\n",
		res.Deleted, res.BytesFreed, len(res.Pinned), res.Horizon.Format("2006-01-02"))
	for _, p := range res.Pinned {
		fmt.Fprintf(w, "conveyor: retention sweep: pinned %s\n", p)
	}
	if err != nil {
		fmt.Fprintf(w, "conveyor: retention sweep had errors: %v\n", err)
	}
	if !ready {
		fmt.Fprintln(w, "conveyor: retention sweep: run deletion deferred until initial source evidence is reconstructed")
	}
}

func (s *Server) retentionIsReady() bool {
	select {
	case <-s.retentionReady:
		return true
	default:
		return false
	}
}

// sweep runs the retention sweep once at startup and then daily at
// storage.sweepAt, parsed in the process's own local time zone, until ctx ends.
// Callers gate this on auto: a server started with -watch only observes, and
// must not delete anything either.
func (s *Server) sweep(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-s.retentionReady:
	}
	s.runSweep(os.Stderr)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(untilNextSweepAt(s.cfg.Storage.SweepAt, time.Now())):
			s.runSweep(os.Stderr)
		}
	}
}

// untilNextSweepAt is how long until the next occurrence of hhmm (HH:MM,
// already validated at config load) in now's own location.
func untilNextSweepAt(hhmm string, now time.Time) time.Duration {
	var h, m int
	fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}

// StorageView is what the data and scratch stores currently hold. Run metadata
// is decoded only to assign each directory's bytes to its persisted class.
type StorageView struct {
	Bytes             int64            `json:"bytes"`
	TemporaryBytes    int64            `json:"temporaryBytes"`
	Runs              int              `json:"runs"`
	OldestDay         string           `json:"oldestDay,omitempty"`
	ByClass           map[string]int64 `json:"byClass"`
	ProjectedGrowth   int64            `json:"projectedGrowth"`
	MaxBytes          int64            `json:"maxBytes"`
	TempMaxBytes      int64            `json:"tempMaxBytes"`
	HighWatermark     int              `json:"highWatermark"`
	CriticalWatermark int              `json:"criticalWatermark"`
	Level             string           `json:"level"`
	LastCleanup       time.Time        `json:"lastCleanup,omitempty"`
}

func (s *Server) storageUse() StorageView {
	v := StorageView{ByClass: map[string]int64{}, MaxBytes: int64(s.cfg.Storage.MaxBytes), TempMaxBytes: int64(s.cfg.Storage.TempMaxBytes), HighWatermark: s.cfg.Storage.HighWatermark, CriticalWatermark: s.cfg.Storage.CriticalWatermark}
	s.mu.RLock()
	v.LastCleanup = s.lastCleanup
	s.mu.RUnlock()
	now := time.Now()
	days, err := os.ReadDir(s.run.Root)
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		if v.OldestDay == "" || day.Name() < v.OldestDay {
			v.OldestDay = day.Name()
		}
		dayDir := filepath.Join(s.run.Root, day.Name())
		entries, err := os.ReadDir(dayDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				v.Runs++
				dir := filepath.Join(dayDir, e.Name())
				sz := dirSize(dir)
				b, _ := os.ReadFile(filepath.Join(dir, "meta.json"))
				var m model.Run
				if json.Unmarshal(b, &m) == nil {
					v.ByClass[runClass(s.cfg, m)] += sz
				}
			}
		}
	}
	_ = err
	dataRoot := s.cfg.DataDir()
	payloadRoot := filepath.Join(filepath.Dir(s.run.Root), "payloads")
	v.Bytes = dirSize(dataRoot)
	v.ProjectedGrowth = recentSize(dataRoot, now.Add(-24*time.Hour))
	for _, root := range []string{s.run.Root, payloadRoot, s.run.TempRoot} {
		if !pathWithin(dataRoot, root) {
			v.Bytes += dirSize(root)
			v.ProjectedGrowth += recentSize(root, now.Add(-24*time.Hour))
		}
	}
	v.TemporaryBytes = dirSize(s.run.TempRoot)
	var accounted int64
	for _, n := range v.ByClass {
		accounted += n
	}
	v.ByClass["payloads"] = dirSize(payloadRoot)
	v.ByClass["scratch"] = v.TemporaryBytes
	accounted += v.ByClass["payloads"] + v.ByClass["scratch"]
	if storeBytes := v.Bytes - accounted; storeBytes > 0 {
		v.ByClass["store"] = storeBytes
	}
	pct := 0
	if v.MaxBytes > 0 {
		pct = int(v.Bytes * 100 / v.MaxBytes)
	}
	tmpPct := 0
	if v.TempMaxBytes > 0 {
		tmpPct = int(v.TemporaryBytes * 100 / v.TempMaxBytes)
	}
	if tmpPct > pct {
		pct = tmpPct
	}
	switch {
	case pct >= v.CriticalWatermark:
		v.Level = "critical"
	case pct >= v.HighWatermark:
		v.Level = "high"
	default:
		v.Level = "ok"
	}
	return v
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func recentSize(root string, since time.Time) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(since) {
			total += info.Size()
		}
		return nil
	})
	return total
}

func runClass(cfg *config.Config, m model.Run) string {
	if m.Outcome == model.OutcomeFailure || m.Outcome == model.OutcomeBlocked || m.Outcome == model.OutcomeTimeout || m.Outcome == model.OutcomeInterrupted {
		return "failure"
	}
	if m.RetentionClass == "model" || m.RetentionClass == "status" || m.RetentionClass == "polling" {
		return m.RetentionClass
	}
	if m.Kind == "stage" && cfg.AgentFor(m.Source, m.To) != "" {
		return "model"
	}
	if m.Kind == "status" {
		return "status"
	}
	return "polling"
}

func retentionFor(cfg *config.Config, class string) time.Duration {
	switch class {
	case "failure":
		return cfg.Storage.Retention.Failure.D()
	case "model":
		return cfg.Storage.Retention.Model.D()
	case "status":
		return cfg.Storage.Retention.Status.D()
	default:
		return cfg.Storage.Retention.Polling.D()
	}
}

func sweepClasses(root string, cfg *config.Config, now time.Time, pinned func(RunMeta) (bool, string)) (SweepResult, error) {
	res := SweepResult{Horizon: now.UTC()}
	days, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	var errs []error
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		// Today's directory may contain a run between mkdir and its first
		// atomic metadata write. No retention class can expire today.
		if day.Name() >= now.UTC().Format("2006-01-02") {
			continue
		}
		dayDir := filepath.Join(root, day.Name())
		entries, readErr := os.ReadDir(dayDir)
		if readErr != nil {
			errs = append(errs, readErr)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(dayDir, entry.Name())
			b, readErr := os.ReadFile(filepath.Join(dir, "meta.json"))
			if readErr != nil {
				errs = append(errs, readErr)
				continue
			}
			var m RunMeta
			if json.Unmarshal(b, &m) != nil {
				errs = append(errs, fmt.Errorf("parse %s: invalid JSON", filepath.Join(dir, "meta.json")))
				continue
			}
			m.ID, m.Dir = entry.Name(), dir
			if m.Outcome == model.OutcomeRunning {
				res.Pinned = append(res.Pinned, m.ID+" (still running)")
				continue
			}
			if keep, why := pinned(m); keep {
				res.Pinned = append(res.Pinned, fmt.Sprintf("%s (%s)", m.ID, why))
				continue
			}
			started := m.StartedAt
			if started.IsZero() {
				started, _ = time.Parse("2006-01-02", day.Name())
			}
			if !started.Before(now.Add(-retentionFor(cfg, runClass(cfg, m.Run)))) {
				continue
			}
			sz := dirSize(dir)
			if removeErr := os.RemoveAll(dir); removeErr != nil {
				errs = append(errs, removeErr)
				continue
			}
			res.Deleted++
			res.BytesFreed += sz
			res.DeletedRuns = append(res.DeletedRuns, m.ID)
		}
		left, _ := os.ReadDir(dayDir)
		if len(left) == 0 {
			_ = os.Remove(dayDir)
		}
	}
	return res, errors.Join(errs...)
}

const minimumModelStorageAllowance int64 = 1 << 20

func (s *Server) reserveModelStorage(itemID string) bool {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	s.mu.RLock()
	persistFault := s.state.PersistFault != nil
	s.mu.RUnlock()
	if persistFault {
		return false
	}
	v := s.storageUse()
	if v.Level == "critical" {
		s.runSweep(io.Discard)
		v = s.storageUse()
	}
	allowance := v.ProjectedGrowth
	highBytes := v.MaxBytes * int64(v.HighWatermark) / 100
	if floor := min(minimumModelStorageAllowance, highBytes/20); allowance < floor {
		allowance = floor
	}
	var reserved int64
	for _, n := range s.storageReserved {
		reserved += n
	}
	tempHighBytes := v.TempMaxBytes * int64(v.HighWatermark) / 100
	if v.Level != "ok" || v.Bytes+reserved+allowance >= highBytes ||
		v.TemporaryBytes+reserved+allowance >= tempHighBytes {
		return false
	}
	s.storageReserved[itemID] = allowance
	return true
}

func (s *Server) releaseModelStorage(itemID string) {
	s.storageMu.Lock()
	delete(s.storageReserved, itemID)
	s.storageMu.Unlock()
}

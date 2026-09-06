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
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
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
	Pinned  []string
	Horizon time.Time
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
		}
		if !keepDay {
			if err := os.Remove(dayDir); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove empty day %s: %w", dayDir, err))
			}
		}
	}
	return res, errors.Join(errs...)
}

// dirSize sums file sizes under dir without reading any file's content —
// exactly the cost /api/state's storage figure is allowed to pay, and the
// cost retention itself pays only for a run it has decided to delete.
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
	cutoff := time.Now().Add(-s.cfg.Logs.Retention.D())
	res, err := sweepRoot(s.run.Root, cutoff, s.pinnedFunc())

	s.mu.Lock()
	if res.Deleted > 0 {
		s.everSwept = true
	}
	s.sweepHorizon = res.Horizon
	s.mu.Unlock()

	fmt.Fprintf(w, "conveyor: retention sweep: deleted %d run(s), %d bytes reclaimed, %d pinned, horizon %s\n",
		res.Deleted, res.BytesFreed, len(res.Pinned), res.Horizon.Format("2006-01-02"))
	for _, p := range res.Pinned {
		fmt.Fprintf(w, "conveyor: retention sweep: pinned %s\n", p)
	}
	if err != nil {
		fmt.Fprintf(w, "conveyor: retention sweep had errors: %v\n", err)
	}
}

// sweep runs the retention sweep once at startup and then daily at
// logs.sweepAt, parsed in the process's own local time zone, until ctx ends.
// Callers gate this on auto: a server started with -watch only observes, and
// must not delete anything either.
func (s *Server) sweep(ctx context.Context) {
	s.runSweep(os.Stderr)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(untilNextSweepAt(s.cfg.Logs.SweepAt, time.Now())):
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

// StorageView is what the run store currently holds, computed by walking
// directory and file sizes only — never by decoding a single meta.json,
// which is the cost retention exists to bound.
type StorageView struct {
	Bytes     int64  `json:"bytes"`
	Runs      int    `json:"runs"`
	OldestDay string `json:"oldestDay,omitempty"`
}

func (s *Server) storageUse() StorageView {
	var v StorageView
	days, err := os.ReadDir(s.run.Root)
	if err != nil {
		return v
	}
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
			}
		}
		v.Bytes += dirSize(dayDir)
	}
	return v
}

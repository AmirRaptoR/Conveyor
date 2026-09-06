package server

import (
	"context"
	"os"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// A meta.json write failure — a full disk — must not look like a clean
// success: it becomes a board-visible fault rather than being dropped.
func TestPersistenceFailureRaisesABoardVisibleFault(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.notePersistFault(&runner.Result{Run: model.Run{
		ID: "000000.000-x", ItemID: "s1:1", Source: "s1",
		Error: "persist meta.json: write /data/runs/.../meta.json: no space left on device",
	}, PersistErr: "persist meta.json: write /data/runs/.../meta.json: no space left on device"})
	if s.state.PersistFault == nil {
		t.Fatal("PersistFault is nil, want it set")
	}
	if s.state.PersistFault.RunID != "000000.000-x" {
		t.Errorf("RunID = %q, want 000000.000-x", s.state.PersistFault.RunID)
	}
}

// A run whose process failed to start occupies Run.Error with that failure
// first — the same run's log.txt append can still fail on the same full
// disk, and that must not be masked just because Run.Error was already
// taken. notePersistFault reads the dedicated PersistErr field for exactly
// this reason.
func TestPersistenceFailureIsNotMaskedByAnEarlierUnrelatedError(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.notePersistFault(&runner.Result{Run: model.Run{
		ID: "000000.000-z", ItemID: "s1:1", Source: "s1",
		Error: "fork/exec /path/to/script: permission denied",
	}, PersistErr: "append log.txt: write /data/runs/.../log.txt: no space left on device"})
	if s.state.PersistFault == nil {
		t.Fatal("PersistFault is nil, want it set even though Run.Error names an unrelated failure")
	}
	if s.state.PersistFault.RunID != "000000.000-z" {
		t.Errorf("RunID = %q, want 000000.000-z", s.state.PersistFault.RunID)
	}
}

// A log.txt append failure is a persistence fault exactly like a failed
// meta.json write — the run's own record still could not be trusted, even
// though meta.json itself may have written fine.
func TestLogAppendFailureRaisesABoardVisibleFaultToo(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.notePersistFault(&runner.Result{Run: model.Run{
		ID: "000000.000-y", ItemID: "s1:1", Source: "s1",
		Error: "append log.txt: write /data/runs/.../log.txt: no space left on device",
	}, PersistErr: "append log.txt: write /data/runs/.../log.txt: no space left on device"})
	if s.state.PersistFault == nil {
		t.Fatal("PersistFault is nil for a log.txt append failure, want it set")
	}
	if s.state.PersistFault.RunID != "000000.000-y" {
		t.Errorf("RunID = %q, want 000000.000-y", s.state.PersistFault.RunID)
	}
}

// The fault is sticky: it must survive a refresh, which rebuilds Warnings
// from scratch every poll and would otherwise make a disk-full condition
// disappear within one poll interval. Unlike Warnings, refresh must not be
// what clears it — only a run that actually persists successfully does, and
// with the run root itself unwritable here, nothing can.
func TestPersistenceFaultSurvivesARefresh(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.notePersistFault(&runner.Result{Run: model.Run{ID: "x", Error: "persist meta.json: enospc"}, PersistErr: "persist meta.json: enospc"})
	if s.state.PersistFault == nil {
		t.Fatal("setup: fault not recorded")
	}
	if err := os.MkdirAll(r.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.Root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.Root, 0o755) })

	s.refresh(context.Background())

	if s.state.PersistFault == nil {
		t.Error("fault was dropped by refresh")
	}
}

// The fault clears once a later run persists its own metadata successfully —
// the disk is not, or is no longer, the problem.
func TestPersistenceFaultClearsOnALaterSuccessfulPersist(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.notePersistFault(&runner.Result{Run: model.Run{ID: "x", Error: "persist meta.json: enospc"}, PersistErr: "persist meta.json: enospc"})
	s.notePersistFault(&runner.Result{Run: model.Run{ID: "y", Outcome: model.OutcomeSuccess}})
	if s.state.PersistFault != nil {
		t.Errorf("PersistFault = %+v, want cleared by a later clean persist", s.state.PersistFault)
	}
}

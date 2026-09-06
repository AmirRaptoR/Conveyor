package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func neverPinned(RunMeta) (bool, string) { return false, "" }

// A run directory whose day is strictly before the cutoff's UTC date is
// deleted; one on or after it is kept, per CONTRACTS §6's day-based
// comparison rather than a timestamp one.
func TestSweepDeletesBeforeCutoffKeepsOnOrAfter(t *testing.T) {
	root := t.TempDir()
	old := writeTestRun(t, root, "2026-08-01", "000000.000-old", model.Run{Outcome: model.OutcomeSuccess})
	onCutoff := writeTestRun(t, root, "2026-08-15", "000000.000-onc", model.Run{Outcome: model.OutcomeSuccess})
	recent := writeTestRun(t, root, "2026-08-20", "000000.000-new", model.Run{Outcome: model.OutcomeSuccess})

	cutoff := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	res, err := sweepRoot(root, cutoff, neverPinned)
	if err != nil {
		t.Fatalf("sweepRoot: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", res.Deleted)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old run still on disk: %v", err)
	}
	if _, err := os.Stat(onCutoff); err != nil {
		t.Errorf("cutoff-day run was deleted: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent run was deleted: %v", err)
	}
	// The day directory left with nothing in it is removed too.
	if _, err := os.Stat(filepath.Join(root, "2026-08-01")); !os.IsNotExist(err) {
		t.Errorf("empty day directory left behind: %v", err)
	}
}

// A day directory still holding a pinned run must not be removed even though
// every other run in it was swept.
func TestSweepKeepsDayDirectoryHoldingAPinnedRun(t *testing.T) {
	root := t.TempDir()
	writeTestRun(t, root, "2026-08-01", "000000.000-gone", model.Run{Outcome: model.OutcomeSuccess})
	pinnedDir := writeTestRun(t, root, "2026-08-01", "000001.000-kept", model.Run{Outcome: model.OutcomeSuccess})

	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	res, err := sweepRoot(root, cutoff, func(m RunMeta) (bool, string) {
		return m.ID == "000001.000-kept", "test pin"
	})
	if err != nil {
		t.Fatalf("sweepRoot: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", res.Deleted)
	}
	if len(res.Pinned) != 1 || !strings.Contains(res.Pinned[0], "000001.000-kept") {
		t.Errorf("Pinned = %v, want it to name 000001.000-kept", res.Pinned)
	}
	if _, err := os.Stat(pinnedDir); err != nil {
		t.Errorf("pinned run was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "2026-08-01")); err != nil {
		t.Errorf("day directory holding a pinned run was removed: %v", err)
	}
}

// A run whose own meta.json outcome is "running" is never deleted regardless
// of age, independent of any item-based pin: nothing this process started is
// alive to have finished writing that record honestly yet.
func TestSweepNeverDeletesARunningOutcome(t *testing.T) {
	root := t.TempDir()
	dir := writeTestRun(t, root, "2026-01-01", "000000.000-live", model.Run{Outcome: model.OutcomeRunning})

	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	res, err := sweepRoot(root, cutoff, neverPinned)
	if err != nil {
		t.Fatalf("sweepRoot: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("deleted = %d, want 0", res.Deleted)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("running run was deleted: %v", err)
	}
}

func TestSweepToleratesAMissingRoot(t *testing.T) {
	res, err := sweepRoot(filepath.Join(t.TempDir(), "never-created"), time.Now(), neverPinned)
	if err != nil || res.Deleted != 0 {
		t.Fatalf("sweep of a missing root = (%+v, %v), want (0 deleted, nil)", res, err)
	}
}

// The most recent blocked/failure/timeout stage run of a currently-marked
// item must survive retention even when it is older than the cutoff, and
// recallBlocks — which is what a restarted process uses to recover a mark's
// reason — must still find it afterwards.
func TestSweepPinsTheMarkingRunSoARestartStillRecoversTheReason(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	day := filepath.Join(r.Root, "2020-01-01", "120000.000-mark")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "meta.json"), []byte(
		`{"id":"120000.000-mark","source":"s1","itemId":"s1:1","kind":"stage",
		  "to":"working","outcome":"blocked","exitCode":20,
		  "startedAt":"2020-01-01T00:00:00Z","finishedAt":"2020-01-01T00:00:00Z"}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "result.json"),
		[]byte(`{"blocked":true,"reason":"needs a person"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	item := model.Item{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}
	s.recallBlocks([]model.Item{item})
	if s.blocks["s1:1"].RunID != "120000.000-mark" {
		t.Fatalf("setup: block not recovered from the run, got %+v", s.blocks["s1:1"])
	}

	s.runSweep(io.Discard)

	if _, err := os.Stat(day); err != nil {
		t.Fatalf("the marking run was deleted: %v", err)
	}
	// A restart loses the in-memory index; recallBlocks must recover the same
	// answer from the run the sweep just protected.
	restarted := New(cfg, r)
	restarted.recallBlocks([]model.Item{item})
	if got := restarted.blocks["s1:1"].Reason; got != "needs a person" {
		t.Errorf("reason after restart = %q, want %q", got, "needs a person")
	}
}

// The most recent successful move that landed a currently-listed item in its
// current stage must survive retention, so the stage-age chip is not dropped
// after a restart.
func TestSweepPinsTheArrivalMoveSoARestartStillRecoversTheStageAge(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	day := filepath.Join(r.Root, "2020-01-01", "120000.000-move")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "meta.json"), []byte(
		`{"id":"120000.000-move","source":"s1","itemId":"s1:1","kind":"move",
		  "from":"backlog","to":"working","outcome":"success",
		  "startedAt":"2020-01-01T00:00:00Z","finishedAt":"2020-01-01T00:00:00Z"}`),
		0o644); err != nil {
		t.Fatal(err)
	}

	item := model.Item{ID: "s1:1", Source: "s1", Stage: "working"}
	s.recallBlocks([]model.Item{item})
	if s.times["s1:1"].RunID != "120000.000-move" {
		t.Fatalf("setup: time not recovered from the run, got %+v", s.times["s1:1"])
	}

	s.runSweep(io.Discard)

	if _, err := os.Stat(day); err != nil {
		t.Fatalf("the arrival move was deleted: %v", err)
	}
	restarted := New(cfg, r)
	restarted.recallBlocks([]model.Item{item})
	if restarted.times["s1:1"].EnteredStage.IsZero() {
		t.Errorf("stage-age chip is empty after restart, want it recovered")
	}
}

// Pinning is evaluated against the board's current items only: an expired
// run belonging to an item that has fallen off the board — closed, or gone
// from the source entirely — is swept normally, because s.blocks/s.times are
// themselves pruned to on-board items on every refresh.
func TestSweepDoesNotPinRunsOfItemsNoLongerOnTheBoard(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	dir := writeTestRun(t, r.Root, "2020-01-01", "120000.000-stale", model.Run{
		Source: "s1", ItemID: "s1:gone", Kind: "stage", Outcome: model.OutcomeBlocked, ExitCode: 20,
	})
	// Recovering a block for an item that is (for this moment) still on the
	// board, then dropping it off the board the way refresh() would, leaves
	// s.blocks empty again — exactly the state a truly gone item is in.
	s.recallBlocks([]model.Item{{ID: "s1:gone", Source: "s1", Stage: "working", Blocked: true}})
	delete(s.blocks, "s1:gone")

	s.runSweep(io.Discard)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("run of an off-board item was pinned instead of swept normally: %v", err)
	}
}

// untilNextSweepAt paces the daily sweep off logs.sweepAt in the caller's own
// location, never in the past.
func TestUntilNextSweepAtPacesToTheNextOccurrence(t *testing.T) {
	now := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	if got := untilNextSweepAt("04:00", now); got != time.Hour {
		t.Errorf("earlier today: got %s, want 1h", got)
	}
	if got := untilNextSweepAt("02:00", now); got != 23*time.Hour {
		t.Errorf("already passed today: got %s, want 23h (tomorrow)", got)
	}
	if got := untilNextSweepAt("03:00", now); got != 24*time.Hour {
		t.Errorf("exactly now: got %s, want 24h (tomorrow, never zero/negative)", got)
	}
}

// /api/state carries how much the run store holds and how far back it
// reaches, computed by walking file sizes rather than decoding any run's
// meta.json.
func TestStorageUseCountsRunsAndBytesWithoutReadingMetaJSON(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	writeTestRun(t, r.Root, "2020-01-01", "000000.000-a", model.Run{Outcome: model.OutcomeSuccess})
	writeTestRun(t, r.Root, "2020-01-02", "000000.000-b", model.Run{Outcome: model.OutcomeSuccess})

	v := s.storageUse()
	if v.Runs != 2 {
		t.Errorf("Runs = %d, want 2", v.Runs)
	}
	if v.Bytes <= 0 {
		t.Errorf("Bytes = %d, want > 0", v.Bytes)
	}
	if v.OldestDay != "2020-01-01" {
		t.Errorf("OldestDay = %q, want 2020-01-01", v.OldestDay)
	}
}

// -watch observes only: it must not delete anything either.
func TestSweepDoesNotRunUnderWatch(t *testing.T) {
	cfg, r := boardFor(t)
	expired := writeTestRun(t, r.Root, "2020-01-01", "000000.000-old", model.Run{Outcome: model.OutcomeSuccess})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(cfg, r)
	go s.Run(ctx, "127.0.0.1:0", false) // auto=false: -watch
	time.Sleep(150 * time.Millisecond)

	if _, err := os.Stat(expired); err != nil {
		t.Errorf("an expired run was swept under -watch: %v", err)
	}
}

// Each sweep logs one summary line naming counts and the horizon, and each
// pinned run individually — so pinned evidence cannot pile up unnoticed.
func TestSweepLogsASummaryLineAndNamesEachPinnedRun(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	writeTestRun(t, r.Root, "2020-01-01", "000000.000-gone", model.Run{Outcome: model.OutcomeSuccess})
	writeTestRun(t, r.Root, "2020-01-01", "000001.000-live", model.Run{Outcome: model.OutcomeRunning})

	var buf bytes.Buffer
	s.runSweep(&buf)
	out := buf.String()
	if !strings.Contains(out, "deleted 1") {
		t.Errorf("summary line missing the delete count: %s", out)
	}
	if !strings.Contains(out, "1 pinned") {
		t.Errorf("summary line missing the pinned count: %s", out)
	}
	if !strings.Contains(out, "000001.000-live") {
		t.Errorf("pinned run not named individually: %s", out)
	}
}

package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func neverPinned(RunMeta) (bool, string) { return false, "" }

func allowRunRetention(s *Server) {
	s.retentionReadyOnce.Do(func() { close(s.retentionReady) })
}

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
	allowRunRetention(s)
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
	allowRunRetention(s)
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
	allowRunRetention(s)
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

// untilNextSweepAt paces the daily sweep off storage.sweepAt in the caller's own
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

// /api/state carries totals, retention-class bytes and projected daily growth.
func TestStorageUseCountsRunsBytesClassesAndGrowth(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	writeTestRun(t, r.Root, "2020-01-01", "000000.000-a", model.Run{Outcome: model.OutcomeSuccess})
	writeTestRun(t, r.Root, time.Now().UTC().Format("2006-01-02"), "000000.000-b", model.Run{Kind: "status", StartedAt: time.Now(), Outcome: model.OutcomeSuccess})

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
	if v.ByClass["polling"] == 0 || v.ByClass["status"] == 0 {
		t.Errorf("ByClass = %v, want polling and status bytes", v.ByClass)
	}
	if v.ProjectedGrowth == 0 {
		t.Error("ProjectedGrowth = 0, want today's run bytes")
	}
	if v.MaxBytes == 0 || v.TempMaxBytes == 0 {
		t.Errorf("ceilings missing: %+v", v)
	}
}

// -watch observes only: it must not delete anything either.
func TestSweepDoesNotRunUnderWatch(t *testing.T) {
	cfg, r := boardFor(t)
	expired := writeTestRun(t, r.Root, "2020-01-01", "000000.000-old", model.Run{Outcome: model.OutcomeSuccess})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(cfg, r)
	go s.Run(ctx, "127.0.0.1:0", ModeObserve) // -watch
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
	allowRunRetention(s)
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

func TestStorageAdmissionPausesOnlyModelWorkAtHighWatermark(t *testing.T) {
	cfg, r := boardFor(t)
	cfg.Sources[0].Scripts["work"] = config.ScriptSpec{Agent: "fake"}
	cfg.Storage.MaxBytes = 100
	cfg.Storage.HighWatermark = 80
	cfg.Storage.CriticalWatermark = 95
	s := New(cfg, r)
	if err := os.WriteFile(filepath.Join(cfg.DataDir(), "used"), make([]byte, 85), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.reserveModelStorage("s1:direct") {
		t.Fatal("model work allowed above the high watermark")
	}
	if got := s.claim(model.Item{ID: "s1:1", Source: "s1", Stage: "backlog"}, "working", false); got != claimStorageHigh {
		t.Fatalf("model claim = %v, want claimStorageHigh", got)
	}
}

func TestRunClassUsesFailureBeforeModelAndSeparatesPollingStatus(t *testing.T) {
	cfg, _ := boardFor(t)
	cfg.Sources[0].Scripts["work"] = config.ScriptSpec{Agent: "fake"}
	tests := []struct {
		run  model.Run
		want string
	}{
		{model.Run{Kind: "stage", Source: "s1", To: "working", Outcome: model.OutcomeFailure}, "failure"},
		{model.Run{Kind: "stage", Source: "s1", To: "working", Outcome: model.OutcomeSuccess}, "model"},
		{model.Run{Kind: "list", Outcome: model.OutcomeSuccess}, "polling"},
		{model.Run{Kind: "status", Outcome: model.OutcomeSuccess}, "status"},
	}
	for _, tt := range tests {
		if got := runClass(cfg, tt.run); got != tt.want {
			t.Errorf("class = %q, want %q", got, tt.want)
		}
	}
}

func TestRetentionClassesExpireIndependently(t *testing.T) {
	cfg, r := boardFor(t)
	cfg.Storage.Retention.Polling = config.Duration(2 * 24 * time.Hour)
	cfg.Storage.Retention.Status = config.Duration(7 * 24 * time.Hour)
	cfg.Storage.Retention.Model = config.Duration(30 * 24 * time.Hour)
	cfg.Storage.Retention.Failure = config.Duration(90 * 24 * time.Hour)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	poll := writeTestRun(t, r.Root, "2026-09-21", "000000.000-poll", model.Run{Kind: "list", StartedAt: now.Add(-5 * 24 * time.Hour), Outcome: model.OutcomeSuccess})
	status := writeTestRun(t, r.Root, "2026-09-21", "000001.000-status", model.Run{Kind: "status", StartedAt: now.Add(-5 * 24 * time.Hour), Outcome: model.OutcomeSuccess})
	modelRun := writeTestRun(t, r.Root, "2026-08-17", "000002.000-model", model.Run{Kind: "stage", RetentionClass: "model", StartedAt: now.Add(-40 * 24 * time.Hour), Outcome: model.OutcomeSuccess})
	failure := writeTestRun(t, r.Root, "2026-08-17", "000003.000-fail", model.Run{Kind: "stage", RetentionClass: "model", StartedAt: now.Add(-40 * 24 * time.Hour), Outcome: model.OutcomeFailure})
	if _, err := sweepClasses(r.Root, cfg, now, neverPinned); err != nil {
		t.Fatal(err)
	}
	for path, exists := range map[string]bool{poll: false, status: true, modelRun: false, failure: true} {
		_, err := os.Stat(path)
		if got := err == nil; got != exists {
			t.Errorf("%s exists = %v, want %v (err %v)", filepath.Base(path), got, exists, err)
		}
	}
}

func TestPartialListingNeverReplacesLastGoodSourceState(t *testing.T) {
	cfg, r := boardFor(t)
	list := cfg.Sources[0].List
	writeScript(t, list, `#!/bin/sh
printf '%s' '[{"id":"s1:1","ref":"1","stage":"backlog","title":"last good"}]' >"$CONVEYOR_RESULT"
`)
	s := New(cfg, r)
	s.refresh(t.Context())
	writeScript(t, list, `#!/bin/sh
printf '%s' '[{"id":"s1:1","ref":"1","stage":"backlog","title":"partial replacement"},{"id":"s1:2","stage":"backlog","title":"missing ref"}]' >"$CONVEYOR_RESULT"
`)
	s.refresh(t.Context())
	if len(s.state.Items) != 1 || s.state.Items[0].Title != "last good" {
		t.Fatalf("items after partial listing = %+v, want untouched last-good state", s.state.Items)
	}
	if s.state.Sources[0].ListError == "" {
		t.Fatal("partial listing was not reported as a failed listing")
	}
}

func TestEmptyOrTruncatedListingNeverReplacesLastGoodSourceState(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "empty", body: `: >"$CONVEYOR_RESULT"`},
		{name: "truncated", body: `printf '%s' '[{"id":"s1:1"' >"$CONVEYOR_RESULT"`},
		{name: "null", body: `printf '%s' 'null' >"$CONVEYOR_RESULT"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, r := boardFor(t)
			writeScript(t, cfg.Sources[0].List, `#!/bin/sh
printf '%s' '[{"id":"s1:1","ref":"1","stage":"backlog","title":"last good"}]' >"$CONVEYOR_RESULT"
`)
			s := New(cfg, r)
			s.refresh(t.Context())
			writeScript(t, cfg.Sources[0].List, "#!/bin/sh\n"+tc.body+"\n")
			s.refresh(t.Context())
			if len(s.state.Items) != 1 || s.state.Items[0].Title != "last good" {
				t.Fatalf("items after invalid listing = %+v, want untouched last-good state", s.state.Items)
			}
			if s.state.Sources[0].ListError == "" {
				t.Fatal("invalid listing was not reported as failed")
			}
		})
	}
}

func TestStartupSweepWaitsForInitialListingEvidence(t *testing.T) {
	cfg, r := boardFor(t)
	release := filepath.Join(t.TempDir(), "release")
	writeScript(t, cfg.Sources[0].List, `#!/bin/sh
while [ ! -e "`+release+`" ]; do sleep 0.01; done
printf '%s' '[{"id":"s1:1","ref":"1","stage":"working","title":"blocked","blocked":true}]' >"$CONVEYOR_RESULT"
`)
	dir := writeTestRun(t, r.Root, "2020-01-01", "120000.000-mark", model.Run{
		Source: "s1", ItemID: "s1:1", Kind: "stage", To: "working",
		StartedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Outcome: model.OutcomeBlocked,
	})
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"blocked":true,"reason":"keep me"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, r)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.sweep(ctx)
	go s.refresh(ctx)
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("startup sweep ran before initial listing: %v", err)
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "startup retention sweep", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return !s.lastCleanup.IsZero()
	})
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("marking evidence was deleted after startup recall pinned it: %v", err)
	}
}

func TestModelStorageReservationsAreAtomicAndIncludeProjectedGrowth(t *testing.T) {
	cfg, r := boardFor(t)
	cfg.Sources[0].Scripts["work"] = config.ScriptSpec{Agent: "fake"}
	cfg.Concurrency.Global, cfg.Concurrency.PerSource, cfg.Concurrency.PerStage = 2, 2, 2
	s := New(cfg, r)
	v := s.storageUse()
	if v.ProjectedGrowth == 0 {
		t.Fatal("test setup has no projected growth")
	}
	highBytes := v.Bytes + v.ProjectedGrowth + v.ProjectedGrowth/2
	cfg.Storage.MaxBytes = config.ByteSize(highBytes * 100 / int64(cfg.Storage.HighWatermark))
	cfg.Storage.TempMaxBytes = config.ByteSize(1 << 30)

	items := []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "backlog"},
		{ID: "s1:2", Source: "s1", Stage: "backlog"},
	}
	results := make(chan claimRefusal, len(items))
	var wg sync.WaitGroup
	for _, item := range items {
		wg.Add(1)
		go func(item model.Item) {
			defer wg.Done()
			results <- s.claim(item, "working", false)
		}(item)
	}
	wg.Wait()
	close(results)
	accepted, refused := 0, 0
	for got := range results {
		switch got {
		case claimAccepted:
			accepted++
		case claimStorageHigh:
			refused++
		default:
			t.Fatalf("claim refusal = %v, want accepted or storage high", got)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("accepted=%d storage-refused=%d, want one each", accepted, refused)
	}
	for _, item := range items {
		if _, ok := s.working.Load(item.ID); ok {
			s.releaseModelStorage(item.ID)
			s.working.Delete(item.ID)
			s.eng.Locks().Release(item.Source, "working", s.cfg.ResourcesFor(item.Source, "working")...)
		}
	}
}

func TestProjectedGrowthIncludesStorePayloadAndScratch(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	files := map[string]int{
		filepath.Join(cfg.DataDir(), "ledger.json"):                        11,
		filepath.Join(filepath.Dir(r.Root), "payloads", "payload.json.gz"): 13,
		filepath.Join(r.TempRoot, ".run-test", "artifact"):                 17,
	}
	var want int64
	for path, size := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		want += int64(size)
	}
	v := s.storageUse()
	if v.ProjectedGrowth < want {
		t.Fatalf("ProjectedGrowth = %d, want at least store+payload+scratch bytes %d", v.ProjectedGrowth, want)
	}
	if v.ByClass["payloads"] < 13 || v.ByClass["scratch"] < 17 || v.ByClass["store"] < 11 {
		t.Fatalf("ByClass = %v, want payload, scratch and store accounted", v.ByClass)
	}
}

func TestCriticalPressureBeforeRetentionReadyCleansIndependentStorageButRetainsRuns(t *testing.T) {
	cfg, r := boardFor(t)
	cfg.Sources[0].Scripts["work"] = config.ScriptSpec{Agent: "fake"}
	cfg.Storage.MaxBytes = 100
	cfg.Storage.TempMaxBytes = 100
	cfg.Storage.HighWatermark = 80
	cfg.Storage.CriticalWatermark = 90
	cfg.Storage.Retention.Polling = config.Duration(time.Hour)
	s := New(cfg, r)
	runDir := writeTestRun(t, r.Root, "2020-01-01", "000000.000-old", model.Run{Kind: "list", StartedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Outcome: model.OutcomeSuccess})
	scratch := filepath.Join(r.TempRoot, "120000.000-old")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(scratch, ".conveyor-owned")
	if err := os.WriteFile(marker, []byte(`{"v":1,"runId":"120000.000-old","ended":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	payloadRoot := filepath.Join(filepath.Dir(r.Root), "payloads")
	if err := os.MkdirAll(payloadRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(payloadRoot, strings.Repeat("a", 64)+".json.gz")
	if err := os.WriteFile(payload, []byte("unused"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(payload, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir(), "pressure"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.reserveModelStorage("s1:1") {
		t.Fatal("model work admitted before retention was safe under critical pressure")
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("run swept before initial evidence reconstruction: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("owned expired scratch remains: %v", err)
	}
	if _, err := os.Stat(payload); !os.IsNotExist(err) {
		t.Fatalf("unreferenced expired payload remains: %v", err)
	}
}

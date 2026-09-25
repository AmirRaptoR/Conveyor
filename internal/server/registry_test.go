package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The live-run registry (#111) is opened on Runner.OnStart and closed on
// Runner.OnResult: an entry exists exactly while the stage process itself
// is running, unlike active (schedule.go), which is set before
// Engine.Advance and survives the post-stage provider move.
func TestLiveRunsRegistryOpensWhileStageRunsAndClosesAfter(t *testing.T) {
	cfg, r, dir := pipelineFor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(cfg, r)
	s.ctx = ctx
	go s.poll(ctx)
	go s.schedule(ctx)

	waitFor(t, "the stage to start", func() bool {
		_, err := os.Stat(filepath.Join(dir, "started"))
		return err == nil
	})

	waitFor(t, "the live-run registry to see the stage running", func() bool {
		_, ok := s.liveRuns.Lookup("s1:1")
		return ok
	})
	entry, ok := s.liveRuns.Lookup("s1:1")
	if !ok {
		t.Fatal("expected a live-run entry for s1:1")
	}
	if entry.Stage != "working" {
		t.Errorf("entry.Stage = %q, want %q", entry.Stage, "working")
	}
	if entry.RunID == "" {
		t.Error("entry.RunID is empty")
	}
	if entry.Dir == "" || filepath.Base(filepath.Dir(entry.Dir)) == "" {
		t.Errorf("entry.Dir looks wrong: %q", entry.Dir)
	}

	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the live-run entry to close once the stage process exits", func() bool {
		_, ok := s.liveRuns.Lookup("s1:1")
		return !ok
	})
	// Registry closure deliberately precedes the provider move that finishes
	// the transition. Wait for that separate lifetime before TempDir cleanup.
	waitFor(t, "the transition to finish", func() bool {
		_, working := s.working.Load("s1:1")
		return !working
	})
}

// A list, move, doctor or status run is never steerable and must never
// reach the registry — only a "stage" run with a real item id and target
// stage does.
func TestLiveRunsRegistryIgnoresNonStageRuns(t *testing.T) {
	cfg, r, _ := pipelineFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()

	// A listing run is dispatched by refresh(); driving one directly and
	// waiting for it to settle is enough to prove it never opens an entry.
	s.refresh(s.ctx)

	if _, ok := s.liveRuns.Lookup("s1:1"); ok {
		t.Error("a list run must never appear in the live-run registry")
	}
}

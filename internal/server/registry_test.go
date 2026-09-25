package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// The live-run registry (#111) is opened on Runner.OnStart and closed on
// Runner.OnProcessExit: an entry exists exactly while the stage process itself
// is running, unlike active (schedule.go), which is set before Engine.Advance
// and survives the post-stage provider move.
func TestLiveRunsRegistryClosesBeforeProviderMove(t *testing.T) {
	cfg, r, dir := pipelineFor(t)
	moves := filepath.Join(dir, "moves")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\necho x >> "+moves+"\nexit 0\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(cfg, r)
	processExited := make(chan struct{})
	releaseExitCallback := make(chan struct{})
	previous := r.OnProcessExit
	r.OnProcessExit = func(run model.Run) {
		if previous != nil {
			previous(run)
		}
		if run.Kind == "stage" {
			close(processExited)
			<-releaseExitCallback
		}
	}
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
	select {
	case <-processExited:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for process-exit callback")
	}
	if _, ok := s.liveRuns.Lookup("s1:1"); ok {
		t.Fatal("live-run registry remained open in the process-exit callback")
	}
	if got := countLines(moves); got != 1 {
		t.Fatalf("provider moves before process-exit callback returned = %d, want only the initial move", got)
	}
	close(releaseExitCallback)
	waitFor(t, "the transition to finish", func() bool {
		_, working := s.working.Load("s1:1")
		return !working
	})
	if got := countLines(moves); got != 2 {
		t.Fatalf("provider moves after transition = %d, want initial and outgoing moves", got)
	}
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

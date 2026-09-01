package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// deferringPipeline lays down one item already sitting in a stage whose script
// always exits 10 — the shape of `approving` waiting on a pull request that has
// not settled. Every run of it appends a line, and so does every listing, so a
// test can compare the two.
func deferringPipeline(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	lists := filepath.Join(dir, "lists")
	runs := filepath.Join(dir, "stageruns")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
echo x >> `+lists+`
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"working","title":"waiting on the outside world"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\necho x >> "+runs+"\nexit 10\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 250ms
stages:
  - name: working
    script: work
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo
    scripts:
      work:
        script: ./work.sh
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), dir
}

// Exit 10 says "leave it, try again next poll" (CONTRACTS §2), and the engine
// answered "try again now": a finished transition wakes the scheduler, the item
// is still workable in the stage it never left, and nothing else can outrank a
// stage that deep — so it was picked again straight away. In production one
// `approving` stage waiting on a quiet pull request re-ran itself every couple
// of seconds, fourteen hundred runs an hour, each a real API call.
//
// The invariant that ends it: a deferred stage runs at most once per listing.
func TestANoOpWaitsForTheNextListing(t *testing.T) {
	cfg, r, dir := deferringPipeline(t)
	lists, stageRuns := filepath.Join(dir, "lists"), filepath.Join(dir, "stageruns")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := New(cfg, r)
	s.ctx = ctx
	go s.poll(ctx)
	go s.schedule(ctx)

	waitFor(t, "the deferring stage to run once", func() bool { return countLines(stageRuns) >= 1 })
	// Long enough for a tight loop to file hundreds of runs, and for several
	// listings to land.
	time.Sleep(1500 * time.Millisecond)

	got, listings := countLines(stageRuns), countLines(lists)
	if got > listings {
		t.Errorf("stage ran %d times against %d listings; a no-op must wait for the next one", got, listings)
	}
	// It must still be retried — deferring is not giving up.
	if got < 2 {
		t.Errorf("stage ran %d time(s) across %d listings; it should retry once per listing", got, listings)
	}
}

// The tick button means "look again now". A deferral is exactly the kind of
// answer it must not receive.
func TestTheTickButtonClearsADeferral(t *testing.T) {
	cfg, r, dir := deferringPipeline(t)
	stageRuns := filepath.Join(dir, "stageruns")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := New(cfg, r)
	s.ctx = ctx
	s.refresh(ctx) // one listing, no poll ticker: nothing else will clear it
	go s.schedule(ctx)
	go s.button(ctx, true)

	waitFor(t, "the deferring stage to run once", func() bool { return countLines(stageRuns) >= 1 })
	waitFor(t, "the item to be resting", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.resting["s1:1"]
	})
	before := countLines(stageRuns)

	s.tick <- struct{}{}
	waitFor(t, "the button to run it again", func() bool { return countLines(stageRuns) > before })
}

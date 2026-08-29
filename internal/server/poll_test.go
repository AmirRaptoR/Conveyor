package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// write creates an executable script, making parents as needed.
func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// pipelineFor lays down a one-source pipeline whose single stage blocks until
// the test releases it, so a stage can be held mid-flight on purpose.
func pipelineFor(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	lists := filepath.Join(dir, "lists") // one line per list script run
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
echo x >> `+lists+`
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"backlog","title":"the only item"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), `#!/bin/sh
touch `+started+`
while [ ! -f `+release+` ]; do sleep 0.02; done
`)
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 100ms
stages:
  - name: backlog
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

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func countLines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(b)))
}

// The bug this exists for: draining used to run on the poll's goroutine and
// returned only once the pipeline was at rest, so a long stage stopped every
// source being listed — work in other repositories was not deprioritised, it
// was invisible.
func TestPollKeepsListingWhileAStageRuns(t *testing.T) {
	cfg, r, dir := pipelineFor(t)
	lists := filepath.Join(dir, "lists")

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

	// Held mid-stage. Discovery must carry on regardless.
	before := countLines(lists)
	waitFor(t, "another listing while the stage is held", func() bool {
		return countLines(lists) > before+1
	})

	// And the item is genuinely still in flight, not finished early.
	if s.inFlight.Load() == 0 {
		t.Fatal("the stage was released before the assertion; the test proved nothing")
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stage to finish", func() bool { return s.inFlight.Load() == 0 })
}

// A finished transition wakes the scheduler, so the next item starts at once
// rather than waiting out the poll interval.
func TestFinishedTransitionWakesTheScheduler(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the transition to complete", func() bool { return s.inFlight.Load() == 0 })
}

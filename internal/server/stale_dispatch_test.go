package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// staleDispatchPipeline is a one-source, one-item pipeline whose only
// transition writes a marker file if it actually ran — so a test can assert
// the scheduler refused to dispatch simply by the marker's absence.
func staleDispatchPipeline(t *testing.T) (cfg *config.Config, r *runner.Runner, ran string) {
	t.Helper()
	dir := t.TempDir()
	ran = filepath.Join(dir, "ran")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"backlog","title":"the only item"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\ntouch "+ran+"\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
stages:
  - name: backlog
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
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return c, runner.New(filepath.Join(dir, "runs")), ran
}

func TestLaunchSkipsAFreshItemWhoseDependencySourceIsStale(t *testing.T) {
	cfg, r, ran := staleDispatchPipeline(t)
	// The dependency is read-only in this test. Cloning the resolved source
	// gives the engine a valid second client without adding another fixture.
	s2 := cfg.Sources[0]
	s2.Name = "s2"
	cfg.Sources = append(cfg.Sources, s2)
	s := New(cfg, r)
	s.state.Items = []model.Item{
		{ID: "s2:1", Ref: "1", Source: "s2", Stage: "done"},
		{ID: "s1:2", Ref: "2", Source: "s1", Stage: "backlog", DependsOn: []string{"s2:1"}},
	}
	s.listErr["s2"] = "boom"

	if n := s.launch(context.Background()); n != 0 {
		t.Fatalf("launch() = %d, want 0 while dependency source is stale", n)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("fresh follower ran using its dependency source's last-good state")
	}

	delete(s.listErr, "s2")
	if n := s.launch(context.Background()); n != 1 {
		t.Fatalf("launch() after dependency recovery = %d, want 1", n)
	}
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })
}

// Engine: the scheduler refuses to dispatch work for a source whose items are
// stale — reading the last listing's own outcome is for the board, never a
// basis to run a script against content that could not be confirmed this
// poll (CLAUDE.md: "stale state is for reading, never for acting on").
func TestLaunchSkipsAStaleSource(t *testing.T) {
	cfg, r, ran := staleDispatchPipeline(t)
	s := New(cfg, r)
	s.refresh(context.Background())

	if !hasItemID(s.state.Items, "s1:1") {
		t.Fatal("setup: s1's item did not appear after the first refresh")
	}

	// Mark s1 stale directly, as a failed or timed-out listing would, without
	// needing to actually break its list script for this test.
	s.mu.Lock()
	s.listErr["s1"] = "boom"
	s.mu.Unlock()

	n := s.launch(context.Background())
	if n != 0 {
		t.Errorf("launch() = %d, want 0 against a stale source", n)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the stage script ran against a stale source's item")
	}

	// s1 recovers: the same item is now dispatchable again.
	s.mu.Lock()
	delete(s.listErr, "s1")
	s.mu.Unlock()

	n = s.launch(context.Background())
	if n != 1 {
		t.Fatalf("launch() after recovery = %d, want 1", n)
	}
	waitFor(t, "the transition to finish", func() bool { return s.inFlight.Load() == 0 })
}

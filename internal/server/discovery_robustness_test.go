package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// concurrentSourcesPipeline lays down two sources whose list scripts each
// touch their own "started" marker and then block on a shared release file —
// so a test can prove both were launched before either finished, which a
// sequential loop could never show regardless of how fast each script runs.
func concurrentSourcesPipeline(t *testing.T) (cfg *config.Config, r *runner.Runner, started1, started2, release string) {
	t.Helper()
	dir := t.TempDir()
	started1 = filepath.Join(dir, "started-s1")
	started2 = filepath.Join(dir, "started-s2")
	release = filepath.Join(dir, "release")

	script := func(started string) string {
		return `#!/bin/sh
touch "` + started + `"
while [ ! -e "` + release + `" ]; do sleep 0.02; done
cat > "$CONVEYOR_RESULT" <<'JSON'
[]
JSON
`
	}
	writeScript(t, filepath.Join(dir, "providers", "p1", "list.sh"), script(started1))
	writeScript(t, filepath.Join(dir, "providers", "p1", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "p2", "list.sh"), script(started2))
	writeScript(t, filepath.Join(dir, "providers", "p2", "move.sh"), "#!/bin/sh\nexit 0\n")
	for _, repo := range []string{"repo1", "repo2"} {
		if err := os.MkdirAll(filepath.Join(dir, repo), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
discovery: 5s
stages:
  - name: backlog
  - name: done
    terminal: true
sources:
  - name: s1
    provider: p1
    workdir: ./repo1
  - name: s2
    provider: p2
    workdir: ./repo2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), started1, started2, release
}

// Engine: refresh lists every source concurrently rather than one after
// another. If it were sequential, s2's list script could not have started
// before s1's own blocking list script released it — but both scripts here
// block on the same release file, so the test can only pass if both were
// already running before either could finish.
func TestRefreshListsSourcesConcurrently(t *testing.T) {
	cfg, r, started1, started2, release := concurrentSourcesPipeline(t)
	s := New(cfg, r)

	done := make(chan struct{})
	go func() {
		s.refresh(context.Background())
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, e1 := os.Stat(started1)
		_, e2 := os.Stat(started2)
		if e1 == nil && e2 == nil {
			break // both started concurrently, neither waited for the other
		}
		if time.Now().After(deadline) {
			t.Fatalf("both sources did not start concurrently within 5s (s1 started=%v, s2 started=%v)", e1 == nil, e2 == nil)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := os.WriteFile(release, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not return after release")
	}
}

// hangOrListPipeline lays down two sources: s2 always lists one item
// successfully; s1 lists one item unless a "hang" flag file exists, in which
// case its list script sleeps well past the configured discovery timeout and
// is killed. discovery is set far shorter than the sleep, so a refresh call
// with the hang flag present must still return promptly.
func hangOrListPipeline(t *testing.T) (cfg *config.Config, r *runner.Runner, hangFlag string) {
	t.Helper()
	dir := t.TempDir()
	hangFlag = filepath.Join(dir, "hang")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
if [ -e "`+hangFlag+`" ]; then
  sleep 5
fi
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"backlog","title":"s1 item"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "ok", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s2:1","ref":"1","source":"s2","stage":"backlog","title":"s2 item"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "ok", "move.sh"), "#!/bin/sh\nexit 0\n")
	for _, repo := range []string{"repo1", "repo2"} {
		if err := os.MkdirAll(filepath.Join(dir, repo), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
discovery: 200ms
stages:
  - name: backlog
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo1
  - name: s2
    provider: ok
    workdir: ./repo2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), hangFlag
}

// Engine: a source that hangs past its discovery timeout keeps its last-good
// items on the board, flagged stale, with the age of that last-good listing —
// and a healthy source's own items are unaffected. The hung source's timeout
// is independent of the much larger stage/default timeout, so refresh itself
// returns promptly instead of blocking for the full sleep.
func TestHungSourceRetainsLastGoodItemsFlaggedStale(t *testing.T) {
	cfg, r, hangFlag := hangOrListPipeline(t)
	s := New(cfg, r)

	// First refresh: both sources healthy.
	s.refresh(context.Background())
	s1 := sourceView(t, s, "s1")
	if s1.Stale {
		t.Fatal("s1 after a healthy refresh: want not stale")
	}
	if s1.LastListedAt == "" {
		t.Fatal("s1 after a healthy refresh: want lastListedAt set")
	}
	firstGood := s1.LastListedAt
	if !hasItemID(s.state.Items, "s1:1") {
		t.Fatal("s1's item missing after the first, healthy refresh")
	}

	// s1 now hangs past its 200ms discovery timeout.
	if err := os.WriteFile(hangFlag, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.refresh(context.Background())
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("refresh took %v with a 200ms discovery timeout on a 5s sleep; the timeout is not bounding the hang", elapsed)
	}

	s1 = sourceView(t, s, "s1")
	if !s1.Stale {
		t.Error("s1 after hanging past its discovery timeout: want stale")
	}
	if s1.LastListedAt != firstGood {
		t.Errorf("s1's lastListedAt changed to %q after a failed attempt; want it to still name the last good listing, %q", s1.LastListedAt, firstGood)
	}
	if !hasItemID(s.state.Items, "s1:1") {
		t.Error("s1's last-good item vanished instead of being retained while stale")
	}
	if !hasItemID(s.state.Items, "s2:1") {
		t.Error("s2's own item is missing; a hung sibling source must not affect it")
	}

	// s1 recovers. LastListedAt is RFC3339 (second granularity, deliberately —
	// see SourceView's own comment), so the test must cross a second boundary
	// for a fresh value to be distinguishable from firstGood at all.
	if err := os.Remove(hangFlag); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	s.refresh(context.Background())
	s1 = sourceView(t, s, "s1")
	if s1.Stale {
		t.Error("s1 after recovering: want stale cleared")
	}
	if s1.LastListedAt == firstGood {
		t.Error("s1 after recovering: want a fresh lastListedAt")
	}
	if !hasItemID(s.state.Items, "s1:1") {
		t.Error("s1's item missing after recovering")
	}
}

func hasItemID(items []model.Item, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

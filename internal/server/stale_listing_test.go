package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// racyBoard is a one-source, two-item pipeline whose list script can be held
// at a barrier: it reports both items at fixed stages once released, or —
// when told to — omits the racy one entirely, to test that a listing which
// began before a transition completed cannot publish that transition's
// pre-image (F03).
func racyBoard(t *testing.T) (cfg *config.Config, r *runner.Runner, dir string, block, started, omitX string) {
	t.Helper()
	dir = t.TempDir()
	block = filepath.Join(dir, "block")
	started = filepath.Join(dir, "started")
	omitX = filepath.Join(dir, "omit-x")
	workRuns := filepath.Join(dir, "workruns")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
echo x >> `+started+`
while [ -e `+block+` ]; do sleep 0.02; done
if [ -e `+omitX+` ]; then
  cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:y","ref":"y","source":"s1","stage":"backlog","title":"unaffected"}]
JSON
else
  cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:x","ref":"x","source":"s1","stage":"implementing","title":"race me"},
 {"id":"s1:y","ref":"y","source":"s1","stage":"backlog","title":"unaffected"}]
JSON
fi
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\necho \"$CONVEYOR_ITEM_ID\" >> "+workRuns+"\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
stages:
  - name: backlog
  - name: implementing
    script: work
    onSuccess: reviewing
  - name: reviewing
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
	return c, runner.New(filepath.Join(dir, "runs")), dir, block, started, omitX
}

// F03 core: capture the board with item X in implementing, complete a
// transition into reviewing while a listing that began earlier is still
// running, then let that listing land reporting the old stage. The listing
// must not overwrite the transition, and the item's sibling (Y, no concurrent
// transition) must still publish exactly what the same listing reported —
// the guard is per item, not per pass.
func TestOlderListingCannotOverwriteANewerTransition(t *testing.T) {
	cfg, r, dir, block, started, _ := racyBoard(t)
	s := New(cfg, r)
	s.ctx = context.Background()

	s.refresh(s.ctx)
	s.mu.RLock()
	stageX := stageOf(s.state.Items, "s1:x")
	s.mu.RUnlock()
	if stageX != "implementing" {
		t.Fatalf("first listing: X at %q, want implementing", stageX)
	}

	if err := os.WriteFile(block, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(started); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.refresh(s.ctx)
	}()
	waitFor(t, "the second listing to begin", func() bool {
		return countLines(started) > 0
	})

	// The transition completes while the listing above is parked at the
	// barrier, having already begun (and stamped its per-source generation)
	// before this.
	item := model.Item{ID: "s1:x", Ref: "x", Source: "s1", Stage: "implementing", Title: "race me"}
	s.runOne(s.ctx, item, "reviewing")
	s.mu.RLock()
	afterTransition := stageOf(s.state.Items, "s1:x")
	s.mu.RUnlock()
	if afterTransition != "reviewing" {
		t.Fatalf("transition: X at %q, want reviewing", afterTransition)
	}

	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	<-done

	s.mu.RLock()
	finalX := stageOf(s.state.Items, "s1:x")
	finalY := stageOf(s.state.Items, "s1:y")
	s.mu.RUnlock()
	if finalX != "reviewing" {
		t.Errorf("after the stale listing landed, X = %q, want reviewing (the listing must not overwrite a newer transition)", finalX)
	}
	if finalY != "backlog" {
		t.Errorf("Y = %q, want backlog — an item with no concurrent transition must still publish normally in the same pass", finalY)
	}

	// And the scheduler must not relaunch "implementing" for X a second
	// time on the strength of the stale listing. Y legitimately advances out
	// of backlog into implementing on this same pass, so only X's own count
	// is telling.
	xRuns := countRunsFor(t, filepath.Join(dir, "workruns"), "s1:x")
	s.launch(s.ctx)
	waitFor(t, "launch to settle", func() bool { return s.inFlight.Load() == 0 })
	if got := countRunsFor(t, filepath.Join(dir, "workruns"), "s1:x"); got != xRuns {
		t.Errorf("work.sh ran for s1:x %d more time(s) after the stale listing; the stage must not be re-run", got-xRuns)
	}
}

func countRunsFor(t *testing.T, path, id string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if line == id {
			n++
		}
	}
	return n
}

func stageOf(items []model.Item, id string) string {
	for _, it := range items {
		if it.ID == id {
			return it.Stage
		}
	}
	return ""
}

// The rule is by when a listing began, never by what it reports: a listing
// that begins *after* a transition completed is applied even though its
// content disagrees with the transition — the separate human-edit conflict.
func TestListingBeginningAfterATransitionAppliesEvenIfItDisagrees(t *testing.T) {
	dir := t.TempDir()
	stagePath := filepath.Join(dir, "stage") // what the "provider" currently says
	if err := os.WriteFile(stagePath, []byte("implementing"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
stage=$(cat `+stagePath+`)
cat > "$CONVEYOR_RESULT" <<JSON
[{"id":"s1:x","ref":"x","source":"s1","stage":"$stage","title":"race me"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
stages:
  - name: backlog
  - name: implementing
    script: work
    onSuccess: reviewing
  - name: reviewing
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
	s := New(cfg, runner.New(filepath.Join(dir, "runs")))
	s.ctx = context.Background()

	s.refresh(s.ctx)

	// The transition completes, moving the cache to reviewing.
	item := model.Item{ID: "s1:x", Ref: "x", Source: "s1", Stage: "implementing", Title: "race me"}
	s.runOne(s.ctx, item, "reviewing")
	s.mu.RLock()
	afterTransition := stageOf(s.state.Items, "s1:x")
	s.mu.RUnlock()
	if afterTransition != "reviewing" {
		t.Fatalf("transition: X at %q, want reviewing", afterTransition)
	}

	// A person then moves the label back by hand — the next listing, which
	// begins after the transition completed, must be trusted even though it
	// disagrees with the transition's own outcome.
	if err := os.WriteFile(stagePath, []byte("backlog"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.refresh(s.ctx)

	s.mu.RLock()
	final := stageOf(s.state.Items, "s1:x")
	s.mu.RUnlock()
	if final != "backlog" {
		t.Errorf("after a listing that began later disagreed, X = %q, want backlog (the later listing wins)", final)
	}
}

// F03 + provider robustness: the staleness stamp is taken per source,
// immediately before that source's own List call — not once for the whole
// refresh, and not delayed by any other source's. Sources are listed
// concurrently (provider robustness), so the test that once proved
// "per-source, not global" by showing fast's stamp waited for slow's ~300ms
// call now proves the opposite: fast's own stamp must NOT wait for slow's,
// because each is taken independently, right before that source's own call,
// on its own goroutine.
func TestStaleListingGenerationIsStampedPerSource(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "providers", "slow", "list.sh"), "#!/bin/sh\nsleep 0.3\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "slow", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fast", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fast", "move.sh"), "#!/bin/sh\nexit 0\n")
	for _, name := range []string{"reposlow", "repofast"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
stages:
  - name: backlog
  - name: done
    terminal: true
sources:
  - name: slow
    provider: slow
    workdir: ./reposlow
  - name: fast
    provider: fast
    workdir: ./repofast
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, runner.New(filepath.Join(dir, "runs")))
	s.ctx = context.Background()

	t0 := time.Now()
	s.refresh(s.ctx)

	s.mu.RLock()
	slowGen, fastGen := s.sourceGen["slow"], s.sourceGen["fast"]
	s.mu.RUnlock()
	if slowGen.Before(t0) {
		t.Fatalf("slow's own generation was not stamped during this refresh")
	}
	if fastGen.Before(t0) {
		t.Fatalf("fast's own generation was not stamped during this refresh")
	}
	// Each is its own independent stamp, not a single value shared by the
	// whole refresh: fast's must not have to wait out slow's ~300ms sleep,
	// which a global, once-per-refresh stamp — or a sequential loop — would
	// force.
	if fastGen.Sub(t0) > 250*time.Millisecond {
		t.Errorf("fast's generation was stamped %v after refresh started; it must not wait for slow's own ~300ms List call", fastGen.Sub(t0))
	}
	if slowGen.Sub(t0) > 250*time.Millisecond {
		t.Errorf("slow's generation was stamped %v after refresh started; it must be stamped immediately before its own List call, not after", slowGen.Sub(t0))
	}
}

// F03: an item whose transition is still running when a listing lands, and
// one whose transition completed after that listing began, are not removed
// from the board by a listing that did not include them.
func TestListingOmittingAnItemStillInFlightDoesNotRemoveIt(t *testing.T) {
	cfg, r, _, _, _, omitX := racyBoard(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.refresh(s.ctx)

	// Mark X as though a transition were actively running for it — the
	// state mergeSourceListing must check to keep an in-flight item off the
	// tombstone path even though the listing below omits it entirely.
	s.working.Store("s1:x", struct{}{})
	defer s.working.Delete("s1:x")

	if err := os.WriteFile(omitX, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.refresh(s.ctx)

	s.mu.RLock()
	stillThere := stageOf(s.state.Items, "s1:x") != ""
	s.mu.RUnlock()
	if !stillThere {
		t.Error("X was removed from the board by a listing that omitted it while its transition was still in flight")
	}
}

// F03: a resting deferral set by a transition that completed after its
// source's listing began survives that listing's clearing of resting; one
// set before it is still cleared.
func TestRestingSurvivesAListingItsSourceBeganBeforeTheDeferral(t *testing.T) {
	cfg, r, dir, block, started, _ := racyBoard(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.refresh(s.ctx)

	if err := os.WriteFile(block, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(started); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.refresh(s.ctx)
	}()
	waitFor(t, "the listing to begin", func() bool { return countLines(started) > 0 })

	// A no-op transition defers X, after this source's listing already began.
	s.mu.Lock()
	s.resting["s1:x"] = true
	s.restingAt["s1:x"] = time.Now()
	s.mu.Unlock()

	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	<-done

	s.mu.RLock()
	stillResting := s.resting["s1:x"]
	s.mu.RUnlock()
	if !stillResting {
		t.Error("a deferral set after the listing began did not survive that listing's clearing of resting")
	}
	_ = dir
}

func TestRestingIsClearedByAListingThatBeganAfterTheDeferral(t *testing.T) {
	cfg, r, _, _, _, _ := racyBoard(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.refresh(s.ctx)

	s.mu.Lock()
	s.resting["s1:x"] = true
	s.restingAt["s1:x"] = time.Now()
	s.mu.Unlock()

	s.refresh(s.ctx) // begins after the deferral was set

	s.mu.RLock()
	stillResting := s.resting["s1:x"]
	s.mu.RUnlock()
	if stillResting {
		t.Error("a deferral set before the listing began was not cleared by it")
	}
}

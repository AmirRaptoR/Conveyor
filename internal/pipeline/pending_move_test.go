package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

func countLines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(b)))
}

// stuckOutgoingMove builds a pipeline whose move script succeeds entering
// "working" but always refuses to move an item on to "done" — the outgoing
// write a stage's own success does not survive (F04).
func stuckOutgoingMove(t *testing.T) (cfg *config.Config, root, workRuns string) {
	t.Helper()
	dir := t.TempDir()
	workRuns = filepath.Join(dir, "workruns")
	writeExec(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), `#!/bin/sh
body=$(cat)
case "$body" in
  *'"stage": "done"'*) exit 1 ;;
  *) exit 0 ;;
esac
`)
	writeExec(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\necho x >> "+workRuns+"\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
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
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return c, filepath.Join(dir, "runs"), workRuns
}

// F04: once a stage script has completed, a failing outgoing move must not
// cause it to run a second time — only the move itself is retried. Proven
// across a restart too: a fresh Engine and Runner over the same data
// directory must still find the pending move recorded against the run and
// must not re-run the stage.
func TestOutgoingMoveFailureRetriesOnlyTheMoveNotTheStage(t *testing.T) {
	cfg, root, workRuns := stuckOutgoingMove(t)
	moveRuns := filepath.Join(filepath.Dir(root), "moveruns")
	writeExec(t, filepath.Join(filepath.Dir(root), "providers", "fake", "move.sh"), `#!/bin/sh
echo x >> `+moveRuns+`
body=$(cat)
case "$body" in
  *'"stage": "done"'*) exit 1 ;;
  *) exit 0 ;;
esac
`)
	ctx := context.Background()
	item := &model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}

	e1 := New(cfg, runner.New(root))
	tr1, err1 := e1.Advance(ctx, "s1", item, "working", model.Resume{})
	if err1 == nil {
		t.Fatalf("expected the outgoing move to fail, got a clean transition: %+v", tr1)
	}
	if item.Stage != "working" {
		t.Fatalf("item.Stage = %q, want working — the stage itself succeeded", item.Stage)
	}
	if got := countLines(workRuns); got != 1 {
		t.Fatalf("work.sh ran %d time(s) after the first attempt, want 1", got)
	}
	if got := countLines(moveRuns); got != 2 {
		t.Fatalf("provider was called %d time(s) on the first attempt, want enter + outgoing move", got)
	}

	// The scheduler wakes and dispatches this same item back into "working"
	// (pipeline.Target's recovery case) with the *same* engine.
	tr2, err2 := e1.Advance(ctx, "s1", item, "working", model.Resume{})
	if err2 == nil {
		t.Fatalf("expected the move to keep failing, got: %+v", tr2)
	}
	if got := countLines(workRuns); got != 1 {
		t.Errorf("work.sh ran %d time(s) after a same-engine retry, want 1 — the stage must not re-run", got)
	}
	if tr2.RunID != tr1.RunID {
		t.Errorf("a retry produced a new run id (%q vs %q) — no new stage run should have been made", tr2.RunID, tr1.RunID)
	}
	if got := countLines(moveRuns); got != 3 {
		t.Errorf("same-engine retry made %d provider calls total, want exactly one additional outgoing move", got)
	}

	// A restart: a fresh Engine and Runner over the same root. The pending
	// move is read back off the run's own directory, not engine memory.
	e2 := New(cfg, runner.New(root))
	tr3, err3 := e2.Advance(ctx, "s1", item, "working", model.Resume{})
	if err3 == nil {
		t.Fatalf("expected the move to keep failing after a restart, got: %+v", tr3)
	}
	if got := countLines(workRuns); got != 1 {
		t.Errorf("work.sh ran %d time(s) after a restart, want 1 — a fresh engine must not re-run the stage either", got)
	}
	if tr3.RunID != tr1.RunID {
		t.Errorf("a post-restart retry produced a new run id (%q vs %q)", tr3.RunID, tr1.RunID)
	}
	if got := countLines(moveRuns); got != 4 {
		t.Errorf("post-restart retry made %d provider calls total, want exactly one additional outgoing move", got)
	}
}

func TestFailedProviderMarkRetriesOnlyTheMarkNotTheStage(t *testing.T) {
	cfg, root, workRuns := stuckOutgoingMove(t)
	dir := filepath.Dir(root)
	moveRuns := filepath.Join(dir, "moveruns")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), `#!/bin/sh
echo x >> `+moveRuns+`
body=$(cat)
case "$body" in
  *'"blocked": true'*) exit 1 ;;
  *) exit 0 ;;
esac
`)
	writeExec(t, filepath.Join(dir, "work.sh"), `#!/bin/sh
echo x >> `+workRuns+`
printf '%s\n' '{"blocked":true,"kind":"decision","reason":"choose"}' > "$CONVEYOR_RESULT"
exit 20
`)
	ctx := context.Background()
	item := &model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}
	e1 := New(cfg, runner.New(root))
	tr1, err := e1.Advance(ctx, "s1", item, "working", model.Resume{})
	if err == nil || !tr1.ProviderWritePending || tr1.Blocked {
		t.Fatalf("failed mark transition = %+v, err=%v", tr1, err)
	}
	if got := countLines(workRuns); got != 1 {
		t.Fatalf("stage ran %d times, want 1", got)
	}
	if got := countLines(moveRuns); got != 2 {
		t.Fatalf("provider was called %d time(s) on the first attempt, want enter + mark", got)
	}

	e2 := New(cfg, runner.New(root))
	tr2, err := e2.Advance(ctx, "s1", item, "working", model.Resume{})
	if err == nil || !tr2.ProviderWritePending {
		t.Fatalf("retried mark transition = %+v, err=%v", tr2, err)
	}
	if got := countLines(workRuns); got != 1 {
		t.Fatalf("stage reran while retrying its mark: %d runs", got)
	}
	if got := countLines(moveRuns); got != 3 {
		t.Fatalf("mark retry made %d provider calls total, want exactly one additional mark", got)
	}
}

// A provider outage is not a decision for a person. The server owns bounded
// backoff between these calls; the engine keeps the successful stage's pending
// move available however many attempts the provider needs.
func TestOutgoingMoveFailureNeverMarksOrRerunsTheStage(t *testing.T) {
	cfg, root, _ := stuckOutgoingMove(t)
	ctx := context.Background()
	item := &model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}
	e := New(cfg, runner.New(root))

	if _, err := e.Advance(ctx, "s1", item, "working", model.Resume{}); err == nil {
		t.Fatal("expected the initial attempt's outgoing move to fail")
	}

	var last *Transition
	const retries = 5
	for i := 0; i < retries; i++ {
		tr, _ := e.Advance(ctx, "s1", item, "working", model.Resume{})
		last = tr
	}
	if last == nil || last.Blocked {
		t.Fatalf("provider outage became a mark after %d retries: %+v", retries, last)
	}
	if last.Attempts != retries {
		t.Errorf("Attempts = %d, want %d", last.Attempts, retries)
	}
	if item.Blocked {
		t.Error("item was marked for a provider outage")
	}
	if _, ok := runner.PendingMove(root, item.ID, "working"); !ok {
		t.Error("the pending move was discarded while the provider was unavailable")
	}
	if got := countLines(filepath.Join(root, "work-runs")); got > 1 {
		t.Errorf("stage ran %d times during outgoing move retries, want 1", got)
	}
}

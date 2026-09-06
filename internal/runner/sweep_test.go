package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func writeRun(t *testing.T, root, day, id string, outcome model.Outcome) string {
	t.Helper()
	dir := filepath.Join(root, day, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(model.Run{ID: id, Outcome: outcome})
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func outcomeOf(t *testing.T, dir string) model.Outcome {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r model.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r.Outcome
}

// Two runs claiming to be in flight cannot both be true. Anything still marked
// running when a scheduler starts was killed.
func TestSweepSettlesKilledRuns(t *testing.T) {
	root := t.TempDir()
	killed := writeRun(t, root, "2026-08-28", "a", model.OutcomeRunning)
	done := writeRun(t, root, "2026-08-28", "b", model.OutcomeSuccess)
	failed := writeRun(t, root, "2026-08-27", "c", model.OutcomeFailure)

	n, err := SweepInterrupted(root)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("swept %d runs, want 1", n)
	}
	if got := outcomeOf(t, killed); got != model.OutcomeInterrupted {
		t.Errorf("killed run = %q, want interrupted", got)
	}
	// A settled verdict is a fact and must not be rewritten.
	if got := outcomeOf(t, done); got != model.OutcomeSuccess {
		t.Errorf("finished run = %q, want success", got)
	}
	if got := outcomeOf(t, failed); got != model.OutcomeFailure {
		t.Errorf("failed run = %q, want failure", got)
	}
}

func TestSweepToleratesNoRunsYet(t *testing.T) {
	n, err := SweepInterrupted(filepath.Join(t.TempDir(), "never-created"))
	if err != nil || n != 0 {
		t.Fatalf("sweep of a missing root = (%d, %v), want (0, nil)", n, err)
	}
}

// A meta.json the sweep cannot read (as opposed to one that simply is not
// there) is worth reporting: silently skipping it once meant a run nobody
// could explain, indistinguishable from one that behaved.
func TestSweepReportsRatherThanSilentlySkipsUnreadableRuns(t *testing.T) {
	root := t.TempDir()
	killed := writeRun(t, root, "2026-08-28", "a", model.OutcomeRunning)
	unreadable := writeRun(t, root, "2026-08-28", "b", model.OutcomeRunning)
	if err := os.Chmod(filepath.Join(unreadable, "meta.json"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(unreadable, "meta.json"), 0o644) })

	n, err := SweepInterrupted(root)
	if err == nil {
		t.Fatal("expected an error naming the unreadable meta.json")
	}
	if !strings.Contains(err.Error(), unreadable) && !strings.Contains(err.Error(), "meta.json") {
		t.Errorf("error %v does not name the unreadable run", err)
	}
	// The one run it could read is still settled — one bad directory must not
	// stop the rest of the sweep.
	if n != 1 {
		t.Errorf("swept %d runs, want 1 (the readable one)", n)
	}
	if got := outcomeOf(t, killed); got != model.OutcomeInterrupted {
		t.Errorf("readable run = %q, want interrupted", got)
	}
}

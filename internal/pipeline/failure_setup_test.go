package pipeline

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

func TestRepeatedRunnerSetupFailuresHaveStableEvidenceAndQuarantine(t *testing.T) {
	budgets := store.OpenBudgets(filepath.Join(t.TempDir(), "budgets.json"))
	var signature string
	for i := 0; i < 2; i++ {
		runDir := filepath.Join(t.TempDir(), "runs", "2026-09-26", "unique-run")
		run := model.Run{
			ID: "run-" + string(rune('1'+i)), To: "working", Dir: runDir,
			Outcome: model.OutcomeFailure, ExitCode: -1,
			Error: "create result channel " + filepath.Join(runDir, "result.json") + ": permission denied",
		}
		sig, reason := FailureEvidence(run, nil, 0)
		if sig == "" || reason == "" {
			t.Fatalf("setup failure %d has empty evidence: signature=%q reason=%q", i+1, sig, reason)
		}
		if i > 0 && sig != signature {
			t.Fatalf("repeated setup signatures differ: %q != %q", sig, signature)
		}
		signature = sig
		failure, err := budgets.RecordFailure("s1:1", "working", sig, reason, run.ID, 2, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if failure.Quarantined != (i == 1) {
			t.Fatalf("failure %d quarantined=%v", i+1, failure.Quarantined)
		}
	}
}

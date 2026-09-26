package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func TestStartupReconcilesDurableRunMissingFromAudit(t *testing.T) {
	cfg, r := boardFor(t)
	first := New(cfg, r)
	if !first.audit.Status().ReconciliationComplete {
		t.Fatal("initial run reconciliation did not complete")
	}
	run := model.Run{ID: "120000.000-reconcile", Kind: "stage", ItemID: "s1:1", Source: "s1", To: "working",
		StartedAt: time.Now().Add(-time.Minute), FinishedAt: time.Now(), Outcome: model.OutcomeSuccess,
		NextStage: "done", MoveConfirmed: true, RetentionClass: "model"}
	dir := filepath.Join(r.Root, time.Now().UTC().Format("2006-01-02"), run.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(run)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := New(cfg, r)
	status := restarted.audit.Status()
	if !status.ReconciliationComplete || !status.EvidenceComplete || status.RunWatermark == "" {
		t.Fatalf("status = %#v", status)
	}
	var found int
	for _, event := range restarted.audit.Since(time.Time{}) {
		if event.Kind == "run" && event.RunID == run.ID && event.Confirmed {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("reconciled run count = %d, want 1", found)
	}
}

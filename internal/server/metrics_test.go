package server

import (
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

func TestOperationalMetricsUseStructuredSevenDayEvidence(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	runs := []RunMeta{
		{Run: model.Run{Kind: "stage", ItemID: "s:1", To: "work", StartedAt: now.Add(-time.Hour), Outcome: model.OutcomeSuccess, MoveConfirmed: true, NextStage: "done", RetentionClass: "model"}},
		{Run: model.Run{Kind: "stage", ItemID: "s:1", To: "work", StartedAt: now.Add(-2 * time.Hour), Outcome: model.OutcomeFailure, RetentionClass: "model"}},
		{Run: model.Run{Kind: "stage", ItemID: "s:old", To: "work", StartedAt: now.Add(-8 * 24 * time.Hour), Outcome: model.OutcomeFailure, RetentionClass: "model"}},
	}
	events := []store.AuditEvent{
		{At: now.Add(-30 * time.Minute), Kind: "human", Action: "unblock"},
		{At: now.Add(-20 * time.Minute), Kind: "recovery", DurationNs: int64(10 * time.Minute)},
	}
	m := calculateMetrics(now, runs, events, map[string]bool{"done": true})
	if m.StageRuns != 2 || m.SuccessfulRuns != 1 || m.Completions != 1 || m.SuccessRate != .5 || m.RetriesPerCompletion != 1 || m.WastedModelRuns != 1 || m.HumanInterventions != 1 || m.BlockedToRecovered != 10*time.Minute {
		t.Fatalf("metrics = %#v", m)
	}
}

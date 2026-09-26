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
	coverage := metricCoverage{StartedAt: now.Add(-metricsWindow), Healthy: true}
	m := calculateMetrics(now, coverage, runs, events, map[string]bool{"done": true})
	if m.StageRuns != 2 || m.SuccessfulRuns != 1 || m.Completions != 1 || m.SuccessRate != .5 || m.RetriesPerCompletion != 1 || m.WastedModelRuns != 1 || m.HumanInterventions != 1 || m.BlockedToRecovered != 10*time.Minute {
		t.Fatalf("metrics = %#v", m)
	}
	if !m.CoverageComplete {
		t.Fatal("a complete seven-day observation window was reported incomplete")
	}
	if fresh := calculateMetrics(now, metricCoverage{StartedAt: now.Add(-time.Hour), Healthy: true}, runs, events, map[string]bool{"done": true}); fresh.CoverageComplete {
		t.Fatal("one hour of evidence was reported as a seven-day soak")
	}
	if broken := calculateMetrics(now, metricCoverage{StartedAt: now.Add(-metricsWindow), Error: "audit truncated"}, runs, events, map[string]bool{"done": true}); broken.CoverageComplete || broken.EvidenceError == "" {
		t.Fatalf("broken evidence metrics = %#v", broken)
	}
}

func TestCompletionRateDoesNotTreatSuccessfulNonterminalChurnAsCompletion(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	runs := []RunMeta{
		{Run: model.Run{Kind: "stage", Outcome: model.OutcomeSuccess, MoveConfirmed: true, NextStage: "review", StartedAt: now}},
		{Run: model.Run{Kind: "stage", Outcome: model.OutcomeSuccess, MoveConfirmed: true, NextStage: "work", StartedAt: now}},
		{Run: model.Run{Kind: "stage", Outcome: model.OutcomeSuccess, MoveConfirmed: true, NextStage: "review", StartedAt: now}},
	}
	m := calculateMetrics(now, metricCoverage{StartedAt: now.Add(-metricsWindow), Healthy: true}, runs, nil, map[string]bool{"done": true})
	if m.SuccessRate != 1 || m.CompletionRate != 0 || m.Completions != 0 {
		t.Fatalf("metrics = %#v, want successful churn with zero completion rate", m)
	}
}

func TestStartingANewSoakCannotInheritOldElapsedCoverage(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Release.Revision = "same-revision"
	started := time.Now().UTC().Add(time.Second)
	first, err := s.startSoak(started)
	if err != nil {
		t.Fatal(err)
	}
	later := started.Add(metricsWindow)
	if got := s.metrics(later); !got.CoverageComplete {
		t.Fatalf("first soak never reached coverage: %#v", got)
	}
	second, err := s.startSoak(later)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || s.metrics(later).CoverageComplete {
		t.Fatalf("new soak inherited old coverage: first=%#v second=%#v metrics=%#v", first, second, s.metrics(later))
	}
}

func TestRunAuditRetryThenConfirmationCountsOnce(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	now := time.Now().UTC()
	s.state.Release.Revision = "revision"
	if _, err := s.startSoak(now); err != nil {
		t.Fatal(err)
	}
	pending := store.AuditEvent{At: now, Kind: "run", RunID: "run-1", ItemID: "s1:1", Stage: "working", Outcome: string(model.OutcomeSuccess), NextStage: "done"}
	if err := s.audit.Append(pending, now); err != nil {
		t.Fatal(err)
	}
	pending.Confirmed = true
	if err := s.audit.Append(pending, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	m := s.metrics(now.Add(time.Minute))
	if m.StageRuns != 1 || m.SuccessfulRuns != 1 || m.Completions != 1 || m.Retries != 0 {
		t.Fatalf("metrics = %#v, retry update was counted as another run", m)
	}
}

package server

import (
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

const metricsWindow = 7 * 24 * time.Hour

// Metrics contains fixed seven-day operational aggregates. Run facts come from
// structured metadata and its bounded audit projection; logs, plans and control
// records are never metrics inputs.
type Metrics struct {
	WindowStart          time.Time     `json:"windowStart"`
	WindowEnd            time.Time     `json:"windowEnd"`
	ObservationSince     time.Time     `json:"observationSince"`
	CoverageComplete     bool          `json:"coverageComplete"`
	EvidenceHealthy      bool          `json:"evidenceHealthy"`
	EvidenceError        string        `json:"evidenceError,omitempty"`
	StageRuns            int           `json:"stageRuns"`
	SuccessfulRuns       int           `json:"successfulRuns"`
	SuccessRate          float64       `json:"successRate"`
	Completions          int           `json:"completions"`
	CompletionRate       float64       `json:"completionsPerDay"`
	Retries              int           `json:"retries"`
	RetriesPerCompletion float64       `json:"retriesPerCompletion"`
	BlockedRecoveries    int           `json:"blockedRecoveries"`
	BlockedToRecovered   time.Duration `json:"blockedToRecoveredNs"`
	WastedModelRuns      int           `json:"wastedModelRuns"`
	HumanInterventions   int           `json:"humanInterventions"`
}

type metricCoverage struct {
	StartedAt time.Time
	Healthy   bool
	Error     string
}

func calculateMetrics(now time.Time, coverage metricCoverage, runs []RunMeta, events []store.AuditEvent, terminal map[string]bool) Metrics {
	m := Metrics{WindowStart: now.Add(-metricsWindow), WindowEnd: now, ObservationSince: coverage.StartedAt,
		EvidenceHealthy: coverage.Healthy, EvidenceError: coverage.Error,
		CoverageComplete: coverage.Healthy && !coverage.StartedAt.IsZero() && !coverage.StartedAt.After(now.Add(-metricsWindow))}
	for _, run := range runs {
		at := run.FinishedAt
		if at.IsZero() {
			at = run.StartedAt
		}
		if run.Kind != "stage" || at.Before(m.WindowStart) || at.After(now) || run.Outcome == model.OutcomeRunning {
			continue
		}
		m.StageRuns++
		success := run.Outcome == model.OutcomeSuccess && run.MoveConfirmed
		if success {
			m.SuccessfulRuns++
		} else {
			m.Retries++
		}
		if success && terminal[run.NextStage] {
			m.Completions++
		}
		if run.RetentionClass == "model" && !success {
			m.WastedModelRuns++
		}
	}
	if m.StageRuns > 0 {
		m.SuccessRate = float64(m.SuccessfulRuns) / float64(m.StageRuns)
	}
	if m.Completions > 0 {
		m.RetriesPerCompletion = float64(m.Retries) / float64(m.Completions)
	}
	m.CompletionRate = float64(m.Completions) / 7
	var recoveryTotal time.Duration
	for _, event := range events {
		if event.At.Before(m.WindowStart) || event.At.After(now) {
			continue
		}
		switch event.Kind {
		case "human":
			m.HumanInterventions++
		case "recovery":
			m.BlockedRecoveries++
			recoveryTotal += time.Duration(event.DurationNs)
		}
	}
	if m.BlockedRecoveries > 0 {
		m.BlockedToRecovered = recoveryTotal / time.Duration(m.BlockedRecoveries)
	}
	return m
}

func (s *Server) metrics(now time.Time) Metrics {
	return s.metricsAt(now)
}

func (s *Server) metricsAt(now time.Time) Metrics {
	status := s.audit.Status()
	soak := s.soakStore.Get()
	s.mu.RLock()
	revision := s.state.Release.Revision
	s.mu.RUnlock()
	coverage := metricCoverage{StartedAt: soak.StartedAt, Healthy: status.Healthy}
	switch {
	case !status.Healthy:
		coverage.Error = status.Error
	case soak.StartedAt.IsZero():
		coverage.Healthy, coverage.Error = false, "soak has not been started"
	case soak.Revision != revision:
		coverage.Healthy, coverage.Error = false, "soak revision does not match the running release"
	case soak.EvidenceID == "" || soak.EvidenceID != status.ContinuityID:
		coverage.Healthy, coverage.Error = false, "soak audit evidence identity does not match"
	case status.ContinuitySince.After(soak.StartedAt):
		coverage.Healthy, coverage.Error = false, "audit evidence continuity began after the soak"
	}
	cutoff := now.Add(-metricsWindow)
	if !soak.StartedAt.IsZero() && soak.StartedAt.After(cutoff) {
		cutoff = soak.StartedAt
	}
	events := s.audit.Since(cutoff)
	var runs []RunMeta
	for _, event := range events {
		if event.Kind != "run" {
			continue
		}
		class := ""
		if event.ModelRun {
			class = "model"
		}
		runs = append(runs, RunMeta{Run: model.Run{ID: event.RunID, Kind: "stage", ItemID: event.ItemID,
			To: event.Stage, StartedAt: event.At, FinishedAt: event.At, Outcome: model.Outcome(event.Outcome), NextStage: event.NextStage,
			MoveConfirmed: event.Confirmed, RetentionClass: class}})
	}
	terminal := map[string]bool{}
	for _, stage := range s.cfg.Stages {
		terminal[stage.Name] = stage.Terminal
	}
	return calculateMetrics(now, coverage, runs, events, terminal)
}

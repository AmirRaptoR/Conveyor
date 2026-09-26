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

func calculateMetrics(now time.Time, runs []RunMeta, events []store.AuditEvent, terminal map[string]bool) Metrics {
	m := Metrics{WindowStart: now.Add(-metricsWindow), WindowEnd: now}
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
	events := s.audit.Since(now.Add(-metricsWindow))
	audited := map[string]bool{}
	var runs []RunMeta
	for _, event := range events {
		if event.Kind != "run" {
			continue
		}
		audited[event.RunID] = true
		class := ""
		if event.ModelRun {
			class = "model"
		}
		runs = append(runs, RunMeta{Run: model.Run{ID: event.RunID, Kind: "stage", ItemID: event.ItemID,
			To: event.Stage, StartedAt: event.At, FinishedAt: event.At, Outcome: model.Outcome(event.Outcome), NextStage: event.NextStage,
			MoveConfirmed: event.Confirmed, RetentionClass: class}})
	}
	s.walkRuns(func(run RunMeta) bool {
		if !run.StartedAt.IsZero() && run.StartedAt.Before(now.Add(-metricsWindow)) {
			return false
		}
		if !audited[run.ID] {
			runs = append(runs, run)
		}
		return true
	})
	terminal := map[string]bool{}
	for _, stage := range s.cfg.Stages {
		terminal[stage.Name] = stage.Terminal
	}
	return calculateMetrics(now, runs, events, terminal)
}

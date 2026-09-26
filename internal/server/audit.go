package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

func (s *Server) beginHumanAudit(action, itemID, by string) (string, error) {
	return s.audit.BeginHuman(action, itemID, by, time.Now())
}

func (s *Server) resolveHumanAudit(intent string, committed bool) {
	if intent == "" {
		return
	}
	if err := s.audit.ResolveHuman(intent, committed, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: resolve human audit intent %s: %v\n", intent, err)
	}
}

func (s *Server) beginHumanAuditHTTP(w http.ResponseWriter, r *http.Request, action, itemID string) (string, bool) {
	intent, err := s.beginHumanAudit(action, itemID, requestedBy(r))
	if err != nil {
		http.Error(w, "could not persist audit intent: "+err.Error(), http.StatusInternalServerError)
		return "", false
	}
	return intent, true
}

func (s *Server) auditRecovery(itemID string, duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	if err := s.audit.Append(store.AuditEvent{At: time.Now(), Kind: "recovery", Action: "unblocked", ItemID: itemID, DurationNs: int64(duration)}, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: persist recovery audit event: %v\n", err)
	}
}

func auditEventFromRun(run RunMeta) store.AuditEvent {
	at := run.FinishedAt
	if at.IsZero() {
		at = run.StartedAt
	}
	return store.AuditEvent{At: at, Kind: "run", RunID: run.ID, ItemID: run.ItemID,
		Stage: run.To, Outcome: string(run.Outcome), NextStage: run.NextStage, BlockKind: run.MarkKind,
		ModelRun: run.RetentionClass == "model", Confirmed: run.MoveConfirmed}
}

// reconcileAuditRuns is the one startup-only history reconstruction. The
// durable cursor means metrics and watchdog requests never walk run history.
func (s *Server) reconcileAuditRuns() error {
	watermark, err := s.audit.BeginRunReconciliation()
	if err != nil {
		return err
	}
	type entry struct {
		cursor string
		run    RunMeta
	}
	var entries []entry
	days, err := os.ReadDir(s.run.Root)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		runs, err := os.ReadDir(filepath.Join(s.run.Root, day.Name()))
		if err != nil {
			return err
		}
		for _, runDir := range runs {
			if !runDir.IsDir() {
				continue
			}
			cursor := day.Name() + "/" + runDir.Name()
			if cursor <= watermark {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.run.Root, day.Name(), runDir.Name(), "meta.json"))
			if err != nil {
				return fmt.Errorf("reconcile run %s: %w", cursor, err)
			}
			var run RunMeta
			if err := json.Unmarshal(b, &run); err != nil {
				return fmt.Errorf("reconcile run %s: %w", cursor, err)
			}
			entries = append(entries, entry{cursor: cursor, run: run})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].cursor < entries[j].cursor })
	nextWatermark := watermark
	for _, entry := range entries {
		if entry.run.Outcome == model.OutcomeRunning {
			return fmt.Errorf("reconcile run %s: run is not authoritatively settled", entry.cursor)
		}
		if entry.run.Kind == "stage" && entry.run.Outcome != model.OutcomeRunning {
			event := auditEventFromRun(entry.run)
			if event.At.IsZero() {
				return fmt.Errorf("reconcile run %s: settled stage run has no timestamp", entry.cursor)
			}
			if err := s.audit.Append(event, time.Now()); err != nil {
				return err
			}
		}
		nextWatermark = entry.cursor
	}
	return s.audit.CompleteRunReconciliation(nextWatermark)
}

func (s *Server) auditRun(tr *pipeline.Transition) {
	if tr == nil || tr.RunID == "" {
		return
	}
	event := store.AuditEvent{At: time.Now(), Kind: "run", RunID: tr.RunID, ItemID: tr.Item.ID,
		Stage: tr.Stage, Outcome: string(tr.Outcome), NextStage: tr.Next, BlockKind: tr.Kind,
		ModelRun: tr.ModelRun, Confirmed: !tr.ProviderWritePending && tr.Err == nil}
	if err := s.audit.Append(event, event.At); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: persist run audit event: %v\n", err)
	}
}

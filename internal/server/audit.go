package server

import (
	"fmt"
	"os"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

func (s *Server) auditHuman(action, itemID, by string) {
	if err := s.audit.Append(store.AuditEvent{At: time.Now(), Kind: "human", Action: action, ItemID: itemID, By: by}, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: persist %s audit event: %v\n", action, err)
	}
}

func (s *Server) auditRecovery(itemID string, duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	if err := s.audit.Append(store.AuditEvent{At: time.Now(), Kind: "recovery", Action: "unblocked", ItemID: itemID, DurationNs: int64(duration)}, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: persist recovery audit event: %v\n", err)
	}
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

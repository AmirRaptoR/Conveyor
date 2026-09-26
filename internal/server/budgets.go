package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

// budgetDay is the caller's idea of "today" for MaxRunsPerDay: UTC, so it
// means the same thing whatever host this process runs on — the same reason
// CONVEYOR_DEADLINE is always read in UTC.
func budgetDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// budgetAvailable reports whether itemID has room under both configured
// ceilings right now, without spending anything — the same "fact about the
// world right now" pre-filter manuallyPaused and agentPaused already are for
// launch's free-item pass, so an item that cannot possibly be claimed is
// never handed to Pick in the first place. claim (via reserveBudget) is what
// actually spends a run, atomically; a race between this read and that
// reservation can only ever cost the item one extra scheduler pass, never
// overspend a ceiling.
func (s *Server) budgetAvailable(itemID, source, stage string) bool {
	if s.cfg.AgentFor(source, stage) == "" {
		return true
	}
	ok, _ := s.budgets.PeekModel(itemID, stage, budgetDay(time.Now()), s.cfg.Budgets.MaxRunsPerItem, s.cfg.Budgets.MaxRunsPerDay)
	return ok
}

// reserveBudget spends one run against itemID's ceilings, atomically — the
// only place a run is actually counted, called from claim() once a
// transition is certain to launch (busy slots and paused sources are refused
// before this point and spend nothing).
func (s *Server) reserveBudget(itemID, source, stage string) (ok bool, reason string) {
	if s.cfg.AgentFor(source, stage) == "" {
		return true, ""
	}
	ok, reason, err := s.budgets.ReserveModel(itemID, stage, budgetDay(time.Now()), s.cfg.Budgets.MaxRunsPerItem, s.cfg.Budgets.MaxRunsPerDay)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: %s: could not record a budget reservation: %v\n", itemID, err)
		return false, "error"
	}
	return ok, reason
}

// whyBudgetExhausted names which ceiling refused a claim, for a manual start
// that meets the same wall the scheduler does. It re-derives which ceiling is
// responsible with Peek rather than threading claim()'s own refusal reason
// through claimRefusal, which carries only claimBudgetExhausted — a read
// this cheap is simpler than a second channel for the same fact.
func (s *Server) whyBudgetExhausted(itemID, stage string) string {
	_, reason := s.budgets.PeekModel(itemID, stage, budgetDay(time.Now()), s.cfg.Budgets.MaxRunsPerItem, s.cfg.Budgets.MaxRunsPerDay)
	switch reason {
	case "item":
		runs, _ := s.budgets.Usage(itemID)
		return fmt.Sprintf("%s has reached its execution budget (%d of %d runs) — an operator override is required to run it again",
			itemID, runs, s.cfg.Budgets.MaxRunsPerItem)
	case "day":
		return fmt.Sprintf("the board's daily execution budget (%d runs) is spent for today (UTC) — an operator override is required to run %s now",
			s.cfg.Budgets.MaxRunsPerDay, itemID)
	case "evidence", "quarantine":
		if f, ok := s.budgets.Failure(itemID); ok {
			state := "held"
			if f.Quarantined {
				state = "quarantined"
			}
			return fmt.Sprintf("%s is %s after %d identical failure(s): %s; next eligible when %s, or after one scoped operator override",
				itemID, state, f.Count, f.Reason, f.ReleaseCondition)
		}
	default:
		return fmt.Sprintf("%s's execution budget could not be checked", itemID)
	}
	return fmt.Sprintf("%s's model run is held pending external evidence", itemID)
}

// overrideBudget records an operator's deliberate decision to let one item
// keep spending past its configured ceilings, persists it, and updates the
// board. by is the identity behind it, the same as a pause's or a
// cancellation's audit trail.
func (s *Server) overrideBudget(itemID, stage, reason, by string) error {
	failure, failureHeld := s.budgets.Failure(itemID)
	if err := s.budgets.Grant(itemID, stage, store.BudgetOverride{Reason: reason, By: by, At: time.Now()}); err != nil {
		return err
	}
	// Clear only the in-memory rest created by this exact failure hold. A typed
	// recovery or operator cancellation that arrived meanwhile must still win.
	if failureHeld && failure.Held && failure.Stage == stage {
		s.mu.Lock()
		_, recovering := s.recovery.Get("item", itemID)
		if waiting, ok := s.waiting[itemID]; !recovering && ok && waiting.Why == failure.ReleaseCondition {
			delete(s.resting, itemID)
			delete(s.restingAt, itemID)
			delete(s.waiting, itemID)
		}
		s.mu.Unlock()
	}
	s.hub.publish(event{Kind: "state"})
	s.wakeUp()
	return nil
}

// restoreBudget lifts an override and reimposes the item's configured
// ceilings. Idempotent: restoring an item with no override is not an error.
func (s *Server) restoreBudget(itemID, by string) error {
	if err := s.budgets.Revoke(itemID, by, time.Now()); err != nil {
		return err
	}
	s.hub.publish(event{Kind: "state"})
	return nil
}

// budgetViews is the board's per-item view of the execution-budget ledger,
// built fresh from onBoard rather than iterating the whole persisted file:
// an item long gone from every source has nothing here worth showing, the
// same reason Blocks and Times are rebuilt from the current item list rather
// than the store directly.
func (s *Server) budgetViews(items []model.Item) map[string]BudgetView {
	out := map[string]BudgetView{}
	deps := pipeline.NewDeps(s.cfg, items)
	for _, it := range items {
		stage := it.Stage
		if target, ok := pipeline.Target(s.cfg, &it, deps); ok {
			stage = target
		}
		runs, ov := s.budgets.Usage(it.ID)
		v := BudgetView{Runs: runs, Remaining: max(0, s.cfg.Budgets.MaxRunsPerItem-runs), ModelRun: s.cfg.AgentFor(it.Source, stage) != ""}
		if ov != nil {
			v.Override = &BudgetOverrideView{By: ov.By, Reason: ov.Reason, At: ov.At, Stage: ov.Stage, Remaining: ov.Remaining, UsedAt: ov.UsedAt, RevokedAt: ov.RevokedAt, RevokedBy: ov.RevokedBy}
		}
		for _, audit := range s.budgets.OverrideHistory(it.ID) {
			v.OverrideHistory = append(v.OverrideHistory, BudgetOverrideView{By: audit.By, Reason: audit.Reason, At: audit.At,
				Stage: audit.Stage, Remaining: audit.Remaining, UsedAt: audit.UsedAt, RevokedAt: audit.RevokedAt, RevokedBy: audit.RevokedBy})
		}
		out[it.ID] = v
	}
	return out
}

func (s *Server) failureViews(items []model.Item) map[string]FailureView {
	out := map[string]FailureView{}
	for _, item := range items {
		if f, ok := s.budgets.Failure(item.ID); ok && f.Held && f.Stage == item.Stage {
			out[item.ID] = FailureView{Stage: f.Stage, Signature: f.Signature, Reason: f.Reason, RunID: f.RunID,
				Count: f.Count, Quarantined: f.Quarantined, At: f.At, ReleaseCondition: f.ReleaseCondition}
		}
	}
	return out
}

// reconcileModelFailures closes the crash window between a completed run's
// durable meta.json and applyTransition's ledger update. Only the newest run
// in the item's current stage is authoritative.
func (s *Server) reconcileModelFailures(items []model.Item, failedSources map[string]string) []string {
	var warnings []string
	for _, it := range items {
		if failedSources[it.Source] != "" {
			continue
		}
		for _, stage := range s.cfg.Stages {
			if stage.Name == it.Stage || s.cfg.AgentFor(it.Source, stage.Name) == "" {
				continue
			}
			if run, ok := runner.LatestStageRun(s.run.Root, it.ID, stage.Name); ok &&
				(run.Outcome == model.OutcomeFailure || run.Outcome == model.OutcomeTimeout) {
				if err := s.budgets.ProcessFailureRun(it.ID, run.ID); err != nil {
					warnings = append(warnings, it.Source+": retire stale model failure: "+err.Error())
				}
			}
		}
		if err := s.budgets.ReconcileStage(it.ID, it.Stage); err != nil {
			warnings = append(warnings, it.Source+": reconcile stale model failure: "+err.Error())
			continue
		}
		if s.cfg.AgentFor(it.Source, it.Stage) == "" {
			continue
		}
		run, ok := runner.LatestStageRun(s.run.Root, it.ID, it.Stage)
		if !ok || run.Outcome == model.OutcomeRunning || run.Outcome == model.OutcomeInterrupted {
			continue
		}
		switch run.Outcome {
		case model.OutcomeFailure, model.OutcomeTimeout:
			if s.budgets.HasFailureRun(it.ID, run.ID) {
				continue
			}
			data, _ := os.ReadFile(filepath.Join(run.Dir, "result.json"))
			timeout := time.Duration(0)
			if stage, ok := s.cfg.Stage(it.Stage); ok {
				timeout = stage.Timeout.D()
			}
			signature, reason := pipeline.FailureEvidence(run, data, timeout)
			threshold := max(2, s.cfg.Budgets.QuarantineAfter)
			at := run.FinishedAt
			if at.IsZero() {
				at = run.StartedAt
			}
			if _, err := s.budgets.RecordFailure(it.ID, it.Stage, signature, reason, run.ID, threshold, at); err != nil {
				warnings = append(warnings, it.Source+": recover model failure gate: "+err.Error())
			}
		default:
			if err := s.budgets.ClearFailure(it.ID, it.Stage); err != nil {
				warnings = append(warnings, it.Source+": reconcile completed model run: "+err.Error())
			}
		}
	}
	return warnings
}

// budgetOverrideBodyLimit bounds POST /api/items/{id}/budget-override — a
// JSON object with one short string field.
const budgetOverrideBodyLimit = 4 << 10

// handleBudgetOverride lets an operator run one item past its configured
// execution ceiling, deliberately and with an audit record — CLAUDE.md's
// "only a person clears a mark", applied here to a budget rather than a
// mark: no amount of waiting lifts a per-item ceiling on its own, so only an
// explicit override does.
func (s *Server) handleBudgetOverride(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Reason string `json:"reason"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, budgetOverrideBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if bodyTooLarge(err) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `expected {"reason": "..."}`, http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		http.Error(w, "reason is required: an override with no reason is a mystery for whoever looks at this item next", http.StatusBadRequest)
		return
	}
	stage, ok := s.itemStage(id)
	if !ok {
		http.Error(w, "no item "+id+" on the board", http.StatusNotFound)
		return
	}
	if err := s.overrideBudget(id, stage, reason, requestedBy(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditHuman("budget-override", id, requestedBy(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) itemStage(id string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	deps := pipeline.NewDeps(s.cfg, s.state.Items)
	for _, it := range s.state.Items {
		if it.ID == id {
			if target, ok := pipeline.Target(s.cfg, &it, deps); ok {
				return target, true
			}
			return it.Stage, true
		}
	}
	return "", false
}

// handleBudgetRestore lifts an override placed by handleBudgetOverride.
// Idempotent: restoring an item with no override is not an error, the same
// as resuming a source that was never paused.
func (s *Server) handleBudgetRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.restoreBudget(id, requestedBy(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditHuman("budget-restore", id, requestedBy(r))
	w.WriteHeader(http.StatusNoContent)
}

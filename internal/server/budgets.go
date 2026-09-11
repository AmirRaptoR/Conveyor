package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

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
func (s *Server) budgetAvailable(itemID string) bool {
	ok, _ := s.budgets.Peek(itemID, budgetDay(time.Now()), s.cfg.Budgets.MaxRunsPerItem, s.cfg.Budgets.MaxRunsPerDay)
	return ok
}

// reserveBudget spends one run against itemID's ceilings, atomically — the
// only place a run is actually counted, called from claim() once a
// transition is certain to launch (busy slots and paused sources are refused
// before this point and spend nothing).
func (s *Server) reserveBudget(itemID string) (ok bool, reason string) {
	ok, reason, err := s.budgets.Reserve(itemID, budgetDay(time.Now()), s.cfg.Budgets.MaxRunsPerItem, s.cfg.Budgets.MaxRunsPerDay)
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
func (s *Server) whyBudgetExhausted(itemID string) string {
	_, reason := s.budgets.Peek(itemID, budgetDay(time.Now()), s.cfg.Budgets.MaxRunsPerItem, s.cfg.Budgets.MaxRunsPerDay)
	switch reason {
	case "item":
		runs, _ := s.budgets.Usage(itemID)
		return fmt.Sprintf("%s has reached its execution budget (%d of %d runs) — an operator override is required to run it again",
			itemID, runs, s.cfg.Budgets.MaxRunsPerItem)
	case "day":
		return fmt.Sprintf("the board's daily execution budget (%d runs) is spent for today (UTC) — an operator override is required to run %s now",
			s.cfg.Budgets.MaxRunsPerDay, itemID)
	default:
		return fmt.Sprintf("%s's execution budget could not be checked", itemID)
	}
}

// overrideBudget records an operator's deliberate decision to let one item
// keep spending past its configured ceilings, persists it, and updates the
// board. by is the identity behind it, the same as a pause's or a
// cancellation's audit trail.
func (s *Server) overrideBudget(itemID, reason, by string) error {
	if err := s.budgets.Override(itemID, store.BudgetOverride{Reason: reason, By: by, At: time.Now()}); err != nil {
		return err
	}
	s.hub.publish(event{Kind: "state"})
	s.wakeUp()
	return nil
}

// restoreBudget lifts an override and reimposes the item's configured
// ceilings. Idempotent: restoring an item with no override is not an error.
func (s *Server) restoreBudget(itemID string) error {
	if err := s.budgets.Restore(itemID); err != nil {
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
func (s *Server) budgetViews(items []string) map[string]BudgetView {
	out := map[string]BudgetView{}
	for _, id := range items {
		runs, ov := s.budgets.Usage(id)
		if runs == 0 && ov == nil {
			continue
		}
		v := BudgetView{Runs: runs}
		if ov != nil {
			v.Override = &BudgetOverrideView{By: ov.By, Reason: ov.Reason, At: ov.At}
		}
		out[id] = v
	}
	return out
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
	if err := s.overrideBudget(id, reason, requestedBy(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBudgetRestore lifts an override placed by handleBudgetOverride.
// Idempotent: restoring an item with no override is not an error, the same
// as resuming a source that was never paused.
func (s *Server) handleBudgetRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.restoreBudget(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

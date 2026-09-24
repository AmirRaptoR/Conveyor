// The plan/todo channel: turning runner.PlanUpdate (live, per run) into the
// board's own per-item summary (State.Plans), the SSE plan event, and the
// recovery read of a historical run's plan.jsonl — CONTRACTS §2a/§6.
package server

import (
	"path/filepath"

	"github.com/AmirRaptoR/Conveyor/internal/plan"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// handlePlanUpdate is Runner.OnPlan: called for every accepted revision and
// every rejection, live, while a run is in flight, and once more after it
// exits. It updates the item's card summary only for a stage run naming a
// real target stage — a list, move, doctor or status run publishing a
// revision is recorded in its own run directory (read straight off disk by
// GET /api/runs/{id}) and never reaches a card — and always publishes the SSE
// event, whatever the run's kind, so the panel can follow any run it is
// looking at.
func (s *Server) handlePlanUpdate(runID, itemID string, u runner.PlanUpdate) {
	var rev *plan.Revision
	if u.HasRevision {
		r := u.Revision
		rev = &r
	}
	if u.Kind == "stage" && u.Stage != "" && itemID != "" {
		s.mu.Lock()
		s.planGeneration[itemID]++
		if u.HasRevision {
			s.plans[itemID] = planViewFrom(runID, u.Stage, u)
			delete(s.planMisses, itemID)
		} else if pv, ok := s.plans[itemID]; ok && pv.RunID == runID && pv.Stage == u.Stage {
			// Be defensive about subscribers that report a rejection without
			// repeating the run's accepted revision. The runner normally does
			// repeat it, but rejection diagnostics must never erase good state.
			pv.Rejected = u.Rejected
			s.plans[itemID] = pv
		} else {
			delete(s.plans, itemID)
			s.planMisses[itemID] = planCursor{Stage: u.Stage, RunID: runID}
		}
		s.mu.Unlock()
	}
	rejected := u.Rejected
	accepted := u.Accepted
	s.hub.publish(event{Kind: "plan", RunID: runID, ItemID: itemID, Plan: rev, Accepted: &accepted, Rejected: &rejected})
}

// finishPlanRun attaches an otherwise silent stage run to the dispatch
// tombstone installed by runOne. Without this, an empty plan has no OnPlan
// callback from which retention could learn which negative cache entry to
// evict when that run directory is swept.
func (s *Server) finishPlanRun(res *runner.Result) {
	if res == nil || res.Run.Kind != "stage" || res.Run.To == "" || res.Run.ItemID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if pv, ok := s.plans[res.Run.ItemID]; ok && pv.RunID == res.Run.ID {
		return
	}
	cur, ok := s.planMisses[res.Run.ItemID]
	if !ok || cur.Stage != res.Run.To || cur.RunID != "" {
		return
	}
	cur.RunID = res.Run.ID
	s.planMisses[res.Run.ItemID] = cur
	s.planGeneration[res.Run.ItemID]++
}

// planViewFrom builds the card-facing summary from one live update.
func planViewFrom(runID, stage string, u runner.PlanUpdate) PlanView {
	pv := PlanView{RunID: runID, Stage: stage, Rejected: u.Rejected}
	if u.HasRevision {
		pv.Rev = u.Revision.Rev
		pv.At = u.Revision.At
		pv.Completed, pv.Total, pv.InProgress = u.Revision.Progress()
	}
	return pv
}

// mergeRecoveredPlans applies one unlocked history scan. Both generations are
// captured before that scan: a dispatch/live callback for one item invalidates
// only that item, while a retention deletion invalidates the whole filesystem
// snapshot. Empty results become private cursors rather than visible plans.
func (s *Server) mergeRecoveredPlans(runStoreGen uint64, generations map[string]uint64, stages map[string]string, found map[string]PlanView, foundRuns map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runStoreGen != runStoreGen {
		return
	}
	for id, generation := range generations {
		if s.planGeneration[id] != generation {
			continue
		}
		delete(s.plans, id)
		delete(s.planMisses, id)
		if p, ok := found[id]; ok {
			s.plans[id] = p
		} else {
			s.planMisses[id] = planCursor{Stage: stages[id], RunID: foundRuns[id]}
		}
		s.planGeneration[id]++
	}
}

// loadRunPlan reads one run's plan.jsonl in full — at most plan.MaxRevisions
// accepted lines, the same bound the writer enforces — and returns its
// latest revision plus the accepted/rejected counts. hasRevision is false
// when the file holds no accepted revision at all (missing, empty, or every
// line rejected); a caller wanting to know only whether *anything* was
// published at all (accepted or rejected) should check accepted+rejected>0
// instead — GET /api/runs/{id} does, so a run that only ever produced
// rejections still reports that count.
func loadRunPlan(dir string, final bool) (rev plan.Revision, hasRevision bool, accepted, rejected int) {
	path := filepath.Join(dir, "plan.jsonl")
	r := plan.NewReader()
	if final {
		r.Final(path)
	} else {
		r.Poll(path)
	}
	last, hasLast := r.Last()
	if !hasLast {
		return plan.Revision{}, false, r.Accepted(), r.Rejected()
	}
	return last, true, r.Accepted(), r.Rejected()
}

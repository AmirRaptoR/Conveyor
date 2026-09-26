package server

import (
	"sort"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
)

type admissionFacts struct {
	targetOK       bool
	active         bool
	itemBusy       bool
	resting        bool
	stale          bool
	manualPaused   bool
	agentPaused    bool
	budgetBlocked  bool
	storageBlocked bool
	slotBlocked    bool
	persistFault   bool
}

// admissionReason is the shared pure evaluator for advisory scheduling reads.
// Atomic claim remains the authority, but the scheduler, watchdog and deploy
// gate use these same reasons when deciding what appears runnable beforehand.
func admissionReason(f admissionFacts) string {
	switch {
	case !f.targetOK:
		return "pipeline-target"
	case f.persistFault:
		return "persistence-fault"
	case f.active || f.itemBusy:
		return "item-active"
	case f.resting:
		return "resting"
	case f.stale:
		return "stale-source"
	case f.manualPaused:
		return "manual-pause"
	case f.agentPaused:
		return "agent-pause"
	case f.budgetBlocked:
		return "budget"
	case f.storageBlocked:
		return "storage"
	case f.slotBlocked:
		return "global-capacity"
	default:
		return ""
	}
}

type AdmissionSimulation struct {
	Candidate string
	Target    string
	Reason    string
	Rejected  map[string]int
}

// SimulateAdmission applies the shared evaluator to one immutable API state.
// It claims no slots, reserves no budgets and performs no provider writes.
func SimulateAdmission(cfg *config.Config, state State, now time.Time) (AdmissionSimulation, error) {
	deps := pipeline.NewDeps(cfg, state.Items)
	if len(deps.Errors) > 0 {
		return AdmissionSimulation{}, &admissionError{detail: deps.Errors[0]}
	}
	active := map[string]bool{}
	for _, run := range state.Active {
		active[run.ItemID] = true
	}
	stale := map[string]bool{}
	for _, source := range state.Sources {
		stale[source.Name] = source.Stale || source.ListError != ""
	}
	staleItems := staleItemIDs(state.Items, stale)
	resting := map[string]bool{}
	for _, id := range state.Resting {
		resting[id] = true
	}
	manualGlobal := false
	manualSource := map[string]bool{}
	for _, pause := range state.ManualPauses {
		if pause.Scope == "" {
			manualGlobal = true
		} else {
			manualSource[pause.Scope] = true
		}
	}
	pausedAgent := map[string]bool{}
	for _, pause := range state.Paused {
		pausedAgent[pause.Agent] = pause.Live(now)
	}
	globalFull := state.Slots.GlobalMax > 0 && state.Slots.Global >= state.Slots.GlobalMax
	rejected := map[string]int{}
	var candidates []model.Item
	for _, item := range state.Items {
		target, ok := pipeline.Target(cfg, &item, deps)
		agent := cfg.AgentFor(item.Source, target)
		slotBlocked := globalFull || (state.Slots.PerSource > 0 && state.Slots.BySource[item.Source] >= state.Slots.PerSource) ||
			(state.Slots.PerStage > 0 && state.Slots.ByStage[target] >= state.Slots.PerStage)
		for _, name := range cfg.ResourcesFor(item.Source, target) {
			if use, exists := state.Slots.Resources[name]; exists && use[0] >= use[1] {
				slotBlocked = true
			}
		}
		budgetBlocked := false
		if budget, exists := state.Budgets[item.ID]; exists && cfg.Budgets.MaxRunsPerItem > 0 && agent != "" && budget.Remaining == 0 && (budget.Override == nil || budget.Override.Remaining == 0) {
			budgetBlocked = true
		}
		if state.BudgetDayRemaining != nil && *state.BudgetDayRemaining == 0 && agent != "" {
			budgetBlocked = true
		}
		if failure, held := state.Failures[item.ID]; held && failure.ReleaseCondition != "" {
			budgetBlocked = true
		}
		reason := admissionReason(admissionFacts{targetOK: ok, active: active[item.ID], resting: resting[item.ID],
			stale: dispatchUsesStaleState(item, deps, stale, staleItems), manualPaused: manualGlobal || manualSource[item.Source],
			agentPaused: pausedAgent[agent], budgetBlocked: budgetBlocked,
			storageBlocked: agent != "" && state.Storage.Level != "" && state.Storage.Level != "ok",
			slotBlocked:    slotBlocked, persistFault: state.PersistFault != nil})
		if reason != "" {
			rejected[reason]++
			continue
		}
		candidates = append(candidates, item)
	}
	item, target := pipeline.Pick(cfg, candidates, state.Order, deps)
	if item != nil {
		return AdmissionSimulation{Candidate: item.ID, Target: target, Rejected: rejected}, nil
	}
	reason := "no-candidate"
	if len(rejected) > 0 {
		keys := make([]string, 0, len(rejected))
		for key := range rejected {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		reason = keys[0]
	}
	return AdmissionSimulation{Reason: reason, Rejected: rejected}, nil
}

type admissionError struct{ detail string }

func (e *admissionError) Error() string { return e.detail }

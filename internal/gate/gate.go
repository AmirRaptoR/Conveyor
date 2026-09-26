// Package gate evaluates a post-deploy release without mutating pipeline state.
package gate

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/server"
)

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type Simulation struct {
	Candidate string `json:"candidate,omitempty"`
	Target    string `json:"target,omitempty"`
	Detail    string `json:"detail"`
}

type Result struct {
	Pass       bool       `json:"pass"`
	Revision   string     `json:"revision"`
	Checks     []Check    `json:"checks"`
	Simulation Simulation `json:"simulation"`
}

// Evaluate consumes one server snapshot. It calls the pipeline's own target
// and pick functions but performs no claims, writes, reservations or scripts.
func Evaluate(cfg *config.Config, state server.State, expectedRevision string, uiOK bool, now time.Time) Result {
	result := Result{Pass: true, Revision: state.Release.Revision}
	add := func(name string, ok bool, detail string) {
		status := "pass"
		if !ok {
			status, result.Pass = "fail", false
		}
		result.Checks = append(result.Checks, Check{Name: name, Status: status, Detail: detail})
	}
	identityOK := state.Release.Managed && state.Release.Revision == expectedRevision &&
		filepath.Base(state.Release.Dir) == expectedRevision && state.Release.ConfigSchema == cfg.Version
	add("release", identityOK, fmt.Sprintf("running revision %s from %s", state.Release.Revision, state.Release.Dir))
	add("api", true, "GET /api/state returned valid JSON")
	add("ui", uiOK, "GET / returned the embedded board")

	views := map[string]server.SourceView{}
	for _, source := range state.Sources {
		views[source.Name] = source
	}
	fresh := true
	for _, source := range cfg.Sources {
		view, ok := views[source.Name]
		listedAt, err := time.Parse(time.RFC3339, view.LastListedAt)
		if !ok || err != nil || len(view.Problems) > 0 || view.ListError != "" || now.Sub(listedAt) > cfg.Watchdog.StallWindow.D() {
			fresh = false
		}
	}
	add("sources", fresh, fmt.Sprintf("%d configured source(s) have a fresh successful listing", len(cfg.Sources)))

	deps := pipeline.NewDeps(cfg, state.Items)
	if len(deps.Errors) > 0 {
		add("scheduling", false, deps.Errors[0])
		result.Simulation.Detail = "dependency graph invalid"
		return result
	}
	active := map[string]bool{}
	for _, run := range state.Active {
		active[run.ItemID] = true
	}
	stale := map[string]bool{}
	for _, source := range state.Sources {
		stale[source.Name] = source.Stale || source.ListError != ""
	}
	staleItems := map[string]bool{}
	for _, item := range state.Items {
		if stale[item.Source] {
			staleItems[item.ID] = true
		}
	}
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
		if pause.Live(now) {
			pausedAgent[pause.Agent] = true
		}
	}
	var candidates []model.Item
	for _, item := range state.Items {
		target, ok := pipeline.Target(cfg, &item, deps)
		if !ok || active[item.ID] || resting[item.ID] || stale[item.Source] || deps.DependsOnAny(item.ID, staleItems) || manualGlobal || manualSource[item.Source] || pausedAgent[cfg.AgentFor(item.Source, target)] {
			continue
		}
		if state.Slots.BySource[item.Source] >= state.Slots.PerSource && state.Slots.PerSource > 0 {
			continue
		}
		if state.Slots.ByStage[target] >= state.Slots.PerStage && state.Slots.PerStage > 0 {
			continue
		}
		resourcesBusy := false
		for _, name := range cfg.ResourcesFor(item.Source, target) {
			if use, exists := state.Slots.Resources[name]; exists && use[0] >= use[1] {
				resourcesBusy = true
			}
		}
		if resourcesBusy {
			continue
		}
		if failure, held := state.Failures[item.ID]; held && failure.ReleaseCondition != "" {
			continue
		}
		if budget, exists := state.Budgets[item.ID]; exists && cfg.Budgets.MaxRunsPerItem > 0 && cfg.AgentFor(item.Source, target) != "" && budget.Remaining == 0 && (budget.Override == nil || budget.Override.Remaining == 0) {
			continue
		}
		if state.BudgetDayRemaining != nil && *state.BudgetDayRemaining == 0 && cfg.AgentFor(item.Source, target) != "" {
			continue
		}
		if state.Storage.Level != "" && state.Storage.Level != "ok" && cfg.AgentFor(item.Source, target) != "" {
			continue
		}
		candidates = append(candidates, item)
	}
	item, target := pipeline.Pick(cfg, candidates, state.Order, deps)
	if item == nil {
		result.Simulation.Detail = "no item is currently runnable after server gates"
	} else {
		result.Simulation = Simulation{Candidate: item.ID, Target: target, Detail: "pure pipeline.Target/Pick simulation"}
	}
	add("scheduling", true, result.Simulation.Detail)
	return result
}

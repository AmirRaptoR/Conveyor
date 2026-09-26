// Package gate evaluates a post-deploy release without mutating pipeline state.
package gate

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
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
	freshFor := 2 * cfg.Poll.D()
	if freshFor < time.Minute {
		freshFor = time.Minute
	}
	for _, source := range cfg.Sources {
		view, ok := views[source.Name]
		listedAt, err := time.Parse(time.RFC3339, view.LastListedAt)
		if !ok || err != nil || len(view.Problems) > 0 || view.ListError != "" || now.Sub(listedAt) > freshFor {
			fresh = false
		}
	}
	add("sources", fresh, fmt.Sprintf("%d configured source(s) have a fresh successful listing", len(cfg.Sources)))
	add("persistence", state.PersistFault == nil, "run metadata and logs are persisting without a sticky fault")

	simulation, err := server.SimulateAdmission(cfg, state, now)
	if err != nil {
		add("scheduling", false, err.Error())
		result.Simulation.Detail = "dependency graph invalid"
		return result
	}
	if simulation.Candidate == "" {
		result.Simulation.Detail = "no item is currently runnable after " + simulation.Reason
	} else {
		result.Simulation = Simulation{Candidate: simulation.Candidate, Target: simulation.Target, Detail: "shared read-only admission and pipeline.Pick simulation"}
	}
	add("scheduling", true, result.Simulation.Detail)
	return result
}

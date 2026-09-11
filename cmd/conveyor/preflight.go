package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/preflight"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/source"
)

// cmdPreflight is a readiness check for one source or all of them. It moves
// no item, runs no stage, and writes nothing to any provider or to the
// config — read-only, like cmdList and cmdProbe, so it takes no owner lock
// and settles no interrupted runs.
func cmdPreflight(args []string) error {
	c := newFlags("preflight")
	only := c.fs.String("source", "", "only this source")
	cfg, r, ctx, stop, err := c.load(args)
	if err != nil {
		return err
	}
	defer stop()

	if *only != "" {
		found := false
		for _, s := range cfg.Sources {
			if s.Name == *only {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no source named %q", *only)
		}
	}

	if runPreflight(ctx, cfg, r, *only, os.Stdout) {
		return nil
	}
	return errors.New("one or more checks failed")
}

// runPreflight checks every source named (all of them when only is empty),
// printing the checklist docs/CONTRACTS.md §3 describes, and reports whether
// every check across every source passed.
func runPreflight(ctx context.Context, cfg *config.Config, r *runner.Runner, only string, out io.Writer) bool {
	allOK := true
	for _, s := range cfg.Sources {
		if only != "" && s.Name != only {
			continue
		}
		fmt.Fprintf(out, "%s:\n", s.Name)
		checks := checksForSource(ctx, cfg, r, s)

		preflight.Render(out, checks)
		counts := preflight.Counts(checks)
		fmt.Fprintf(out, "  %d pass, %d warn, %d skip, %d fail, %d unknown\n\n",
			counts[preflight.StatusPass], counts[preflight.StatusWarn], counts[preflight.StatusSkip],
			counts[preflight.StatusFail], counts[preflight.StatusUnknown])

		if preflight.AnyFailed(checks) {
			allOK = false
		}
	}
	return allOK
}

// checksForSource is the one place that assembles a source's readiness
// checks — engine-side, then the provider's own, then one per agent it
// names — shared by conveyor preflight and conveyor run's post-run
// checklist, so the two can never disagree about what "still wrong with
// this source" means.
func checksForSource(ctx context.Context, cfg *config.Config, r *runner.Runner, s config.Source) []preflight.Check {
	var checks []preflight.Check

	// Engine-side: one fail per Source.Problems entry, reusing the
	// resolver rather than re-deriving it.
	for _, p := range s.Problems {
		checks = append(checks, preflight.Check{Name: "config", Status: preflight.StatusFail, Detail: p})
	}

	// Provider: skipped, naming the blocking problem, when the runner
	// could not even start the script (no resolved workdir, no resolved
	// provider) — the engine-side failures above already say what is
	// wrong, and running the script anyway would only say it worse.
	if ok, reason := source.PreflightRunnable(cfg, s); !ok {
		checks = append(checks, preflight.Check{Name: "preflight script", Status: preflight.StatusSkip,
			Detail: "not attempted: " + reason})
	} else {
		client := source.New(cfg, s, r)
		provChecks, _, err := client.Preflight(ctx)
		if err != nil {
			provChecks = []preflight.Check{{Name: "preflight script", Status: preflight.StatusFail, Detail: err.Error()}}
		}
		checks = append(checks, provChecks...)
	}

	// Agents: one check per distinct agent this source's scripts name, in
	// the order the stages that name them appear in the config — the only
	// deterministic order available, since scripts: is a map.
	for _, a := range agentsForSource(cfg, s) {
		if a.Status == "" {
			checks = append(checks, preflight.Check{Name: a.Name, Status: preflight.StatusSkip, Detail: "no status script"})
			continue
		}
		res, runErr := r.Run(ctx, runner.Spec{
			Script: a.Status, Kind: "status", Source: a.Name, Timeout: 30 * time.Second,
		})
		var run model.Run
		var data json.RawMessage
		if res != nil {
			run, data = res.Run, res.Data
		}
		checks = append(checks, preflight.AgentCheck(a.Name, run, data, runErr))
	}
	return checks
}

// agentsForSource lists the distinct agents this source's scripts name, each
// resolved to its own status script when it has one.
func agentsForSource(cfg *config.Config, s config.Source) []config.Agent {
	seen := map[string]bool{}
	var out []config.Agent
	for _, st := range cfg.Stages {
		if st.Script == "" {
			continue
		}
		spec, ok := s.Scripts[st.Script]
		if !ok || spec.Agent == "" || seen[spec.Agent] {
			continue
		}
		seen[spec.Agent] = true
		out = append(out, config.Agent{Name: spec.Agent, Status: cfg.AgentStatusScript(spec.Agent)})
	}
	return out
}

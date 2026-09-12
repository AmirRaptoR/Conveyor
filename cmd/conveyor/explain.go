package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/preflight"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// declineReason composes why pipeline.Target would not move this item,
// naming the item and the config fact that stopped it. Target's own
// signature stays (string, bool); this is run's own job to assemble from
// the same cases Target itself switches on — including the dependency gate,
// so `run -explain` on a held item names the dependency and its stage rather
// than falling through to a generic refusal.
func declineReason(cfg *config.Config, it *model.Item, d pipeline.Deps) string {
	if it.Blocked {
		return fmt.Sprintf("item %s is marked; only a person clears that", it.ID)
	}
	stage, ok := cfg.Stage(it.Stage)
	if !ok {
		return fmt.Sprintf("stage %q is not in this config", it.Stage)
	}
	if stage.Terminal {
		return fmt.Sprintf("stage %q is terminal", it.Stage)
	}
	next := stage.OnSuccess
	if stage.Runs() {
		next = stage.Name
	} else if next == "" {
		return fmt.Sprintf("stage %q is a queue with no onSuccess; items rest there", it.Stage)
	}
	if hold, held := d.Held(it, next); held {
		if hold.Blocked {
			return fmt.Sprintf("held behind %s, which is marked in %s", hold.By, hold.Stage)
		}
		until := hold.Until
		if until == "" {
			until = next
		}
		return fmt.Sprintf("held behind %s, which is still in %s and has not reached %s",
			hold.By, hold.Stage, until)
	}
	return fmt.Sprintf("stage %q has nowhere to go", it.Stage)
}

func findSourceCLI(cfg *config.Config, name string) (config.Source, bool) {
	for _, s := range cfg.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return config.Source{}, false
}

// stageTimeout mirrors pipeline's own unexported timeoutFor: the stage's
// timeout is the default, and a source's own script entry may override it
// for that source only.
func stageTimeout(src config.Source, stage *config.Stage) time.Duration {
	if stage.Run != "" {
		return stage.Timeout.D()
	}
	if t := src.Scripts[stage.Script].Timeout.D(); t > 0 {
		return t
	}
	return stage.Timeout.D()
}

func mergeEnvCLI(env, params map[string]string) map[string]string {
	if len(params) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(params))
	for k, v := range env {
		out[k] = v
	}
	for k, v := range params {
		out[k] = v
	}
	return out
}

// explainRun prints the plan run -explain describes and performs no
// transition: no move run, no stage run.
func explainRun(cfg *config.Config, srcName string, item *model.Item, stageName string, out io.Writer) error {
	st, ok := cfg.Stage(stageName)
	if !ok {
		return fmt.Errorf("no stage named %q", stageName)
	}
	src, _ := findSourceCLI(cfg, srcName)

	fmt.Fprintf(out, "source: %s\n", srcName)
	fmt.Fprintf(out, "item:   %s (ref %s) %q\n", item.ID, item.Ref, item.Title)
	fmt.Fprintf(out, "stage:  %s -> %s\n", item.Stage, stageName)
	fmt.Fprintln(out, "        entering this stage would first call move; -explain does not")

	switch {
	case st.Run != "":
		fmt.Fprintln(out, "script: inline run: body (written to the run directory when actually run)")
	case st.Script == "":
		fmt.Fprintln(out, "script: none — this stage is a queue")
	default:
		if path, ok := src.Paths[st.Script]; ok && path != "" {
			fmt.Fprintf(out, "script: %s\n", path)
		} else {
			fmt.Fprintf(out, "script: %q (unresolved)\n", st.Script)
		}
	}

	resources := cfg.ResourcesFor(srcName, stageName)
	if len(resources) == 0 {
		fmt.Fprintln(out, "resources: none")
	} else {
		fmt.Fprintf(out, "resources: %v\n", resources)
	}

	timeout := stageTimeout(src, st)
	if timeout <= 0 {
		fmt.Fprintln(out, "timeout: none (no deadline)")
	} else {
		fmt.Fprintf(out, "timeout: %s (deadline %s after the script starts)\n", timeout, timeout)
	}

	env := mergeEnvCLI(src.Env, src.Scripts[st.Script].Params)
	fmt.Fprintln(out, "env:")
	redacted := runner.RedactEnv(env)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(out, "  %s=%s\n", k, redacted[k])
	}
	return nil
}

// readRunMeta reads back a finished run's own meta.json, for the exit code
// and timed-out flag the run -stage checklist reports.
func readRunMeta(dir string) (model.Run, bool) {
	if dir == "" {
		return model.Run{}, false
	}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return model.Run{}, false
	}
	var run model.Run
	if json.Unmarshal(b, &run) != nil {
		return model.Run{}, false
	}
	return run, true
}

// readResultReasonSummary reads a run's own result.json for "reason" and
// "summary", when it has them. An absent, unreadable or invalid file is "no
// data" — the script contract's own promise — not an error, so both return
// values are simply empty.
func readResultReasonSummary(dir string) (reason, summary string) {
	if dir == "" {
		return "", ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil || len(b) == 0 {
		return "", ""
	}
	var v struct {
		Reason  string `json:"reason"`
		Summary string `json:"summary"`
	}
	if json.Unmarshal(b, &v) != nil {
		return "", ""
	}
	return v.Reason, v.Summary
}

// printChecklist is what run prints after a real transition: the stage
// entered, the outcome and exit code, whether it timed out, the run
// directory, and reason/summary when the run's own result.json has them. On
// failure, blocked or timeout it is followed by the source's remaining fail
// and unknown checks; on success and noop it is not, since a no-op is
// ordinary control flow and neither outcome should pay for a provider round
// trip.
func printChecklist(ctx context.Context, cfg *config.Config, r *runner.Runner, srcName string, tr *pipeline.Transition) {
	if tr == nil {
		return
	}
	fmt.Println("\nchecklist:")
	fmt.Printf("  stage:     %s\n", tr.Stage)
	fmt.Printf("  outcome:   %s\n", tr.Outcome)
	meta, haveMeta := readRunMeta(tr.RunDir)
	if haveMeta {
		fmt.Printf("  exitCode:  %d\n", meta.ExitCode)
		fmt.Printf("  timedOut:  %v\n", meta.TimedOut)
	}
	if tr.RunDir != "" {
		fmt.Printf("  run:       %s\n", tr.RunDir)
	} else {
		fmt.Println("  run:       (none — this stage runs no script)")
	}
	reason, summary := readResultReasonSummary(tr.RunDir)
	if reason != "" {
		fmt.Printf("  reason:    %s\n", reason)
	}
	if summary != "" {
		fmt.Printf("  summary:   %s\n", summary)
	}

	switch tr.Outcome {
	case model.OutcomeFailure, model.OutcomeBlocked, model.OutcomeTimeout:
		src, ok := findSourceCLI(cfg, srcName)
		if !ok {
			return
		}
		checks := checksForSource(ctx, cfg, r, src)
		var remaining []preflight.Check
		for _, c := range checks {
			if c.Status.Failed() {
				remaining = append(remaining, c)
			}
		}
		if len(remaining) > 0 {
			fmt.Println("  remaining setup problems:")
			preflight.Render(os.Stdout, remaining)
		}
	}
}

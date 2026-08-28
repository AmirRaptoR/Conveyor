// Command agent-team drives a configurable pipeline of scripts over items
// pulled from configurable sources. See docs/CONTRACTS.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/AmirRaptoR/agent-team/internal/config"
	"github.com/AmirRaptoR/agent-team/internal/model"
	"github.com/AmirRaptoR/agent-team/internal/runner"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "validate":
		err = cmdValidate(args)
	case "list":
		err = cmdList(args)
	case "run":
		err = cmdRun(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agent-team — run a configurable pipeline over items from configurable sources

  validate  -c <config>                       load and check a config
  list      -c <config> [-source NAME]        run list scripts, print items
  run       -c <config> -source NAME -item ID -stage NAME
                                              run one stage script for one item

Every script follows docs/CONTRACTS.md: logs on stdout/stderr, structured data
to $AGENT_TEAM_RESULT, exit 0 success / 10 no-op / 20 blocked / other failure.
`)
}

func fs(name string) (*flag.FlagSet, *string) {
	f := flag.NewFlagSet(name, flag.ExitOnError)
	cfg := f.String("c", "agent-team.yaml", "path to config")
	return f, cfg
}

func cmdValidate(args []string) error {
	f, cfgPath := fs("validate")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("ok  %s\n", *cfgPath)
	fmt.Printf("    %d stages: ", len(c.Stages))
	for i, s := range c.Stages {
		if i > 0 {
			fmt.Print(" -> ")
		}
		fmt.Print(s.Name)
		if s.OnEnter != "" {
			fmt.Print("*")
		}
	}
	fmt.Printf("\n    %d source(s), concurrency %d global / %d per source\n",
		len(c.Sources), c.Concurrency.Global, c.Concurrency.PerSource)
	fmt.Printf("    poll %s, default timeout %s, log retention %s\n",
		c.Poll.D(), c.Timeout.D(), c.Logs.Retention.D())
	fmt.Println("    (* = stage runs a script on enter)")
	return nil
}

// runsRoot resolves alongside the config. data/ is gitignored: runs are local
// state, and a run directory holds an item snapshot that may be private.
func runsRoot(c *config.Config) string { return filepath.Join(c.Dir, "data", "runs") }

func newRunner(c *config.Config, verbose bool) *runner.Runner {
	r := runner.New(runsRoot(c))
	if verbose {
		// This is what the UI will do over SSE; on the CLI it just prints.
		r.OnLog = func(_ string, l runner.LogLine) {
			fmt.Printf("  %s %s\n", l.At.Format("15:04:05"), l.Text)
		}
	}
	return r
}

func cmdList(args []string) error {
	f, cfgPath := fs("list")
	only := f.String("source", "", "only this source")
	verbose := f.Bool("v", false, "stream script logs")
	if err := f.Parse(args); err != nil {
		return err
	}
	c, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	r := newRunner(c, *verbose)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTAGE\tPRIO\tTITLE")
	total := 0
	for _, src := range c.Sources {
		if *only != "" && src.Name != *only {
			continue
		}
		items, res, err := listSource(ctx, c, r, src)
		if err != nil {
			return err
		}
		if res.Run.Outcome != model.OutcomeSuccess {
			return fmt.Errorf("source %q list exited %d (%s); log: %s",
				src.Name, res.Run.ExitCode, res.Run.Outcome, filepath.Join(res.Run.Dir, "log.txt"))
		}
		for _, it := range items {
			prio := "-"
			if it.Priority != nil {
				prio = fmt.Sprint(*it.Priority)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", it.ID, it.Stage, prio, it.Title)
			total++
		}
	}
	tw.Flush()
	fmt.Printf("\n%d item(s)\n", total)
	return nil
}

func listSource(ctx context.Context, c *config.Config, r *runner.Runner, src config.Source) ([]model.Item, *runner.Result, error) {
	res, err := r.Run(ctx, runner.Spec{
		Script:  c.ResolveScript(src.List),
		Kind:    "list",
		Workdir: c.Workdir(src),
		Env:     src.Env,
		Source:  src.Name,
		Timeout: c.Timeout.D(),
		Stdin:   model.ListInput{Source: src.Name, Stages: c.StageNames()},
	})
	if err != nil {
		return nil, res, err
	}
	if res.Run.Outcome != model.OutcomeSuccess {
		return nil, res, nil
	}
	var items []model.Item
	if len(res.Data) > 0 {
		if err := json.Unmarshal(res.Data, &items); err != nil {
			return nil, res, fmt.Errorf("source %q: result is not a JSON array of items: %w", src.Name, err)
		}
	}
	// Validate here rather than trusting a source: an unknown stage is a
	// configuration bug that would otherwise strand the item invisibly.
	valid := items[:0]
	seen := map[string]bool{}
	for _, it := range items {
		switch {
		case it.ID == "":
			fmt.Fprintf(os.Stderr, "warn: source %q emitted an item with no id; skipped\n", src.Name)
		case seen[it.ID]:
			fmt.Fprintf(os.Stderr, "warn: source %q emitted duplicate id %q; first wins\n", src.Name, it.ID)
		default:
			if _, ok := c.Stage(it.Stage); !ok {
				fmt.Fprintf(os.Stderr, "warn: item %s has unknown stage %q; skipped\n", it.ID, it.Stage)
				continue
			}
			seen[it.ID] = true
			valid = append(valid, it)
		}
	}
	return valid, res, nil
}

func cmdRun(args []string) error {
	f, cfgPath := fs("run")
	srcName := f.String("source", "", "source name (required)")
	itemID := f.String("item", "", "item id (required)")
	stageName := f.String("stage", "", "stage to move the item into (required)")
	quiet := f.Bool("q", false, "do not stream logs")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *srcName == "" || *itemID == "" || *stageName == "" {
		return fmt.Errorf("-source, -item and -stage are all required")
	}
	c, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var src *config.Source
	for i := range c.Sources {
		if c.Sources[i].Name == *srcName {
			src = &c.Sources[i]
		}
	}
	if src == nil {
		return fmt.Errorf("no source named %q", *srcName)
	}
	stage, ok := c.Stage(*stageName)
	if !ok {
		return fmt.Errorf("no stage named %q", *stageName)
	}

	ctx, stop := signalCtx()
	defer stop()
	r := newRunner(c, !*quiet)

	items, _, err := listSource(ctx, c, r, *src)
	if err != nil {
		return err
	}
	var item *model.Item
	for i := range items {
		if items[i].ID == *itemID {
			item = &items[i]
		}
	}
	if item == nil {
		return fmt.Errorf("source %q has no item %q", *srcName, *itemID)
	}
	from := item.Stage

	// CONTRACTS.md §4: the engine writes provider state BEFORE the stage script
	// runs, so a crash mid-stage leaves a truthful record and the item is not
	// handed out twice.
	fmt.Printf("move %s: %s -> %s\n", item.ID, from, stage.Name)
	moveRes, err := r.Run(ctx, runner.Spec{
		Script: c.ResolveScript(src.Move), Kind: "move", Workdir: c.Workdir(*src),
		Env: src.Env, Source: src.Name, Item: item, From: from, To: stage.Name,
		Timeout: c.Timeout.D(),
		Stdin:   model.StageInput{Item: item, Stage: stage.Name, From: from},
	})
	if err != nil {
		return err
	}
	if moveRes.Run.Outcome != model.OutcomeSuccess {
		return fmt.Errorf("move failed (exit %d); log: %s",
			moveRes.Run.ExitCode, filepath.Join(moveRes.Run.Dir, "log.txt"))
	}

	if stage.OnEnter == "" {
		fmt.Printf("stage %q has no onEnter: it is a queue, nothing to run\n", stage.Name)
		return nil
	}

	item.Stage = stage.Name
	fmt.Printf("run  %s (timeout %s)\n", stage.OnEnter, stage.Timeout.D())
	res, err := r.Run(ctx, runner.Spec{
		Script: c.ResolveScript(stage.OnEnter), Kind: "stage", Workdir: c.Workdir(*src),
		Env: src.Env, Source: src.Name, Item: item, From: from, To: stage.Name,
		Timeout: stage.Timeout.D(),
		Stdin:   model.StageInput{Item: item, Stage: stage.Name, From: from},
	})
	if err != nil {
		return err
	}

	next := nextStage(stage, res.Run.Outcome)
	fmt.Printf("\n%s in %s (exit %d)\n", res.Run.Outcome, res.Run.Duration.Round(time.Millisecond), res.Run.ExitCode)
	if len(res.Data) > 0 {
		fmt.Printf("data: %s\n", res.Data)
	}
	fmt.Printf("run:  %s\n", res.Run.Dir)

	if next == "" {
		fmt.Printf("no transition configured for %s; item stays in %q\n", res.Run.Outcome, stage.Name)
		return exitFor(res.Run.Outcome)
	}
	fmt.Printf("move %s: %s -> %s\n", item.ID, stage.Name, next)
	if _, err := r.Run(ctx, runner.Spec{
		Script: c.ResolveScript(src.Move), Kind: "move", Workdir: c.Workdir(*src),
		Env: src.Env, Source: src.Name, Item: item, From: stage.Name, To: next,
		Timeout: c.Timeout.D(),
		Stdin:   model.StageInput{Item: item, Stage: next, From: stage.Name},
	}); err != nil {
		return err
	}
	return exitFor(res.Run.Outcome)
}

// nextStage applies the transition table from CONTRACTS.md §2. A no-op leaves
// the item exactly where it is.
func nextStage(s *config.Stage, o model.Outcome) string {
	switch o {
	case model.OutcomeSuccess:
		return s.OnSuccess
	case model.OutcomeBlocked:
		return s.OnBlocked
	case model.OutcomeFailure, model.OutcomeTimeout:
		return s.OnFailure
	default:
		return ""
	}
}

// exitFor makes the CLI's own exit code mirror the outcome, so a shell caller
// can branch on it the same way the engine does.
func exitFor(o model.Outcome) error {
	if o == model.OutcomeSuccess || o == model.OutcomeNoop {
		return nil
	}
	return fmt.Errorf("stage outcome: %s", o)
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Command conveyor runs a configurable pipeline of scripts over items pulled
// from configurable sources. See docs/CONTRACTS.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/source"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch cmd := os.Args[1]; cmd {
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "tick":
		err = cmdTick(os.Args[2:])
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
	fmt.Fprint(os.Stderr, `conveyor — run a configurable pipeline over items from configurable sources

  validate                              load and check the config
  list      [-source NAME]              run list scripts, print items
  run       -source N -item ID -stage S move one item into a stage and run it
  tick      [-source NAME] [-n N]       one scheduling pass: pick and advance
  
Common flags: -c <config> (default conveyor.yaml), -v stream logs

Scripts follow docs/CONTRACTS.md: logs on stdout/stderr, structured data to
$CONVEYOR_RESULT, exit 0 success / 10 no-op / 20 blocked / other failure.
`)
}

type common struct {
	fs      *flag.FlagSet
	cfgPath *string
	verbose *bool
}

func newFlags(name string) *common {
	f := flag.NewFlagSet(name, flag.ExitOnError)
	return &common{
		fs:      f,
		cfgPath: f.String("c", "conveyor.yaml", "path to config"),
		verbose: f.Bool("v", false, "stream script logs to stdout"),
	}
}

func (c *common) load(args []string) (*config.Config, *runner.Runner, context.Context, context.CancelFunc, error) {
	if err := c.fs.Parse(args); err != nil {
		return nil, nil, nil, nil, err
	}
	cfg, err := config.Load(*c.cfgPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	r := runner.New(filepath.Join(cfg.Dir, "data", "runs"))
	if *c.verbose {
		// The UI will do this over SSE; on the CLI it just prints.
		r.OnLog = func(_ string, l runner.LogLine) {
			fmt.Printf("  │ %s\n", l.Text)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return cfg, r, ctx, stop, nil
}

func cmdValidate(args []string) error {
	c := newFlags("validate")
	cfg, _, _, stop, err := c.load(args)
	if err != nil {
		return err
	}
	defer stop()

	fmt.Printf("ok  %s\n", *c.cfgPath)
	var parts []string
	for _, s := range cfg.Stages {
		n := s.Name
		switch {
		case s.OnEnter != "":
			n += "*"
		case s.Terminal:
			n += "."
		}
		parts = append(parts, n)
	}
	fmt.Printf("    stages: %s\n", strings.Join(parts, " -> "))
	fmt.Printf("    %d source(s), concurrency %d global / %d per source\n",
		len(cfg.Sources), cfg.Concurrency.Global, cfg.Concurrency.PerSource)
	fmt.Printf("    poll %s, default timeout %s, log retention %s\n",
		cfg.Poll.D(), cfg.Timeout.D(), cfg.Logs.Retention.D())
	fmt.Println("    (* runs a script on enter, . terminal)")
	return nil
}

func cmdList(args []string) error {
	c := newFlags("list")
	only := c.fs.String("source", "", "only this source")
	cfg, r, ctx, stop, err := c.load(args)
	if err != nil {
		return err
	}
	defer stop()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTAGE\tPRIO\tTITLE")
	total := 0
	for _, s := range cfg.Sources {
		if *only != "" && s.Name != *only {
			continue
		}
		res, err := source.New(cfg, s, r).List(ctx)
		if err != nil {
			return err
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(os.Stderr, "warn: %s: %s\n", s.Name, w)
		}
		for _, it := range res.Items {
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

func cmdRun(args []string) error {
	c := newFlags("run")
	srcName := c.fs.String("source", "", "source name (required)")
	itemID := c.fs.String("item", "", "item id (required)")
	stageName := c.fs.String("stage", "", "stage to move into (required)")
	cfg, r, ctx, stop, err := c.load(args)
	if err != nil {
		return err
	}
	defer stop()
	if *srcName == "" || *itemID == "" || *stageName == "" {
		return errors.New("-source, -item and -stage are all required")
	}

	eng := pipeline.New(cfg, r)
	client, ok := eng.Client(*srcName)
	if !ok {
		return fmt.Errorf("no source named %q", *srcName)
	}
	res, err := client.List(ctx)
	if err != nil {
		return err
	}
	var item *model.Item
	for i := range res.Items {
		if res.Items[i].ID == *itemID {
			item = &res.Items[i]
		}
	}
	if item == nil {
		return fmt.Errorf("source %q has no item %q", *srcName, *itemID)
	}
	tr, err := eng.Advance(ctx, *srcName, item, *stageName)
	report(tr)
	if err != nil {
		return err
	}
	return outcomeErr(tr.Outcome)
}

func cmdTick(args []string) error {
	c := newFlags("tick")
	only := c.fs.String("source", "", "only this source")
	max := c.fs.Int("n", 1, "how many items to advance before stopping")
	cfg, r, ctx, stop, err := c.load(args)
	if err != nil {
		return err
	}
	defer stop()

	eng := pipeline.New(cfg, r)
	advanced := 0
	for _, s := range cfg.Sources {
		if *only != "" && s.Name != *only {
			continue
		}
		for advanced < *max {
			client, _ := eng.Client(s.Name)
			res, err := client.List(ctx)
			if err != nil {
				return err
			}
			for _, w := range res.Warnings {
				fmt.Fprintf(os.Stderr, "warn: %s: %s\n", s.Name, w)
			}
			// v1: order is not yet persisted, so Pick falls back to priority.
			item, target := pipeline.Pick(cfg, res.Items, nil)
			if item == nil {
				fmt.Printf("%s: nothing to do\n", s.Name)
				break
			}
			fmt.Printf("\n%s: %s (%s -> %s) — %s\n", s.Name, item.ID, item.Stage, target, item.Title)
			tr, err := eng.Advance(ctx, s.Name, item, target)
			report(tr)
			advanced++
			if err != nil {
				if errors.Is(err, pipeline.ErrBusy) {
					break
				}
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	fmt.Printf("\nadvanced %d item(s)\n", advanced)
	return nil
}

func report(tr *pipeline.Transition) {
	if tr == nil {
		return
	}
	fmt.Printf("  %s -> %s: %s", tr.From, tr.Stage, tr.Outcome)
	if tr.Attempts > 0 {
		fmt.Printf(" (attempt %d)", tr.Attempts)
	}
	if tr.Next != "" && tr.Next != tr.Stage {
		fmt.Printf(" -> %s", tr.Next)
	}
	fmt.Println()
	if tr.RunDir != "" {
		fmt.Printf("  run: %s\n", tr.RunDir)
	}
}

// outcomeErr makes the CLI's exit code mirror the outcome, so a shell caller
// can branch on it the same way the engine routes on it.
func outcomeErr(o model.Outcome) error {
	if o == model.OutcomeSuccess || o == model.OutcomeNoop {
		return nil
	}
	return fmt.Errorf("outcome: %s", o)
}

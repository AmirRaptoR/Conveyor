// Command conveyor runs a configurable pipeline of scripts over items pulled
// from configurable sources. See docs/CONTRACTS.md.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/server"
	"github.com/AmirRaptoR/Conveyor/internal/source"
	"github.com/AmirRaptoR/Conveyor/internal/store"
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
	case "serve":
		err = cmdServe(os.Args[2:])
	case "passwd":
		err = cmdPasswd(os.Args[2:])
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
  serve     [-addr :8080] [-watch]      run the pipeline; board, live logs,
                                        run history. -watch observes only
  passwd    <name>                      hash a password for the config's
                                        auth.users block
  
Common flags:
  -c <config>       path to the config (default conveyor.yaml). Stage scripts
                    are found beside it, in conveyor.d/<source>/<stage>.
  -providers <dir>  provider root; defaults to providers/ beside the binary,
                    or the config's providers: key. Layout inside is fixed:
                    <dir>/<provider>/{list,move}
  -v                stream script logs to stdout

Scripts follow docs/CONTRACTS.md: logs on stdout/stderr, structured data to
$CONVEYOR_RESULT, exit 0 success / 10 no-op / 20 blocked / other failure.
`)
}

type common struct {
	fs        *flag.FlagSet
	cfgPath   *string
	providers *string
	verbose   *bool
}

func newFlags(name string) *common {
	f := flag.NewFlagSet(name, flag.ExitOnError)
	return &common{
		fs:      f,
		cfgPath: f.String("c", "conveyor.yaml", "path to config"),
		// The config path is the only thing normally needed; this exists for a
		// binary run away from its providers/ directory.
		providers: f.String("providers", "", "provider root (default: beside the binary, then the config)"),
		verbose:   f.Bool("v", false, "stream script logs to stdout"),
	}
}

func (c *common) load(args []string) (*config.Config, *runner.Runner, context.Context, context.CancelFunc, error) {
	if err := c.fs.Parse(args); err != nil {
		return nil, nil, nil, nil, err
	}
	cfg, err := config.LoadFrom(*c.cfgPath, *c.providers)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	r := runner.New(filepath.Join(cfg.DataDir(), "runs"))
	if *c.verbose {
		// The UI will do this over SSE; on the CLI it just prints.
		r.OnLog = func(_ string, l runner.LogLine) {
			fmt.Printf("  │ %s\n", l.Text)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return cfg, r, ctx, stop, nil
}

// own claims the data directory for a command that is about to run stages, and
// settles what a previous process left mid-flight.
//
// Only the commands that write take it. `validate` and `list` promise to read,
// and a read that rewrites a live engine's history — marking a refine that is
// still running "interrupted" — makes the run record untrustworthy exactly
// where it is meant to be authoritative.
//
// The sweep lives here, behind the claim, for the same reason: a run still
// marked running is abandoned only once nobody else could be running it. settle
// is what asks for it — a watching server holds the claim, because it still
// writes the order, but it advances nothing and so has nothing to settle.
func own(cfg *config.Config, r *runner.Runner, settle bool) (func(), error) {
	lock, err := store.AcquireLock(filepath.Join(cfg.DataDir(), "owner.lock"))
	if err != nil {
		return nil, err
	}
	if settle {
		if n, err := runner.SweepInterrupted(r.Root); err == nil && n > 0 {
			fmt.Fprintf(os.Stderr, "conveyor: marked %d interrupted run(s) from a previous process\n", n)
		}
	}
	return func() { _ = lock.Release() }, nil
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
		case s.Runs():
			n += "*"
		case s.Terminal:
			n += "."
		}
		parts = append(parts, n)
	}
	fmt.Printf("    stages: %s\n", strings.Join(parts, " -> "))
	fmt.Printf("    %d source(s), concurrency %d global / %d per stage / %d per source\n",
		len(cfg.Sources), cfg.Concurrency.Global, cfg.Concurrency.PerStage, cfg.Concurrency.PerSource)
	fmt.Printf("    poll %s, default timeout %s, log retention %s\n",
		cfg.Poll.D(), cfg.Timeout.D(), cfg.Logs.Retention.D())
	// Printed because it is the one setting that acts on its own: a reader
	// should not have to open the config to learn the board can un-mark itself.
	if d := cfg.RetryStalled.D(); d > 0 {
		fmt.Printf("    a fully stalled board clears its marks every %s\n", d)
	}
	fmt.Println("    (* runs a script on enter, . terminal)")

	// A repo that has not been onboarded is reported, not fatal: it must not
	// take the healthy repos down with it.
	bad := 0
	for _, s := range cfg.Sources {
		if s.OK() {
			continue
		}
		bad++
		fmt.Printf("\n    source %q cannot run:\n", s.Name)
		for _, p := range s.Problems {
			fmt.Printf("      - %s\n", p)
		}
	}
	switch {
	case bad == len(cfg.Sources):
		fmt.Printf("\nall %d source(s) unusable — nothing would be worked\n", bad)
	case bad > 0:
		fmt.Printf("\n%d of %d source(s) unusable; the other %d will still be worked\n",
			bad, len(cfg.Sources), len(cfg.Sources)-bad)
	}
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
		if !s.OK() {
			fmt.Fprintf(os.Stderr, "skip: %s: %s\n", s.Name, strings.Join(s.Problems, "; "))
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
	release, err := own(cfg, r, true)
	if err != nil {
		return err
	}
	defer release()

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
	tr, err := eng.Advance(ctx, *srcName, item, *stageName, model.Resume{})
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
	release, err := own(cfg, r, true)
	if err != nil {
		return err
	}
	defer release()

	eng := pipeline.New(cfg, r)
	order := store.OpenOrder(filepath.Join(cfg.DataDir(), "order.json"))
	advanced := 0
	for _, s := range cfg.Sources {
		if *only != "" && s.Name != *only {
			continue
		}
		if !s.OK() {
			fmt.Fprintf(os.Stderr, "skip: %s: %s\n", s.Name, strings.Join(s.Problems, "; "))
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
			// The same order the board writes, so a tick from the terminal and
			// a tick from the button choose the same item.
			item, target := pipeline.Pick(cfg, res.Items, order.IDs())
			if item == nil {
				fmt.Printf("%s: nothing to do\n", s.Name)
				break
			}
			fmt.Printf("\n%s: %s (%s -> %s) — %s\n", s.Name, item.ID, item.Stage, target, item.Title)
			if !eng.Locks().TryAcquire(s.Name, target) {
				break // something else holds this source or stage
			}
			tr, err := eng.Advance(ctx, s.Name, item, target, model.Resume{})
			eng.Locks().Release(s.Name, target)
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

func cmdServe(args []string) error {
	c := newFlags("serve")
	addr := c.fs.String("addr", ":8080", "listen address")
	// A pipeline that needs a button pressed is not a pipeline. Serving runs it;
	// -watch is for looking at a board without touching the repositories.
	watch := c.fs.Bool("watch", false, "observe only: never advance an item")
	cfg, r, ctx, stop, err := c.load(args)
	if err != nil {
		return err
	}
	defer stop()
	release, err := own(cfg, r, !*watch)
	if err != nil {
		return err
	}
	defer release()
	return server.New(cfg, r).Run(ctx, server.Addr(*addr), !*watch)
}

// cmdPasswd mints one line for the config's auth.users block.
//
// It exists so a password never has to be typed into the config in the clear:
// the file is read by whoever can read the directory, and lives in every backup
// of it. The hash carries its own parameters, so raising the cost later does
// not invalidate the lines already written.
func cmdPasswd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	user := fs.Arg(0)
	if user == "" {
		return fmt.Errorf("usage: conveyor passwd <name>")
	}

	pass, err := readSecret("password: ")
	if err != nil {
		return err
	}
	if pass == "" {
		return fmt.Errorf("an empty password is not a password")
	}
	again, err := readSecret("again: ")
	if err != nil {
		return err
	}
	if pass != again {
		return fmt.Errorf("they do not match")
	}

	hash, err := config.Hash(pass)
	if err != nil {
		return err
	}
	fmt.Printf("\nauth:\n  users:\n    %s: %q\n", user, hash)
	return nil
}

// stdin is read through one buffered reader for the whole process. A fresh
// bufio.Reader per prompt reads ahead and throws away what it did not return,
// so the second prompt of a piped `printf 'pw\npw\n' | conveyor passwd amir`
// saw EOF and the two never matched.
var stdin = bufio.NewReader(os.Stdin)

// readSecret reads one line without echoing it, when there is a terminal to
// turn the echo off on. Piped input is read as it comes: a password arriving on
// stdin was never on screen to hide.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	defer fmt.Fprintln(os.Stderr)

	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		stty := func(arg string) {
			c := exec.Command("stty", arg)
			c.Stdin = os.Stdin
			_ = c.Run() // no stty, no echo suppression; the read still works
		}
		stty("-echo")
		defer stty("echo")
	}
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

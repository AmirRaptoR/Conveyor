package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/probe"
)

// cmdProbe is the post-deploy check: request the board through every origin
// the config says it is reachable by, and exit non-zero if any of them is
// not serving. It deliberately does not go through common.load or own() —
// see runProbe.
func cmdProbe(args []string) error {
	return runProbe(args, os.Stderr)
}

// stringSlice collects a repeatable flag into a slice, in the order given.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// runProbe is cmdProbe with its output made a parameter, so a test can
// inspect what a detached, unattended run would have written to its journal.
//
// It never calls own(): that claims the data directory's owner lock, which a
// running engine already holds, and probe is read-only — it parses -c, loads
// the config, and takes nothing. It is a client, not a participant in the
// pipeline.
func runProbe(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("c", "conveyor.yaml", "path to config")
	providers := fs.String("providers", "", "provider root (default: beside the binary, then the config)")
	addr := fs.String("addr", "", "loopback diagnostic address (e.g. 127.0.0.1:8090); probed and printed, never affects the exit status")
	wait := fs.Duration("wait", 120*time.Second, "how long to keep retrying a failing origin before giving up")
	reqTimeout := fs.Duration("request-timeout", 10*time.Second, "timeout for one probe request")
	notify := fs.Bool("notify", true, "on failure, send a Web Push to every subscription already in the data directory")
	var extraOrigins stringSlice
	fs.Var(&extraOrigins, "origin", "an extra origin to probe (repeatable); the probe set is auth.origins plus these")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadFrom(*cfgPath, *providers)
	if err != nil {
		return err
	}

	parsed, err := cfg.Auth.ParsedOrigins()
	if err != nil {
		return err
	}
	urls := make([]string, 0, len(parsed)+len(extraOrigins))
	for _, o := range parsed {
		urls = append(urls, o.Origin)
	}
	urls = append(urls, extraOrigins...)

	// An empty probe set is exactly #54's shape — a missing auth.origins key —
	// and must not read as success just because there was nothing to fail.
	if len(urls) == 0 {
		fmt.Fprintln(out, "conveyor probe: nothing to check — no auth.origins in the config and no -origin given")
		return errors.New("empty probe set")
	}

	p := probe.New()
	p.RequestTimeout = *reqTimeout

	ctx := context.Background()
	if *addr != "" {
		v := p.ProbeOnce(ctx, "http://"+*addr)
		fmt.Fprintf(out, "loopback diagnostic (not counted): %s\n", v)
	}

	verdicts := p.Wait(ctx, urls, *wait)
	var failing []probe.Verdict
	for _, v := range verdicts {
		fmt.Fprintln(out, v)
		if !v.Pass {
			failing = append(failing, v)
		}
	}
	if len(failing) == 0 {
		return nil
	}

	if *notify {
		fmt.Fprintln(out, probe.Notify(ctx, cfg.DataDir(), failing))
	}
	return fmt.Errorf("%d of %d origin(s) unreachable", len(failing), len(urls))
}

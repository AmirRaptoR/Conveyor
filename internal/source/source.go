// Package source invokes a source's list and move scripts and validates what
// comes back. The engine trusts a source to know its provider; it does not
// trust it to be correct, so every item is checked before it enters the system.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/preflight"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

func statDir(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	return fi.IsDir(), nil
}

// Warning is a non-fatal problem with one item. A bad item is skipped and
// reported, never silently dropped: an item that vanishes without explanation
// is the hardest kind of bug to find in a pipeline.
type Warning struct {
	ItemID string `json:"itemId,omitempty"`
	Reason string `json:"reason"`
}

func (w Warning) String() string {
	if w.ItemID == "" {
		return w.Reason
	}
	return w.ItemID + ": " + w.Reason
}

// ListResult is what one poll of a source produced.
type ListResult struct {
	Items    []model.Item
	Warnings []Warning
	Run      model.Run
}

// Client runs one source's scripts.
type Client struct {
	cfg *config.Config
	src config.Source
	run *runner.Runner
}

func New(cfg *config.Config, src config.Source, r *runner.Runner) *Client {
	return &Client{cfg: cfg, src: src, run: r}
}

func (c *Client) Name() string { return c.src.Name }

// List runs the source's list script and returns the items it emitted.
//
// A non-zero exit is reported as an error: unlike a stage script, where failure
// is a routable outcome, a source that cannot be listed means the engine has no
// idea what work exists and must not guess.
func (c *Client) List(ctx context.Context) (*ListResult, error) {
	res, err := c.run.Run(ctx, runner.Spec{
		Script:  c.cfg.ResolveScript(c.src.List),
		Kind:    "list",
		Workdir: c.cfg.Workdir(c.src),
		Env:     c.src.ProviderEnv(),
		Source:  c.src.Name,
		// Discovery, not Timeout: a listing is a handful of API calls, not
		// agent work, and must not be able to hold a slot for the 90-minute
		// default meant for stages. A source that hangs past this is failed
		// for this poll; the server keeps its last-good items, flagged stale.
		Timeout: c.cfg.Discovery.D(),
		Stdin: model.ListInput{
			Source:         c.src.Name,
			Stages:         c.cfg.StageNames(),
			TerminalStages: c.cfg.TerminalStageNames(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("source %q: list: %w", c.src.Name, err)
	}
	out := &ListResult{Run: res.Run}
	if res.Run.Outcome != model.OutcomeSuccess {
		return out, fmt.Errorf("source %q: list exited %d (%s); log: %s/log.txt",
			c.src.Name, res.Run.ExitCode, res.Run.Outcome, res.Run.Dir)
	}

	var raw []model.Item
	if len(res.Data) > 0 {
		if err := json.Unmarshal(res.Data, &raw); err != nil {
			return out, fmt.Errorf("source %q: result is not a JSON array of items: %w", c.src.Name, err)
		}
	}
	out.Items, out.Warnings = c.validate(raw)
	return out, nil
}

// validate enforces CONTRACTS.md §1. The checks exist because each failure is
// one an author of a new source script will actually hit.
func (c *Client) validate(in []model.Item) ([]model.Item, []Warning) {
	var (
		ok    []model.Item
		warns []Warning
		seen  = map[string]bool{}
	)
	for i, it := range in {
		switch {
		case it.ID == "":
			warns = append(warns, Warning{Reason: fmt.Sprintf("items[%d] has no id; skipped", i)})
			continue
		case it.Ref == "":
			warns = append(warns, Warning{Reason: fmt.Sprintf("items[%d] has no ref; skipped", i)})
			continue
		case seen[it.ID]:
			warns = append(warns, Warning{ItemID: it.ID, Reason: "duplicate id in one listing; first wins"})
			continue
		case it.Title == "":
			warns = append(warns, Warning{ItemID: it.ID, Reason: "no title; skipped"})
			continue
		}
		if _, known := c.cfg.Stage(it.Stage); !known {
			warns = append(warns, Warning{ItemID: it.ID,
				Reason: fmt.Sprintf("unknown stage %q; skipped", it.Stage)})
			continue
		}
		// A source that reports someone else's items would corrupt routing, so
		// correct it rather than trusting the field.
		if it.Source != c.src.Name {
			it.Source = c.src.Name
		}
		seen[it.ID] = true
		ok = append(ok, it)
	}
	return ok, warns
}

// Preflight runs this source's provider preflight script, if it has one, and
// returns readiness checks — docs/CONTRACTS.md §3. Unlike List, an absent
// script is not a Go error: it is a single `skip` check, because a provider
// shipping none is not a problem (Source.OK() is unaffected). An ambiguous
// one is a single `fail` check naming both candidates. Only a script that
// exists and ran, however it exited, produces run to report — every other
// case returns a nil *model.Run because there was no run to speak of.
//
// The invocation is bounded by Discovery, not Timeout: these are API reads,
// not agent work, matching List.
func (c *Client) Preflight(ctx context.Context) ([]preflight.Check, *model.Run, error) {
	path, ambiguous, err := c.cfg.PreflightScript(c.src)
	if err != nil {
		if ambiguous != nil {
			names := ""
			for i, n := range ambiguous {
				if i > 0 {
					names += ", "
				}
				names += n
			}
			return []preflight.Check{{
				Name: "preflight script", Status: preflight.StatusFail,
				Detail: fmt.Sprintf("ambiguous preflight script (%s)", names),
			}}, nil, nil
		}
		return []preflight.Check{{
			Name: "preflight script", Status: preflight.StatusFail,
			Detail: fmt.Sprintf("provider %q: %v", c.src.Provider.Name, err),
		}}, nil, nil
	}
	if path == "" {
		return []preflight.Check{{
			Name: "preflight script", Status: preflight.StatusSkip,
			Detail: "provider declares no preflight script",
		}}, nil, nil
	}

	res, runErr := c.run.Run(ctx, runner.Spec{
		Script:  path,
		Kind:    "preflight",
		Workdir: c.cfg.Workdir(c.src),
		Env:     c.src.ProviderEnv(),
		Source:  c.src.Name,
		Timeout: c.cfg.Discovery.D(),
		Stdin: model.ListInput{
			Source:         c.src.Name,
			Stages:         c.cfg.StageNames(),
			TerminalStages: c.cfg.TerminalStageNames(),
		},
	})
	if res == nil {
		return []preflight.Check{{
			Name: "preflight script", Status: preflight.StatusFail,
			Detail: fmt.Sprintf("could not start: %v", runErr),
		}}, nil, nil
	}
	return preflight.FromRun(res.Run, res.Data), &res.Run, nil
}

// PreflightRunnable reports whether this source is in a state where its
// provider preflight script could even be attempted — the runner needs
// Workdir as cmd.Dir, and the provider itself has to have resolved. When it
// is not, conveyor preflight reports a single `skip` naming the blocking
// problem instead of running the script and getting a confusing failure that
// only restates what Source.Problems already says.
func PreflightRunnable(cfg *config.Config, src config.Source) (ok bool, reason string) {
	wd := cfg.Workdir(src)
	if fi, err := statDir(wd); err != nil || !fi {
		return false, fmt.Sprintf("workdir %s: not a directory", wd)
	}
	if src.Provider.Name == "" {
		return false, "no provider configured"
	}
	dir := filepath.Join(cfg.ProvidersDir(), src.Provider.Name)
	if fi, err := statDir(dir); err != nil || !fi {
		return false, fmt.Sprintf("provider %q: %s is not a directory", src.Provider.Name, dir)
	}
	return true, ""
}

// Mark is the blocked flag as the provider should end up writing it. Setting
// and clearing are the same call with a different value, which is why it rides
// on move rather than being a verb of its own: a mark is provider state, and
// move is already how provider state gets written.
type Mark struct {
	Blocked bool
	// Reason is what the script said when it blocked. Best effort: a provider
	// that has somewhere to put it should, one that does not may ignore it.
	Reason string
	// Kind is the same thing in one word, for a label and for a board.
	Kind string
}

// Move writes a stage and a mark back to the provider. CONTRACTS.md §4: the
// engine calls this, never a stage script, so a crashed stage cannot leave
// provider state inconsistent with what the engine believes.
//
// Both facts go every time. A move that wrote only the stage would need a
// second call to clear a mark, and the gap between them is a window in which
// the item is in one state and marked for another.
func (c *Client) Move(ctx context.Context, item *model.Item, to string, mark Mark) (*model.Run, error) {
	from := item.Stage
	res, err := c.run.Run(ctx, runner.Spec{
		Script:  c.cfg.ResolveScript(c.src.Move),
		Kind:    "move",
		Workdir: c.cfg.Workdir(c.src),
		Env:     c.src.ProviderEnv(),
		Source:  c.src.Name,
		Item:    item,
		From:    from,
		To:      to,
		Timeout: c.cfg.Timeout.D(),
		Stdin: model.StageInput{
			Item: item, Stage: to, From: from,
			Blocked: mark.Blocked, BlockedReason: mark.Reason, BlockedKind: mark.Kind,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("source %q: move %s %s->%s: %w", c.src.Name, item.ID, from, to, err)
	}
	if res.Run.Outcome != model.OutcomeSuccess {
		return &res.Run, fmt.Errorf("source %q: move %s %s->%s exited %d; log: %s/log.txt",
			c.src.Name, item.ID, from, to, res.Run.ExitCode, res.Run.Dir)
	}
	item.Stage = to
	item.Blocked = mark.Blocked
	return &res.Run, nil
}

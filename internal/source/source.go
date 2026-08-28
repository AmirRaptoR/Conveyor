// Package source invokes a source's list and move scripts and validates what
// comes back. The engine trusts a source to know its provider; it does not
// trust it to be correct, so every item is checked before it enters the system.
package source

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

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
		Timeout: c.cfg.Timeout.D(),
		Stdin:   model.ListInput{Source: c.src.Name, Stages: c.cfg.StageNames()},
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

// Move writes a stage change back to the provider. CONTRACTS.md §4: the engine
// calls this, never a stage script, so a crashed stage cannot leave provider
// state inconsistent with what the engine believes.
func (c *Client) Move(ctx context.Context, item *model.Item, to string) (*model.Run, error) {
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
		Stdin:   model.StageInput{Item: item, Stage: to, From: from},
	})
	if err != nil {
		return nil, fmt.Errorf("source %q: move %s %s->%s: %w", c.src.Name, item.ID, from, to, err)
	}
	if res.Run.Outcome != model.OutcomeSuccess {
		return &res.Run, fmt.Errorf("source %q: move %s %s->%s exited %d; log: %s/log.txt",
			c.src.Name, item.ID, from, to, res.Run.ExitCode, res.Run.Dir)
	}
	item.Stage = to
	return &res.Run, nil
}

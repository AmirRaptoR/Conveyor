package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// pausableFor builds a board with two sources: one whose stage runs on an
// agent, and one whose stage runs a plain script. The second is the control —
// a quota belongs to an agent, and a stage that spends nobody's must keep
// moving while one is out.
func pausableFor(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "agents", "fake", "work"), "#!/bin/sh\ncat >/dev/null\n")
	writeScript(t, filepath.Join(dir, "plain.sh"), "#!/bin/sh\ncat >/dev/null\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 100ms
concurrency:
  perSource: 2
  perStage: 2
  global: 4
stages:
  - name: backlog
  - name: working
    script: work
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo
    scripts:
      work:
        agent: fake
  - name: s2
    provider: fake
    workdir: ./repo
    scripts:
      work:
        script: ./plain.sh
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), dir
}

// The whole feature. An agent out of quota used to make no difference to the
// scheduler: it picked the next item, started a run, had it refused in seconds,
// marked the item and picked the next one — converting the board into marked
// items at whatever rate it could start runs. 1164 of 1409 runs recorded on the
// machine this was written for, 82%, ended on a usage-limit refusal.
//
// A stage that runs no agent is the control. The quota belongs to the agent,
// not to the pipeline, and stopping the whole line would be a second bug.
func TestAPausedAgentIsNotDispatchedTo(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{
		{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "runs on the agent"},
		{ID: "s2:1", Ref: "1", Source: "s2", Stage: "backlog", Title: "runs a plain script"},
	}

	if got := cfg.AgentFor("s1", "working"); got != "fake" {
		t.Fatalf("AgentFor(s1, working) = %q, want fake", got)
	}
	if got := cfg.AgentFor("s2", "working"); got != "" {
		t.Fatalf("AgentFor(s2, working) = %q, want no agent", got)
	}

	s.pauseFor("fake", time.Time{}, "out of quota", false)

	n := s.launch(s.ctx)
	waitFor(t, "the launched transition to finish", func() bool { return s.inFlight.Load() == 0 })
	if n != 1 {
		t.Fatalf("launched %d transition(s), want 1: only the agent-backed item is held back", n)
	}
	// And it was the right one.
	if _, marked := s.blocks["s2:1"]; marked {
		t.Error("the plain-script item was marked; it should simply have run")
	}
	s.mu.RLock()
	stage := ""
	for _, it := range s.state.Items {
		if it.ID == "s1:1" {
			stage = it.Stage
		}
	}
	s.mu.RUnlock()
	if stage != "backlog" {
		t.Errorf("the agent-backed item moved to %s; a paused agent must not be dispatched to", stage)
	}

	// Quota back: the item that waited is picked up, unmarked and unchanged.
	s.resumeAgent("fake")
	if n := s.launch(s.ctx); n != 1 {
		t.Fatalf("launched %d after the pause lifted, want 1", n)
	}
	waitFor(t, "the resumed transition to finish", func() bool { return s.inFlight.Load() == 0 })
}

// The first refused run is account-level news, not a fact about one item.
// Waiting for the next status probe to notice costs a whole poll of runs spent
// to be told what this one just said — at four global slots, that was sixteen
// refusals in forty-eight seconds on 2026-08-30.
func TestALimitMarkPausesTheAgentAtOnce(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working"}}

	s.applyTransition(&pipeline.Transition{
		Item:  model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Blocked: true},
		Stage: "working", Blocked: true, Kind: "limit",
		Reason: "the agent was out of quota and stopped before doing the work",
	})

	if !s.agentPaused("fake") {
		t.Fatal("a limit mark did not pause its agent; the next item meets the same wall")
	}
	s.mu.RLock()
	shown := s.state.Paused
	s.mu.RUnlock()
	if len(shown) != 1 || shown[0].Agent != "fake" || !shown[0].FromMark {
		t.Errorf("the board shows %+v; want one pause on fake, marked as coming from the run", shown)
	}

	// A mark for anything else is a fact about that item and nothing more.
	s2 := New(cfg, r)
	s2.ctx = context.Background()
	s2.applyTransition(&pipeline.Transition{
		Item:  model.Item{ID: "s1:2", Ref: "2", Source: "s1", Stage: "working", Blocked: true},
		Stage: "working", Blocked: true, Kind: "decision", Reason: "which database?",
	})
	if s2.agentPaused("fake") {
		t.Error("a decision mark paused the agent; only a limit is about the quota")
	}
}

// The hourly grind this cost most. retryStalled's premise is that the cause of
// a total stall may have passed — but an agent out of quota is a cause with a
// known end, and handing every item back before it spends one refused run per
// item. Nine items, every hour, all night, against a weekly limit thirty hours
// from resetting.
func TestTheStallTimerDoesNotClearMarksIntoAClosedDoor(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{
		{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:2", Ref: "2", Source: "s1", Stage: "working", Blocked: true},
	}
	s.blocks = map[string]Block{
		"s1:1": {Kind: "limit", Reason: "out of quota"},
		"s1:2": {Kind: "limit", Reason: "out of quota"},
	}
	s.pauseFor("fake", time.Now().Add(time.Hour), "out of quota until the reset", false)

	ctx, cancel := context.WithCancel(context.Background())
	go s.stalled(ctx, 20*time.Millisecond)
	time.Sleep(150 * time.Millisecond) // several ticks
	cancel()

	s.mu.RLock()
	held := len(s.blocks)
	s.mu.RUnlock()
	if held != 2 {
		t.Errorf("%d mark(s) left after the stall timer ran; it cleared marks while the agent was paused", held)
	}
}

// A pause has to end on its own. The agent names the reset in its own status
// report, and that is the moment the quota comes back — checked when the
// scheduler looks for work rather than on a timer of its own.
func TestAPauseEndsAtTheResetTheAgentNamed(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()

	s.pauseFor("fake", time.Now().Add(-time.Minute), "a window that has already closed", false)
	if s.agentPaused("fake") {
		t.Error("a pause outlived the reset it named")
	}
	s.mu.RLock()
	shown := len(s.state.Paused)
	s.mu.RUnlock()
	if shown != 0 {
		t.Errorf("the board still shows %d pause(s) after the window closed", shown)
	}

	s.pauseFor("fake", time.Now().Add(time.Hour), "a window still open", false)
	if !s.agentPaused("fake") {
		t.Error("a pause with an open window was lifted early")
	}
}

// Two rules that stop this wedging the line shut, which is the failure mode
// worth fearing: a pause nobody can lift is worse than the bleed it prevents.
func TestNothingWedgesThePauseShut(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()

	// A probe that cannot run says nothing about the agent, so it lifts the
	// pause rather than holding it: letting work through is what happened
	// before any of this existed, and is the safe direction to be wrong in.
	s.pauseFor("fake", time.Time{}, "refused", true)
	s.applyAgentStates([]AgentView{{Name: "fake", State: "unknown", Error: "status exited 1"}}, cfg.AgentsInUse())
	if s.agentPaused("fake") {
		t.Error("a broken status probe kept the line shut")
	}

	// A probe reporting `limited` pauses through the same path, with the
	// reset it named.
	until0 := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	s.applyAgentStates([]AgentView{{Name: "fake", State: "limited",
		Summary: "hit your weekly limit", ResetsAt: until0.Format(time.RFC3339)}}, cfg.AgentsInUse())
	if !s.agentPaused("fake") {
		t.Fatal("a probe reporting `limited` did not pause the agent")
	}
	s.mu.RLock()
	gotUntil := s.paused["fake"].Until
	s.mu.RUnlock()
	if !gotUntil.Equal(until0) {
		t.Errorf("Until = %v, want the reset the agent named (%v)", gotUntil, until0)
	}
	s.resumeAgent("fake")

	// And a mark's pause must not erase a deadline a probe already knew: the
	// mark does not carry one, and taking the earlier answer would turn a
	// bounded pause into an open-ended one.
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	s.pauseFor("fake", until, "out until the reset", false)
	s.pauseFor("fake", time.Time{}, "refused again", true)
	s.mu.RLock()
	got := s.paused["fake"].Until
	s.mu.RUnlock()
	if !got.Equal(until) {
		t.Errorf("Until = %v, want the deadline the probe knew (%v)", got, until)
	}
}

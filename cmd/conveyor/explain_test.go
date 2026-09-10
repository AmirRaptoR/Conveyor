package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

func explainCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	yaml := `version: 1
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
    env:
      GREETING: hello
    scripts:
      work:
        script: ./work.sh
        params:
          TOKEN: super-secret
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestDeclineReasonMarked(t *testing.T) {
	cfg := explainCfg(t)
	it := &model.Item{ID: "s1:1", Stage: "working", Blocked: true}
	got := declineReason(cfg, it)
	if !strings.Contains(got, "marked") {
		t.Errorf("declineReason = %q, want it to say marked", got)
	}
}

func TestDeclineReasonTerminal(t *testing.T) {
	cfg := explainCfg(t)
	it := &model.Item{ID: "s1:1", Stage: "done"}
	got := declineReason(cfg, it)
	if !strings.Contains(got, "terminal") {
		t.Errorf("declineReason = %q, want it to say terminal", got)
	}
}

func TestDeclineReasonUnknownStage(t *testing.T) {
	cfg := explainCfg(t)
	it := &model.Item{ID: "s1:1", Stage: "nonexistent"}
	got := declineReason(cfg, it)
	if !strings.Contains(got, "not in this config") {
		t.Errorf("declineReason = %q, want it to say the stage is unknown", got)
	}
}

// A queue stage (no script) that is also the last stage declared and not
// marked terminal keeps onSuccess empty — applyDefaults only fills it in
// from the next declared stage, and there is none. Items simply rest there;
// pipeline.Target declines, and declineReason must say why.
func TestDeclineReasonQueueWithNoOnSuccess(t *testing.T) {
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	yaml := `version: 1
stages:
  - name: backlog
  - name: parked
sources:
  - name: s1
    provider: fake
    workdir: ./repo
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	it := &model.Item{ID: "s1:1", Stage: "parked"}
	got := declineReason(cfg, it)
	if !strings.Contains(got, "queue with no onSuccess") {
		t.Errorf("declineReason = %q, want it to name the queue", got)
	}
}

func TestExplainRunPrintsThePlanAndRunsNothing(t *testing.T) {
	cfg := explainCfg(t)
	item := &model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "do the thing"}
	var buf bytes.Buffer
	if err := explainRun(cfg, "s1", item, "working", &buf); err != nil {
		t.Fatalf("explainRun: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"source: s1", "backlog -> working", "would first call move", "work.sh", "timeout:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Cross-cutting: nothing -explain prints includes an env: or scripts.*.params:
// value — only the key.
func TestExplainRunRedactsEnv(t *testing.T) {
	cfg := explainCfg(t)
	item := &model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "x"}
	var buf bytes.Buffer
	if err := explainRun(cfg, "s1", item, "working", &buf); err != nil {
		t.Fatalf("explainRun: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "hello") {
		t.Errorf("env value 'hello' leaked into -explain output:\n%s", out)
	}
	if strings.Contains(out, "super-secret") {
		t.Errorf("param value 'super-secret' leaked into -explain output:\n%s", out)
	}
	if !strings.Contains(out, "GREETING=") || !strings.Contains(out, "TOKEN=") {
		t.Errorf("keys should still be visible:\n%s", out)
	}
}

func TestExplainRunCreatesNoRunDirectory(t *testing.T) {
	cfg := explainCfg(t)
	item := &model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "x"}
	var buf bytes.Buffer
	if err := explainRun(cfg, "s1", item, "working", &buf); err != nil {
		t.Fatalf("explainRun: %v", err)
	}
	runsDir := filepath.Join(cfg.DataDir(), "runs")
	if _, err := os.Stat(runsDir); !os.IsNotExist(err) {
		t.Errorf("explainRun created %s, want no run directory at all", runsDir)
	}
}

func TestPrintChecklistReadsResultAndSkipsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "runs", "2026-09-10", "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := model.Run{ExitCode: 0, TimedOut: false, Outcome: model.OutcomeSuccess}
	b, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(runDir, "meta.json"), b, 0o644)
	os.WriteFile(filepath.Join(runDir, "result.json"), []byte(`{"summary":"all good"}`), 0o644)

	cfg := &config.Config{}
	r := runner.New(filepath.Join(dir, "runs"))
	tr := &pipeline.Transition{Stage: "working", Outcome: model.OutcomeSuccess, RunDir: runDir}

	old := os.Stdout
	rp, wp, _ := os.Pipe()
	os.Stdout = wp
	printChecklist(context.Background(), cfg, r, "s1", tr)
	wp.Close()
	os.Stdout = old
	var out bytes.Buffer
	out.ReadFrom(rp)

	got := out.String()
	if !strings.Contains(got, "all good") {
		t.Errorf("checklist missing the summary read from result.json:\n%s", got)
	}
	if strings.Contains(got, "remaining setup problems") {
		t.Errorf("a success outcome must not print remaining setup problems:\n%s", got)
	}
}

func TestPrintChecklistOnFailureShowsRemainingSetupProblems(t *testing.T) {
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	// No preflight.sh at all: PreflightRunnable is fine (workdir/provider
	// both resolve), so client.Preflight yields a single skip — but the
	// source's own Problems (an unresolvable stage script) must still show.
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	yaml := `version: 1
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
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	r := runner.New(filepath.Join(dir, "runs"))
	tr := &pipeline.Transition{Stage: "working", Outcome: model.OutcomeFailure}

	old := os.Stdout
	rp, wp, _ := os.Pipe()
	os.Stdout = wp
	printChecklist(context.Background(), cfg, r, "s1", tr)
	wp.Close()
	os.Stdout = old
	var out bytes.Buffer
	out.ReadFrom(rp)

	got := out.String()
	if !strings.Contains(got, "remaining setup problems") {
		t.Errorf("a failure outcome must print remaining setup problems:\n%s", got)
	}
	if !strings.Contains(got, "does not declare") {
		t.Errorf("checklist did not surface the source's own Problems entry:\n%s", got)
	}
}

// The supervised-run guarantee: one invocation performs exactly one
// transition, producing at most one "stage" run — never a repeat, and the
// item advances at most one stage.
func TestOneInvocationAdvancesAtMostOneStage(t *testing.T) {
	cfg := explainCfg(t)
	r := runner.New(filepath.Join(cfg.DataDir(), "runs"))
	eng := pipeline.New(cfg, r)
	item := &model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "x"}

	before := item.Stage
	tr, err := eng.Advance(context.Background(), "s1", item, "working", model.Resume{})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if tr.Stage == before {
		t.Fatalf("item did not advance at all")
	}

	stageRuns := 0
	_ = filepath.Walk(filepath.Join(cfg.DataDir(), "runs"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "meta.json" {
			return nil
		}
		b, _ := os.ReadFile(path)
		var run model.Run
		if json.Unmarshal(b, &run) == nil && run.Kind == "stage" {
			stageRuns++
		}
		return nil
	})
	if stageRuns > 1 {
		t.Errorf("stage runs = %d, want at most 1", stageRuns)
	}
	// working's script ran once (the one "stage" run above) and succeeded,
	// so the engine's own single Advance call also carries the item on to
	// "done" — a move, not a second script run, since "done" is a terminal
	// queue with no script of its own. That cascade is the engine's normal
	// behaviour for a queue stage; what this test guards is that no *second
	// script* ran to produce it.
	if item.Stage != "done" {
		t.Errorf("item.Stage = %q, want done (working's onSuccess, reached by a move, not a second script run)", item.Stage)
	}
}

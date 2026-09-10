package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	cfg := &config.Config{
		Stages: []config.Stage{{Name: "ready"}},
	}
	_ = cfgPath
	src := config.Source{Name: "s1"}
	return New(cfg, src, runner.New(t.TempDir()))
}

// CONTRACTS.md §1: ref is REQUIRED, exactly like id — a source that cannot
// tell the engine what to pass back to itself has produced an item the
// engine cannot act on, and skipping it silently would be the harder bug to
// find.
func TestValidateRejectsAnEmptyRef(t *testing.T) {
	c := testClient(t)
	ok, warns := c.validate([]model.Item{
		{ID: "s1:1", Ref: "", Stage: "ready", Title: "no ref"},
		{ID: "s1:2", Ref: "2", Stage: "ready", Title: "fine"},
	})
	if len(ok) != 1 || ok[0].ID != "s1:2" {
		t.Fatalf("validated = %v, want only s1:2", ok)
	}
	found := false
	for _, w := range warns {
		if w.Reason == "items[0] has no ref; skipped" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one naming items[0] with no ref", warns)
	}
}

// preflightConfig builds a real, on-disk config with one source "s1" backed
// by a fake provider, so Client.Preflight can resolve providers/<name> and
// Workdir exactly as it does in production.
func preflightConfig(t *testing.T, providerFiles map[string]string) (*config.Config, config.Source) {
	t.Helper()
	dir := t.TempDir()
	writeExecFile(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExecFile(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	for name, body := range providerFiles {
		writeExecFile(t, filepath.Join(dir, "providers", "fake", name), body)
	}
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	yaml := `version: 1
stages:
  - name: backlog
  - name: done
    terminal: true
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
	return cfg, cfg.Sources[0]
}

func writeExecFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func contextBG() context.Context { return context.Background() }

func TestPreflightNoScriptIsSkip(t *testing.T) {
	cfg, src := preflightConfig(t, nil)
	c := New(cfg, src, runner.New(filepath.Join(t.TempDir(), "runs")))
	checks, run, err := c.Preflight(contextBG())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if run != nil {
		t.Errorf("run = %+v, want nil for an absent script", run)
	}
	if len(checks) != 1 || checks[0].Status != "skip" {
		t.Fatalf("checks = %+v, want one skip", checks)
	}
}

func TestPreflightRunsAndParsesChecks(t *testing.T) {
	body := "#!/bin/sh\ncat > \"$CONVEYOR_RESULT\" <<'EOF'\n{\"checks\":[{\"name\":\"gh\",\"status\":\"pass\"},{\"name\":\"labels\",\"status\":\"fail\",\"detail\":\"missing\",\"fix\":\"run onboard\"}]}\nEOF\n"
	cfg, src := preflightConfig(t, map[string]string{"preflight.sh": body})
	c := New(cfg, src, runner.New(filepath.Join(t.TempDir(), "runs")))
	checks, run, err := c.Preflight(contextBG())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if run == nil {
		t.Fatal("run = nil, want the recorded run")
	}
	if run.Kind != "preflight" {
		t.Errorf("run.Kind = %q, want preflight", run.Kind)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %+v, want 2", checks)
	}
	if checks[0].Status != "pass" || checks[1].Status != "fail" || checks[1].Fix != "run onboard" {
		t.Errorf("checks = %+v", checks)
	}
}

func TestPreflightNonZeroExitIsSyntheticFail(t *testing.T) {
	cfg, src := preflightConfig(t, map[string]string{"preflight.sh": "#!/bin/sh\nexit 3\n"})
	c := New(cfg, src, runner.New(filepath.Join(t.TempDir(), "runs")))
	checks, run, err := c.Preflight(contextBG())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if run == nil {
		t.Fatal("run = nil, want the recorded run")
	}
	if len(checks) != 1 || checks[0].Status != "fail" {
		t.Fatalf("checks = %+v, want one synthetic fail", checks)
	}
}

func TestPreflightAmbiguousScriptIsFail(t *testing.T) {
	cfg, src := preflightConfig(t, map[string]string{
		"preflight.sh": "#!/bin/sh\nexit 0\n",
		"preflight.py": "#!/bin/sh\nexit 0\n",
	})
	c := New(cfg, src, runner.New(filepath.Join(t.TempDir(), "runs")))
	checks, run, err := c.Preflight(contextBG())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if run != nil {
		t.Errorf("run = %+v, want nil — the script never ran", run)
	}
	if len(checks) != 1 || checks[0].Status != "fail" {
		t.Fatalf("checks = %+v, want one fail naming the ambiguity", checks)
	}
}

func TestPreflightRunnable(t *testing.T) {
	cfg, src := preflightConfig(t, nil)
	if ok, reason := PreflightRunnable(cfg, src); !ok {
		t.Fatalf("PreflightRunnable = false (%s), want true for a healthy source", reason)
	}
}

func TestPreflightRunnableBadWorkdir(t *testing.T) {
	cfg, src := preflightConfig(t, nil)
	src.Workdir = "./does-not-exist"
	if ok, reason := PreflightRunnable(cfg, src); ok {
		t.Fatal("PreflightRunnable = true for a missing workdir")
	} else if reason == "" {
		t.Error("reason is empty")
	}
}

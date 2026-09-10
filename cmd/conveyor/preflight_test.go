package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// healthyConfig lays down one fully working source "s1": a provider with
// list, move and preflight (which passes one check), a workdir, and an
// agent with a status script reporting "ok".
func healthyConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "preflight.sh"),
		"#!/bin/sh\ncat > \"$CONVEYOR_RESULT\" <<'EOF'\n{\"checks\":[{\"name\":\"ok\",\"status\":\"pass\"}]}\nEOF\n")
	writeExec(t, filepath.Join(dir, "agents", "fakeagent", "work"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "agents", "fakeagent", "status"),
		"#!/bin/sh\ncat > \"$CONVEYOR_RESULT\" <<'EOF'\n{\"state\":\"ok\",\"summary\":\"fine\"}\nEOF\n")
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
    scripts:
      work:
        agent: fakeagent
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

func TestRunPreflightAllGreen(t *testing.T) {
	cfg := healthyConfig(t)
	r := runner.New(filepath.Join(t.TempDir(), "runs"))
	var buf bytes.Buffer
	ok := runPreflight(context.Background(), cfg, r, "", &buf)
	if !ok {
		t.Fatalf("runPreflight = false, want true; output:\n%s", buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "PASS") {
		t.Errorf("output has no PASS line: %s", out)
	}
	if !strings.Contains(out, "0 fail, 0 unknown") {
		t.Errorf("summary line missing 0 fail/0 unknown: %s", out)
	}
}

func TestRunPreflightRecordsAKindPreflightRun(t *testing.T) {
	cfg := healthyConfig(t)
	runsRoot := filepath.Join(t.TempDir(), "runs")
	r := runner.New(runsRoot)
	var buf bytes.Buffer
	runPreflight(context.Background(), cfg, r, "", &buf)

	found := false
	_ = filepath.Walk(runsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "meta.json" {
			return nil
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), `"kind": "preflight"`) {
			found = true
		}
		return nil
	})
	if !found {
		t.Error("no run recorded with kind \"preflight\"")
	}
}

func TestRunPreflightEngineSideProblemIsFail(t *testing.T) {
	dir := t.TempDir()
	// A source naming a provider that does not exist: Source.Problems will
	// carry it, and the provider check must be a skip naming that, not an
	// attempt to run a script that cannot even be resolved.
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	yaml := `version: 1
stages:
  - name: backlog
  - name: done
    terminal: true
sources:
  - name: broken
    provider: nonexistent
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	r := runner.New(filepath.Join(dir, "runs"))
	var buf bytes.Buffer
	ok := runPreflight(context.Background(), cfg, r, "", &buf)
	if ok {
		t.Fatal("runPreflight = true for a source with unresolved provider")
	}
	out := buf.String()
	if !strings.Contains(out, "FAIL") {
		t.Errorf("no FAIL line for a broken source: %s", out)
	}
	if !strings.Contains(out, "not attempted") {
		t.Errorf("provider check did not say it was skipped: %s", out)
	}
}

func TestRunPreflightNoAgentStatusScriptIsSkip(t *testing.T) {
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "agents", "fakeagent", "work"), "#!/bin/sh\nexit 0\n")
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
    scripts:
      work:
        agent: fakeagent
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	r := runner.New(filepath.Join(dir, "runs"))
	var buf bytes.Buffer
	ok := runPreflight(context.Background(), cfg, r, "", &buf)
	if !ok {
		t.Fatalf("runPreflight = false, want true (an agent with no status script is a skip, not a failure); output:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "SKIP") {
		t.Errorf("no SKIP for the agent with no status script: %s", buf.String())
	}
}

func TestRunPreflightOnlyFiltersToOneSource(t *testing.T) {
	cfg := healthyConfig(t)
	// Add a second, broken source that -source should let us ignore.
	cfg.Sources = append(cfg.Sources, config.Source{Name: "s2", Problems: []string{"boom"}})
	r := runner.New(filepath.Join(t.TempDir(), "runs"))
	var buf bytes.Buffer
	ok := runPreflight(context.Background(), cfg, r, "s1", &buf)
	if !ok {
		t.Fatalf("runPreflight(-source s1) = false; output:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "s2:") {
		t.Errorf("output named the filtered-out source: %s", buf.String())
	}
}

package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
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

// F04: a stage script that cannot be launched at all — missing or not
// executable — must count as an attempt exactly as a non-zero exit does, and
// the mark it eventually leaves must name the failure rather than an exit
// code that never happened.
func TestAdvanceCountsALaunchFailureAsAnAttempt(t *testing.T) {
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	scriptPath := filepath.Join(dir, "work.sh")
	writeExec(t, scriptPath, "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
stages:
  - name: backlog
  - name: working
    script: work
    onSuccess: done
    maxAttempts: 2
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo
    scripts:
      work:
        script: ./work.sh
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	e := New(cfg, runner.New(filepath.Join(dir, "runs")))
	item := &model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working"}

	// The script existed at load time (so the source resolved cleanly) and is
	// removed before it runs — the shape of a script deleted or made
	// unreadable between polls.
	if err := os.Remove(scriptPath); err != nil {
		t.Fatal(err)
	}

	tr1, err1 := e.Advance(context.Background(), "s1", item, "working", model.Resume{})
	if tr1 == nil {
		t.Fatalf("Advance returned a nil transition for a launch failure (err=%v)", err1)
	}
	if tr1.Blocked {
		t.Fatalf("attempt 1 of 2 marked the item; maxAttempts is 2")
	}
	if tr1.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 — a launch failure must count", tr1.Attempts)
	}

	tr2, _ := e.Advance(context.Background(), "s1", item, "working", model.Resume{})
	if tr2 == nil || !tr2.Blocked {
		t.Fatalf("attempt 2 of 2 did not mark the item: %+v", tr2)
	}
	if tr2.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", tr2.Attempts)
	}
	if strings.Contains(tr2.Reason, "exit 0") || strings.Contains(tr2.Reason, "exit -1") {
		t.Errorf("reason = %q, want it to name the launch failure, not an exit code", tr2.Reason)
	}
	if !strings.Contains(tr2.Reason, "working") {
		t.Errorf("reason = %q, want it to name the stage", tr2.Reason)
	}
}

// F04: a source that declares no script for the stage it is being advanced
// into is a config problem, but it must still obey maxAttempts and end in a
// mark — not be returned uncounted, which the scheduler would then retry
// forever without ever slowing down.
func TestAdvanceCountsAnUndeclaredScriptAsAnAttempt(t *testing.T) {
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	// s1 declares no scripts at all, so "working" needs a script this source
	// does not have — resolveSources records the Problem, but Advance itself
	// does not consult src.OK(); that is the scheduler's job, and this proves
	// the engine's own retry policy holds even if it is ever reached anyway.
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
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
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	e := New(cfg, runner.New(filepath.Join(dir, "runs")))
	item := &model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working"}

	tr, _ := e.Advance(context.Background(), "s1", item, "working", model.Resume{})
	if tr == nil {
		t.Fatal("Advance returned a nil transition")
	}
	// maxAttempts unset means one: it must mark on this first attempt, not
	// return uncounted for the scheduler to retry unbounded.
	if !tr.Blocked {
		t.Fatalf("an undeclared script was not marked on the first attempt: %+v", tr)
	}
	if tr.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", tr.Attempts)
	}
	if !strings.Contains(tr.Reason, "declares no script") {
		t.Errorf("reason = %q, want it to name the missing script declaration", tr.Reason)
	}
}

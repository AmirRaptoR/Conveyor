package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/server"
)

// -watch selects observe and says so; an explicit, conflicting -mode is a
// startup error naming both flags; an unknown -mode lists the three valid
// ones.
func TestResolveMode(t *testing.T) {
	cases := []struct {
		name             string
		modeStr          string
		modeSet, watch   bool
		wantMode         server.Mode
		wantNoteContains string
		wantErrContains  string
	}{
		{"default auto, no watch", "auto", false, false, server.ModeAuto, "", ""},
		{"explicit manual, no watch", "manual", true, false, server.ModeManual, "", ""},
		{"watch alone selects observe", "auto", false, true, server.ModeObserve, "-watch", ""},
		{"watch with mode=observe agrees", "observe", true, true, server.ModeObserve, "-watch", ""},
		{"watch with mode=manual conflicts", "manual", true, true, "", "", "-watch"},
		{"watch with mode=auto conflicts", "auto", true, true, "", "", "-watch"},
		{"unknown mode", "sideways", true, false, "", "", "auto, manual, observe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, note, err := resolveMode(tc.modeStr, tc.modeSet, tc.watch)
			if tc.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if tc.wantNoteContains != "" && !strings.Contains(note, tc.wantNoteContains) {
				t.Errorf("note = %q, want it to contain %q", note, tc.wantNoteContains)
			}
		})
	}
}

// observe must not settle a run a previous process left "running" — settling
// is itself a write to run history, which observe never does; manual and auto
// both sweep it, as -watch always has.
func TestObserveDoesNotSettleInterruptedRuns(t *testing.T) {
	for _, tc := range []struct {
		mode        server.Mode
		wantSettled bool
	}{
		{server.ModeObserve, false},
		{server.ModeManual, true},
		{server.ModeAuto, true},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			dir := t.TempDir()
			runsRoot := filepath.Join(dir, "runs")
			runDir := filepath.Join(runsRoot, "2026-09-06", "run1")
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			run := model.Run{ID: "run1", Source: "s1", Kind: "stage", StartedAt: time.Now()}
			run.Outcome = model.OutcomeRunning
			b, _ := json.Marshal(run)
			if err := os.WriteFile(filepath.Join(runDir, "meta.json"), b, 0o644); err != nil {
				t.Fatal(err)
			}

			cfg := &config.Config{Dir: dir}
			r := runner.New(runsRoot)
			mode := tc.mode
			release, err := own(cfg, r, mode.Settles())
			if err != nil {
				t.Fatal(err)
			}
			defer release()

			out, err := os.ReadFile(filepath.Join(runDir, "meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			var got model.Run
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			settled := got.Outcome == model.OutcomeInterrupted
			if settled != tc.wantSettled {
				t.Errorf("mode %s: run outcome = %q, settled = %v, want %v",
					tc.mode, got.Outcome, settled, tc.wantSettled)
			}
		})
	}
}

func TestObserveCarryOverWritesNothing(t *testing.T) {
	dir := t.TempDir()
	runsRoot := filepath.Join(dir, "runs")
	runDir := filepath.Join(runsRoot, "2026-09-24", "120000.000-observe")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"120000.000-observe","source":"s1","itemId":"s1:1","kind":"stage","to":"working","outcome":"interrupted"}`
	control := `{"v":1,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"keep me","itemId":"s1:1","runId":"120000.000-observe","session":"","by":"amir"}` + "\n"
	if err := os.WriteFile(filepath.Join(runDir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "control.jsonl"), []byte(control), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "control-ack.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Dir: dir}
	s := server.New(cfg, runner.New(runsRoot))
	if err := carryOverForServe(s, server.ModeObserve); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "answers.json")); !os.IsNotExist(err) {
		t.Fatalf("observe created answers.json: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(runDir, "control.jsonl"))
	if string(got) != control {
		t.Fatal("observe changed control.jsonl")
	}
}

func TestInterruptedSweepUsedByRunAndTickDoesNotCarryAnswers(t *testing.T) {
	dir := t.TempDir()
	runsRoot := filepath.Join(dir, "runs")
	runDir := filepath.Join(runsRoot, "2026-09-24", "120000.000-cli")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := model.Run{ID: "120000.000-cli", Source: "s1", ItemID: "s1:1", Kind: "stage", To: "working", Outcome: model.OutcomeRunning, StartedAt: time.Now()}
	b, _ := json.Marshal(run)
	if err := os.WriteFile(filepath.Join(runDir, "meta.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	control := `{"v":1,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"keep me","itemId":"s1:1","runId":"120000.000-cli","session":"","by":"amir"}` + "\n"
	if err := os.WriteFile(filepath.Join(runDir, "control.jsonl"), []byte(control), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Dir: dir}
	release, err := own(cfg, runner.New(runsRoot), true)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(filepath.Join(dir, "answers.json")); !os.IsNotExist(err) {
		t.Fatalf("shared run/tick sweep created answers.json: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(runDir, "control.jsonl"))
	if string(got) != control {
		t.Fatal("shared run/tick sweep changed control.jsonl")
	}
}

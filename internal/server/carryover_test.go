package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/steering"
)

func queuedCommand(t *testing.T, dir, id, runID, kind, text string) {
	t.Helper()
	path := filepath.Join(dir, "control.jsonl")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := steering.AppendCommand(path, steering.Command{
		ID: id, At: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC),
		Kind: kind, Text: text, ItemID: "s1:1", RunID: runID,
	}); err != nil {
		t.Fatal(err)
	}
	ack := filepath.Join(dir, "control-ack.jsonl")
	if _, err := os.Stat(ack); os.IsNotExist(err) {
		if err := os.WriteFile(ack, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func ackLines(t *testing.T, dir string, lines ...string) {
	t.Helper()
	content := ""
	for _, line := range lines {
		content += line + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "control-ack.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func interruptedRun(t *testing.T, s *Server, runID string) string {
	t.Helper()
	return writeRunMeta(t, s.run, "2026-09-24", runID, "s1:1", "stage", "backlog", "working", "interrupted", "2026-09-24T12:00:00Z")
}

func TestCarryOverOneAndSeveralQueuedInstructions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		texts []string
	}{
		{name: "one", texts: []string{"keep the API small"}},
		{name: "several", texts: []string{"first instruction", "second instruction", "third instruction"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, r := boardFor(t)
			s := New(cfg, r)
			runID := "120000.000-" + tc.name
			dir := interruptedRun(t, s, runID)
			for i, text := range tc.texts {
				queuedCommand(t, dir, string(rune('a'+i)), runID, steering.KindInstruction, text)
			}
			ackLines(t, dir, `{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":["instruction"],"session":"adapter:session-1"}`)

			if err := s.CarryOverInterrupted(); err != nil {
				t.Fatal(err)
			}
			want := "\n\n--- carried from the interrupted run " + runID + " ---\n" + strings.Join(tc.texts, "\n\n")
			if got := s.answers.Get("s1:1"); got.Answer != want || got.Session != "adapter:session-1" || got.Stage != "working" {
				t.Fatalf("answer = %+v, want text %q, opaque session, and working stage", got, want)
			}
			res := steering.Load(dir, false)
			if len(res.Commands) != len(tc.texts) {
				t.Fatalf("commands = %d, want %d", len(res.Commands), len(tc.texts))
			}
			for _, cmd := range res.Commands {
				if cmd.State != steering.StateCarried {
					t.Fatalf("command = %+v, want carried", cmd)
				}
			}
		})
	}
}

func TestCarryOverSecondStartupIsNoOp(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	runID := "120000.000-repeat"
	dir := interruptedRun(t, s, runID)
	queuedCommand(t, dir, "c1", runID, steering.KindInstruction, "do not duplicate me")

	if err := s.CarryOverInterrupted(); err != nil {
		t.Fatal(err)
	}
	wantAnswer := s.answers.Get("s1:1")
	wantControl, err := os.ReadFile(filepath.Join(dir, "control.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CarryOverInterrupted(); err != nil {
		t.Fatal(err)
	}
	gotControl, _ := os.ReadFile(filepath.Join(dir, "control.jsonl"))
	if got := s.answers.Get("s1:1"); got != wantAnswer {
		t.Fatalf("second startup changed answer: got %+v want %+v", got, wantAnswer)
	}
	if string(gotControl) != string(wantControl) {
		t.Fatal("second startup appended another carried record")
	}
}

func TestCarryOverDeduplicatesCrashAfterAnswerWrite(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	runID := "120000.000-crash"
	dir := interruptedRun(t, s, runID)
	queuedCommand(t, dir, "c1", runID, steering.KindInstruction, "survive the crash")
	block := "\n\n--- carried from the interrupted run " + runID + " ---\nsurvive the crash"
	before := model.Resume{Answer: "existing" + block, Session: "adapter:session-1", Stage: "working", Manual: "merge-now"}
	if err := s.answers.Set("s1:1", before); err != nil {
		t.Fatal(err)
	}

	if err := s.CarryOverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if got := s.answers.Get("s1:1"); got != before {
		t.Fatalf("dedup changed armed answer: got %+v want %+v", got, before)
	}
	res := steering.Load(dir, false)
	if got := res.Commands[0]; got.State != steering.StateCarried || !strings.Contains(got.Reason, "already present") {
		t.Fatalf("command after crash recovery = %+v", got)
	}
}

func TestCarryOverLayersAnswerInSequenceAndKeepsExistingSessionOnConflict(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	runID := "120000.000-layer"
	dir := interruptedRun(t, s, runID)
	queuedCommand(t, dir, "c1", runID, steering.KindInstruction, "first")
	queuedCommand(t, dir, "c2", runID, steering.KindInstruction, "second")
	ackLines(t, dir, `{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":["instruction"],"session":"new-session"}`)
	before := model.Resume{Answer: "person's existing answer", Session: "adapter:existing-session", Stage: "working", Manual: "merge-now"}
	if err := s.answers.Set("s1:1", before); err != nil {
		t.Fatal(err)
	}

	if err := s.CarryOverInterrupted(); err != nil {
		t.Fatal(err)
	}
	wantAnswer := before.Answer + "\n\n--- carried from the interrupted run " + runID + " ---\nfirst\n\nsecond"
	got := s.answers.Get("s1:1")
	if got.Answer != wantAnswer || got.Session != before.Session || got.Manual != before.Manual {
		t.Fatalf("layered answer = %+v, want answer %q with existing session/manual", got, wantAnswer)
	}
	for _, cmd := range steering.Load(dir, false).Commands {
		if cmd.State != steering.StateCarried || !strings.Contains(cmd.Reason, "session conflict") {
			t.Fatalf("session conflict not recorded: %+v", cmd)
		}
	}
}

func TestCarryOverDropsQueuedPause(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	runID := "120000.000-pause"
	dir := interruptedRun(t, s, runID)
	queuedCommand(t, dir, "p1", runID, steering.KindPause, "")

	if err := s.CarryOverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if got := s.answers.Get("s1:1"); got != (model.Resume{}) {
		t.Fatalf("pause armed an answer: %+v", got)
	}
	got := steering.Load(dir, false).Commands[0]
	if got.State != steering.StateDropped || got.Reason != "the process it would have stopped is already gone" {
		t.Fatalf("pause = %+v", got)
	}
}

func TestCarryOverDoesNotCrossAnArmedStage(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	runID := "120000.000-stage"
	dir := interruptedRun(t, s, runID)
	queuedCommand(t, dir, "c1", runID, steering.KindInstruction, "belongs to working")
	before := model.Resume{Answer: "newer answer", Session: "adapter:new", Stage: "done"}
	if err := s.answers.Set("s1:1", before); err != nil {
		t.Fatal(err)
	}

	if err := s.CarryOverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if got := s.answers.Get("s1:1"); got != before {
		t.Fatalf("stage-conflicting carry-over changed answer: got %+v want %+v", got, before)
	}
	got := steering.Load(dir, false).Commands[0]
	if got.State != steering.StateDropped || !strings.Contains(got.Reason, "stage done, not working") {
		t.Fatalf("stage-conflicting command = %+v, want dropped with stage reason", got)
	}
}

func TestRunDoesNotDeliverInputArmedForAnotherStage(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	armed := model.Resume{Answer: "do not deliver", Session: "adapter:old", Stage: "backlog"}
	if err := s.answers.Set("s1:1", armed); err != nil {
		t.Fatal(err)
	}

	s.runOne(t.Context(), model.Item{ID: "s1:1", Source: "s1", Stage: "working"}, "working")

	if got := s.answers.Get("s1:1"); got != (model.Resume{}) {
		t.Fatalf("stale input remains armed: %+v", got)
	}
	paths, err := filepath.Glob(filepath.Join(r.Root, "*", "*", "stdin.json"))
	if err != nil {
		t.Fatalf("run input paths = %v, %v", paths, err)
	}
	var stageInput string
	for _, path := range paths {
		metaBytes, readErr := os.ReadFile(filepath.Join(filepath.Dir(path), "meta.json"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var meta model.Run
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Kind == "stage" {
			stageInput = path
			break
		}
	}
	if stageInput == "" {
		t.Fatalf("no stage input among %v", paths)
	}
	b, err := os.ReadFile(stageInput)
	if err != nil {
		t.Fatal(err)
	}
	var input model.StageInput
	if err := json.Unmarshal(b, &input); err != nil {
		t.Fatal(err)
	}
	if input.Answer != "" || input.Session != "" {
		t.Fatalf("stale input reached another stage: %+v", input)
	}
}

func TestCarryOverDoesNotSearchBehindNewestCompletedStageRun(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	oldID := "110000.000-old"
	old := interruptedRun(t, s, oldID)
	queuedCommand(t, old, "c1", oldID, steering.KindInstruction, "too old")
	writeRunMeta(t, r, "2026-09-24", "120000.000-new", "s1:1", "stage", "working", "working", "success", "2026-09-24T12:00:00Z")

	if err := s.CarryOverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if got := s.answers.Get("s1:1"); got != (model.Resume{}) {
		t.Fatalf("older interrupted run carried through completed run: %+v", got)
	}
	if got := steering.Load(old, false).Commands[0].State; got != steering.StateQueued {
		t.Fatalf("older command state = %q, want queued and untouched", got)
	}
}

func TestCarryOverNeverReplaysEarlierRunIntoLaterRun(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	oldID := "110000.000-old"
	old := interruptedRun(t, s, oldID)
	queuedCommand(t, old, "c1", oldID, steering.KindInstruction, "belongs only to old")
	if err := s.CarryOverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if got := steering.Load(old, false).Commands[0].State; got != steering.StateCarried {
		t.Fatalf("old command state = %q, want carried", got)
	}

	newID := "120000.000-new"
	newer := writeRunMeta(t, r, "2026-09-24", newID, "s1:1", "stage", "working", "working", "success", "2026-09-24T12:00:00Z")
	writeSteeringFiles(t, newer, nil, nil)
	if err := s.CarryOverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(newer, "control.jsonl")); err != nil || len(b) != 0 {
		t.Fatalf("later run control channel changed: %q, %v", b, err)
	}
	if got := steering.Load(newer, false).Commands; len(got) != 0 {
		t.Fatalf("earlier command is visible in later run: %+v", got)
	}
}

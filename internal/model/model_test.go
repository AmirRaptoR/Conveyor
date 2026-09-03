package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The doctor's stdin is deliberately its own shape, not StageInput (whose From
// means "the previous stage" and would be a lie here) and not []Run (which
// carries Env and Item — every source parameter, tokens included — and does
// not carry Dir at all, since Run.Dir is json:"-"). See #17.
func TestDoctorInputShape(t *testing.T) {
	it := &Item{ID: "s:1", Source: "s", Ref: "1", Stage: "review"}
	in := DoctorInput{
		Item:    it,
		Stage:   "review",
		Blocked: true,
		Block: DoctorBlock{
			Kind: "turns", Reason: "ran out of turns", Stage: "review",
			RunID: "r1", At: time.Now(), Asked: false,
		},
		Runs: []DoctorRun{{
			ID: "r1", Kind: "stage", Stage: "review",
			Outcome: OutcomeBlocked, ExitCode: 20, TimedOut: false,
			StartedAt: time.Now(), FinishedAt: time.Now(), Dir: "/data/runs/2026-09-03/r1",
		}},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"item", "stage", "blocked", "block", "runs"} {
		if _, ok := got[key]; !ok {
			t.Errorf("stdin is missing %q: %s", key, b)
		}
	}
	if got["blocked"] != true {
		t.Errorf("blocked = %v, want true", got["blocked"])
	}

	block := got["block"].(map[string]any)
	for _, key := range []string{"kind", "reason", "stage", "runId", "at", "asked"} {
		if _, ok := block[key]; !ok {
			t.Errorf("block is missing %q: %s", key, b)
		}
	}
	if _, leaked := block["session"]; leaked {
		t.Error("block carries session, which a doctor script must never read or forward")
	}

	runs := got["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want one entry", runs)
	}
	run := runs[0].(map[string]any)
	for _, key := range []string{"id", "kind", "stage", "outcome", "exitCode", "timedOut", "startedAt", "finishedAt", "dir"} {
		if _, ok := run[key]; !ok {
			t.Errorf("run is missing %q: %s", key, b)
		}
	}
	for _, key := range []string{"env", "item"} {
		if _, leaked := run[key]; leaked {
			t.Errorf("run carries %q, which would hand source parameters to a model", key)
		}
	}

	// Never StageInput's own vocabulary.
	if strings.Contains(string(b), `"from"`) {
		t.Error("DoctorInput must not carry StageInput's From, which means something else entirely")
	}
}

package steering

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEmptyFilesYieldsEmptyResolution(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "control.jsonl"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "control-ack.jsonl"), nil, 0o600)

	res := Load(dir, false)
	if len(res.Commands) != 0 || res.Accepts != nil || res.Session != "" {
		t.Fatalf("expected an empty resolution, got %+v", res)
	}
}

func TestLoadMissingFilesYieldsEmptyResolution(t *testing.T) {
	dir := t.TempDir() // no control files at all — a run from before this shipped
	res := Load(dir, false)
	if len(res.Commands) != 0 {
		t.Fatalf("expected no commands, got %+v", res.Commands)
	}
}

func TestLoadResolvesEndToEndFromDisk(t *testing.T) {
	dir := t.TempDir()
	ctlPath := filepath.Join(dir, "control.jsonl")
	ackPath := filepath.Join(dir, "control-ack.jsonl")
	writeLines(t, ctlPath, cmdLine(1, "c1"), cmdLine(2, "c2"))
	writeLines(t, ackPath,
		helloLine(1, "instruction"),
		ackLine(2, "c1", "consumed"),
	)

	res := Load(dir, true) // run ended
	if len(res.Accepts) != 1 || res.Accepts[0] != KindInstruction {
		t.Fatalf("expected accepts [instruction], got %+v", res.Accepts)
	}
	if len(res.Commands) != 2 {
		t.Fatalf("expected 2 commands, got %+v", res.Commands)
	}
	if res.Commands[0].State != StateConsumed {
		t.Fatalf("expected c1 consumed, got %+v", res.Commands[0])
	}
	if res.Commands[1].State != StateRejected || res.Commands[1].Reason != "run ended" {
		t.Fatalf("expected c2 rejected as run ended (never acked, run finished), got %+v", res.Commands[1])
	}
}

func TestLoadRunNotEndedLeavesUnackedQueued(t *testing.T) {
	dir := t.TempDir()
	ctlPath := filepath.Join(dir, "control.jsonl")
	writeLines(t, ctlPath, cmdLine(1, "c1"))
	os.WriteFile(filepath.Join(dir, "control-ack.jsonl"), nil, 0o600)

	res := Load(dir, false)
	if res.Commands[0].State != StateQueued {
		t.Fatalf("expected c1 still queued while the run is live, got %+v", res.Commands[0])
	}
}

func TestLoadIncludesCarriedRecords(t *testing.T) {
	dir := t.TempDir()
	ctlPath := filepath.Join(dir, "control.jsonl")
	writeLines(t, ctlPath,
		cmdLine(1, "c1"),
		`{"v":1,"type":"carried","seq":2,"at":"2026-09-24T12:00:00Z","entries":[{"id":"c1","state":"carried","reason":"moved on"}]}`,
	)
	os.WriteFile(filepath.Join(dir, "control-ack.jsonl"), nil, 0o600)

	res := Load(dir, true)
	if res.Commands[0].State != StateCarried || res.Commands[0].Reason != "moved on" {
		t.Fatalf("expected c1 carried, got %+v", res.Commands[0])
	}
}

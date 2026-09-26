package main

import (
	"slices"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/server"
)

func TestSoakPassRequiresACompleteSevenDayObservation(t *testing.T) {
	state := server.State{Metrics: server.Metrics{CoverageComplete: false}, Storage: server.StorageView{Level: "ok"}}
	if soakPass(state) {
		t.Fatal("an incomplete observation window passed the seven-day soak")
	}
	state.Metrics.CoverageComplete = true
	state.Metrics.Completions = 1
	state.Metrics.CompletionRate = 1.0 / 7
	if !soakPass(state) {
		t.Fatal("a complete healthy unattended observation did not pass")
	}
}

func TestSoakRequiresAtLeastOneCompletion(t *testing.T) {
	state := server.State{Metrics: server.Metrics{CoverageComplete: true, EvidenceHealthy: true}, Storage: server.StorageView{Level: "ok"}}
	pass, reasons := soakEvaluation(state)
	if pass || !slices.Contains(reasons, "inconclusive: no terminal completions in the seven-day window") {
		t.Fatalf("pass=%t reasons=%#v", pass, reasons)
	}
	state.Metrics.Completions = 1
	state.Metrics.CompletionRate = 1.0 / 7
	pass, reasons = soakEvaluation(state)
	if !pass || len(reasons) != 0 {
		t.Fatalf("pass=%t reasons=%#v", pass, reasons)
	}
}

func TestSoakFailsCompletionRateFinding(t *testing.T) {
	state := server.State{Metrics: server.Metrics{CoverageComplete: true, EvidenceHealthy: true, Completions: 1, CompletionRate: 1.0 / 7}, Storage: server.StorageView{Level: "ok"}, Watchdog: server.WatchdogView{Findings: []server.WatchdogFinding{{Kind: "completion-rate"}}}}
	pass, reasons := soakEvaluation(state)
	if pass || !slices.Contains(reasons, "watchdog finding: completion-rate") {
		t.Fatalf("pass=%t reasons=%#v", pass, reasons)
	}
}

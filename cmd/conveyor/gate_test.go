package main

import (
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/server"
)

func TestSoakPassRequiresACompleteSevenDayObservation(t *testing.T) {
	state := server.State{Metrics: server.Metrics{CoverageComplete: false}, Storage: server.StorageView{Level: "ok"}}
	if soakPass(state) {
		t.Fatal("an incomplete observation window passed the seven-day soak")
	}
	state.Metrics.CoverageComplete = true
	if !soakPass(state) {
		t.Fatal("a complete healthy unattended observation did not pass")
	}
}

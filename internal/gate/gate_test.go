package gate

import (
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/release"
	"github.com/AmirRaptoR/Conveyor/internal/server"
)

func TestEvaluateRequiresFreshEverySourceAndExpectedRevision(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{Version: 1, Poll: config.Duration(time.Minute), Watchdog: config.Watchdog{StallWindow: config.Duration(10 * time.Minute)}, Stages: []config.Stage{{Name: "ready", OnSuccess: "done"}, {Name: "done", Terminal: true}}, Sources: []config.Source{{Name: "one"}, {Name: "two"}}}
	state := server.State{
		Release: release.Info{Managed: true, Revision: "new", Dir: "/opt/conveyor/releases/new", ConfigSchema: 1},
		Sources: []server.SourceView{{Name: "one", LastListedAt: now.Format(time.RFC3339)}, {Name: "two"}},
		Items:   []model.Item{{ID: "one:1", Source: "one", Stage: "ready", Title: "work"}},
	}
	result := Evaluate(cfg, state, "new", true, now)
	if result.Pass || result.Simulation.Candidate != "one:1" || checkStatus(result.Checks, "sources") != "fail" {
		t.Fatalf("result = %#v", result)
	}
	state.Sources[1].LastListedAt = now.Format(time.RFC3339)
	result = Evaluate(cfg, state, "new", true, now)
	if !result.Pass {
		t.Fatalf("fresh result = %#v", result)
	}
	if Evaluate(cfg, state, "old", true, now).Pass {
		t.Fatal("wrong release revision passed")
	}
}

func TestEvaluateFailsOnPersistenceFaultAndHonorsGlobalCapacity(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{Version: 1, Poll: config.Duration(time.Minute), Stages: []config.Stage{{Name: "ready", OnSuccess: "done"}, {Name: "done", Terminal: true}}, Sources: []config.Source{{Name: "one"}}}
	state := server.State{
		Release: release.Info{Managed: true, Revision: "new", Dir: "/opt/conveyor/releases/new", ConfigSchema: 1},
		Sources: []server.SourceView{{Name: "one", LastListedAt: now.Format(time.RFC3339)}},
		Items:   []model.Item{{ID: "one:1", Source: "one", Stage: "ready", Title: "work"}},
		Slots:   server.SlotsView{Global: 1, GlobalMax: 1, BySource: map[string]int{}, ByStage: map[string]int{}},
	}
	result := Evaluate(cfg, state, "new", true, now)
	if !result.Pass || result.Simulation.Candidate != "" || result.Simulation.Detail != "no item is currently runnable after global-capacity" {
		t.Fatalf("capacity result = %#v", result)
	}
	state.PersistFault = &server.PersistFault{RunID: "run-1", Message: "disk full", At: now}
	result = Evaluate(cfg, state, "new", true, now)
	if result.Pass || checkStatus(result.Checks, "persistence") != "fail" {
		t.Fatalf("persistence result = %#v", result)
	}
}

func checkStatus(checks []Check, name string) string {
	for _, check := range checks {
		if check.Name == name {
			return check.Status
		}
	}
	return ""
}

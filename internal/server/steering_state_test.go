package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/registry"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/steering"
)

func writeSteeringFiles(t *testing.T, dir string, control, ack []string) {
	t.Helper()
	for name, lines := range map[string][]string{"control.jsonl": control, "control-ack.jsonl": ack} {
		content := ""
		for _, line := range lines {
			content += line + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func controlLine(seq int, id, runID, session string) string {
	return `{"v":1,"type":"command","seq":` + strconv.Itoa(seq) + `,"id":"` + id + `","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"do it","itemId":"s1:1","runId":"` + runID + `","session":"` + session + `","by":"amir"}`
}

func hello(session string) string {
	return `{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":["instruction","pause"],"session":"` + session + `"}`
}

func TestStateCarriesCompactSteeringWithActiveStagePrecedence(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog"}}
	s.state.Active = []Active{{ItemID: "s1:1", Stage: "working"}}
	s.steering["s1:1"] = SteeringSummary{RunID: "run", Stage: "working", Session: "s", Accepts: []string{"instruction"}, Queued: 1, MaxSeq: 1, Version: 1}

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var state State
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if got, ok := state.Steering["s1:1"]; !ok || got.Stage != "working" || got.Queued != 1 || got.Session != "s" {
		t.Fatalf("steering = %+v, ok=%v", got, ok)
	}
	if _, ok := state.Plans["s1:1"]; ok {
		t.Fatal("unrelated plan state appeared")
	}
}

func TestStateDropsWrongStageSteering(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "done"}}
	s.steering["s1:1"] = SteeringSummary{RunID: "run", Stage: "working"}
	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var state State
	json.Unmarshal(w.Body.Bytes(), &state)
	if _, ok := state.Steering["s1:1"]; ok {
		t.Fatal("wrong-stage steering was exposed")
	}
}

func TestHandleRunCarriesFullSteeringAndRejectsQueuedAtEnd(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	runID := "120000.000-steer"
	dir := writeRunMeta(t, r, "2026-09-24", runID, "s1:1", "stage", "backlog", "working", "success", "2026-09-24T12:01:00Z")
	writeSteeringFiles(t, dir, []string{controlLine(1, "c1", runID, "s1")}, []string{hello("s1")})
	os.WriteFile(filepath.Join(dir, "log.txt"), nil, 0o600)

	req := httptest.NewRequest("GET", "/api/runs/"+runID, nil)
	req.SetPathValue("id", runID)
	w := httptest.NewRecorder()
	s.handleRun(w, req)
	var got RunMeta
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Steering == nil || got.Steering.Session != "s1" || len(got.Steering.Commands) != 1 {
		t.Fatalf("steering = %+v", got.Steering)
	}
	cmd := got.Steering.Commands[0]
	if cmd.State != steering.StateRejected || cmd.Reason != "run ended" || cmd.Text != "do it" || cmd.By != "amir" {
		t.Fatalf("command = %+v", cmd)
	}
}

func TestSteeringUpdatePublishesEveryAckSnapshotAndStoresSession(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.liveRuns.Open("s1:1", registryEntry("run", "working"))
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	res := steering.Resolve(nil, nil, []steering.AckRecord{{Hello: &steering.AckHello{Accepts: []string{"instruction"}, Session: "sess"}}}, false)
	res.Hello, res.AckSeq, res.AckVersion = true, 1, 1
	s.handleSteeringUpdate("run", "s1:1", runner.SteeringUpdate{Resolution: res, Kind: "stage", Stage: "working"})
	select {
	case e := <-ch:
		if e.Kind != "steering" || e.Steering == nil || e.Steering.Session != "sess" || e.Steering.Version != 1 {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	entry, ok := s.liveRuns.Lookup("s1:1")
	if !ok || entry.Session != "sess" {
		t.Fatalf("registry entry = %+v ok=%v", entry, ok)
	}
}

func registryEntry(runID, stage string) registry.Entry {
	return registry.Entry{RunID: runID, Stage: stage}
}

func TestSteeringRecoveryUsesNewestMatchingRunOnly(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	old := writeRunMeta(t, r, "2026-09-24", "110000.000-old", "s1:1", "stage", "backlog", "working", "success", "2026-09-24T11:00:00Z")
	writeSteeringFiles(t, old, nil, []string{hello("old")})
	newer := writeRunMeta(t, r, "2026-09-24", "120000.000-new", "s1:1", "stage", "working", "working", "failure", "2026-09-24T12:00:00Z")
	writeSteeringFiles(t, newer, nil, nil)

	s.recallBlocks([]model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}})
	if _, ok := s.steering["s1:1"]; ok {
		t.Fatal("recovery searched behind newest empty run")
	}
	if got := s.steeringMisses["s1:1"]; got.RunID != "120000.000-new" || got.Stage != "working" {
		t.Fatalf("miss = %+v", got)
	}
}

func TestSteeringRecoveryUsesActiveTargetAndLiveUpdateWins(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	dir := writeRunMeta(t, r, "2026-09-24", "120000.000-live", "s1:1", "stage", "backlog", "working", "running", "2026-09-24T12:00:00Z")
	writeSteeringFiles(t, dir, nil, []string{hello("historical")})
	s.state.Active = []Active{{ItemID: "s1:1", Stage: "working"}}
	items := []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog"}}
	s.recallBlocks(items)
	if got := s.steering["s1:1"]; got.Stage != "working" || got.Session != "historical" {
		t.Fatalf("active-target recovery = %+v", got)
	}

	storeGen := s.runStoreGen
	generation := s.steeringGen["s1:1"]
	live := SteeringSummary{RunID: "new-live", Stage: "working", Session: "new"}
	s.mu.Lock()
	s.steering["s1:1"] = live
	s.steeringGen["s1:1"]++
	s.mu.Unlock()
	s.mergeRecoveredSteering(storeGen, map[string]uint64{"s1:1": generation}, map[string]string{"s1:1": "working"}, map[string]SteeringSummary{"s1:1": {RunID: "old", Stage: "working"}}, map[string]string{"s1:1": "old"})
	if got := s.steering["s1:1"]; got.RunID != "new-live" {
		t.Fatalf("recovery overwrote live update: %+v", got)
	}
}

func TestSteeringLifecycleClearWrongStagePruneAndRetention(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.steering["s1:1"] = SteeringSummary{RunID: "stale", Stage: "working"}
	s.steeringMisses["s1:1"] = planCursor{RunID: "stale", Stage: "working"}
	s.applyTransition(&pipeline.Transition{Item: model.Item{ID: "s1:1", Source: "s1", Stage: "done"}, From: "working", Stage: "working", Outcome: model.OutcomeSuccess})
	if _, ok := s.steering["s1:1"]; ok {
		t.Fatal("wrong-stage positive survived transition")
	}
	if _, ok := s.steeringMisses["s1:1"]; ok {
		t.Fatal("wrong-stage miss survived transition")
	}

	s.steering["gone"] = SteeringSummary{RunID: "gone-run", Stage: "working"}
	s.steeringMisses["gone2"] = planCursor{RunID: "gone-run2", Stage: "working"}
	s.refresh(s.ctx)
	if _, ok := s.steering["gone"]; ok {
		t.Fatal("off-board steering survived refresh")
	}
	if _, ok := s.steeringMisses["gone2"]; ok {
		t.Fatal("off-board steering miss survived refresh")
	}

	old := time.Now().Add(-90 * 24 * time.Hour).UTC()
	runID := "010000.000-evict"
	dir := writeRunMeta(t, r, old.Format("2006-01-02"), runID, "s1:7", "stage", "backlog", "working", "success", old.Format(time.RFC3339))
	writeSteeringFiles(t, dir, nil, []string{hello("old")})
	s.steering["s1:7"] = SteeringSummary{RunID: runID, Stage: "working"}
	s.steeringMisses["s1:8"] = planCursor{RunID: runID, Stage: "working"}
	s.runSweep(os.Stderr)
	if _, ok := s.steering["s1:7"]; ok {
		t.Fatal("retention left positive steering")
	}
	if _, ok := s.steeringMisses["s1:8"]; ok {
		t.Fatal("retention left steering miss")
	}
}

func TestSteeringClearedOnDispatch(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working"}
	s.steering[item.ID] = SteeringSummary{RunID: "old", Stage: "working", Queued: 1}
	s.runOne(t.Context(), item, "working")
	if _, ok := s.steering[item.ID]; ok {
		t.Fatal("old steering survived a new dispatch")
	}
	if miss, ok := s.steeringMisses[item.ID]; ok && miss.RunID == "old" {
		t.Fatalf("old recovery cursor survived: %+v", miss)
	}
}

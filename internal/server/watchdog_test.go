package server

import (
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func TestWatchdogDetectsDeadSchedulerAndDeduplicatesAlert(t *testing.T) {
	cfg, r := boardFor(t)
	cfg.Watchdog.StallWindow = config.Duration(10 * time.Minute)
	s := New(cfg, r)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog", Title: "waiting"}}
	s.listedAt["s1"] = now
	s.watchdogState.LastProgressAt = now.Add(-time.Hour)

	first := s.evaluateWatchdog(now)
	if !first.Alert || !hasFinding(first.Findings, "dead-scheduler") {
		t.Fatalf("first evaluation = %#v, want one dead-scheduler alert", first)
	}
	second := s.evaluateWatchdog(now.Add(time.Minute))
	if second.Alert {
		t.Fatal("unchanged incident alerted twice")
	}
	restarted := New(cfg, r)
	restarted.state.Items = append([]model.Item(nil), s.state.Items...)
	restarted.listedAt["s1"] = now
	if got := restarted.evaluateWatchdog(now.Add(2 * time.Minute)); got.Alert {
		t.Fatal("unchanged incident alerted again after restart")
	}
}

func TestWatchdogReportsStaleSourceStorageAndRevisionCoherence(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog", Title: "waiting"}}
	s.listedAt["s1"] = now.Add(-time.Hour)
	s.state.Storage.Level = "high"
	s.state.Release.Managed = true
	s.state.Release.Revision = "rev-a"
	s.state.Release.Dir = "/opt/conveyor/releases/rev-b"
	view := s.evaluateWatchdog(now)
	for _, kind := range []string{"source-stale", "storage-headroom", "revision-coherence"} {
		if !hasFinding(view.Findings, kind) {
			t.Errorf("findings = %#v, missing %s", view.Findings, kind)
		}
	}
}

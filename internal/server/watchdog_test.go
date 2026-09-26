package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/store"
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
	s.watchdogEndpoints = func() []string { return []string{"device"} }
	s.watchdogNotifyEndpoint = func(string, string, string, string) error { return nil }
	s.deliverWatchdogAlert(first)
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

func TestWatchdogEvidenceLossIsVisibleAfterRestart(t *testing.T) {
	cfg, r := boardFor(t)
	_ = New(cfg, r)
	if err := os.Remove(filepath.Join(cfg.DataDir(), "watchdog.json")); err != nil {
		t.Fatal(err)
	}
	restarted := New(cfg, r)
	view := restarted.evaluateWatchdog(time.Now())
	if view.EvidenceHealthy || view.EvidenceError == "" || !hasFinding(view.Findings, "watchdog-evidence") {
		t.Fatalf("view = %#v", view)
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

func TestWatchdogStalePotentialWorkBecomesAStall(t *testing.T) {
	cfg, r := boardFor(t)
	cfg.Watchdog.StallWindow = config.Duration(10 * time.Minute)
	s := New(cfg, r)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog", Title: "waiting"}}
	s.listedAt["s1"] = now.Add(-time.Hour)
	s.watchdogState.LastProgressAt = now.Add(-time.Hour)
	view := s.evaluateWatchdog(now)
	if view.Potential != 1 || view.Runnable != 0 || !view.Incident || !hasFinding(view.Findings, "stale-source-stall") {
		t.Fatalf("view = %#v, want stale potential work diagnosed as stalled", view)
	}
}

func TestWatchdogDetectsHeldSlotWithoutActiveRun(t *testing.T) {
	cfg, r := boardFor(t)
	cfg.Watchdog.StallWindow = config.Duration(10 * time.Minute)
	s := New(cfg, r)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	item := model.Item{ID: "s1:1", Source: "s1", Stage: "backlog", Title: "waiting"}
	s.state.Items = []model.Item{item}
	s.listedAt["s1"] = now
	s.watchdogState.LastProgressAt = now.Add(-time.Hour)
	target, ok := pipeline.Target(cfg, &item, pipeline.NewDeps(cfg, s.state.Items))
	if !ok || !s.eng.Locks().TryAcquire(item.Source, target, cfg.ResourcesFor(item.Source, target)...) {
		t.Fatal("could not arrange leaked capacity")
	}
	defer s.eng.Locks().Release(item.Source, target, cfg.ResourcesFor(item.Source, target)...)
	view := s.evaluateWatchdog(now)
	if view.Potential != 1 || view.Active != 0 || !view.Incident || !hasFinding(view.Findings, "capacity-leak") {
		t.Fatalf("view = %#v, want held-slot/no-active incident", view)
	}
}

func TestWatchdogAlertRemainsPendingWithoutSubscriberAndRetriesAfterRestart(t *testing.T) {
	cfg, r := boardFor(t)
	cfg.Watchdog.StallWindow = config.Duration(time.Minute)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog", Title: "waiting"}}
	s.listedAt["s1"] = now
	s.watchdogState.LastProgressAt = now.Add(-time.Hour)
	s.watchdogEndpoints = func() []string { return nil }
	first := s.evaluateWatchdog(now)
	if !first.Alert || first.DeliveryStatus != "pending" {
		t.Fatalf("first = %#v", first)
	}
	s.deliverWatchdogAlert(first)
	if state := s.watchdogStore.Get(); state.DeliveryStatus != "pending" || state.DeliveryAttemptedAt.IsZero() || state.DeliveredAt.IsZero() == false {
		t.Fatalf("failed delivery state = %#v", state)
	}

	restarted := New(cfg, r)
	restarted.state.Items = append([]model.Item(nil), s.state.Items...)
	restarted.listedAt["s1"] = now
	calls := 0
	restarted.watchdogEndpoints = func() []string { return []string{"device"} }
	restarted.watchdogNotifyEndpoint = func(string, string, string, string) error { calls++; return nil }
	retry := restarted.evaluateWatchdog(now.Add(time.Minute))
	if !retry.Alert {
		t.Fatalf("restart did not retry pending delivery: %#v", retry)
	}
	restarted.deliverWatchdogAlert(retry)
	settled := restarted.evaluateWatchdog(now.Add(2 * time.Minute))
	restarted.deliverWatchdogAlert(settled)
	if calls != 1 || settled.Alert || settled.DeliveryStatus != "delivered" || settled.DeliveredAt.IsZero() {
		t.Fatalf("settled = %#v calls=%d", settled, calls)
	}
}

func TestOldWatchdogDeliveryCannotAcknowledgeNewIncident(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	now := time.Now().UTC()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog", Title: "waiting"}}
	s.listedAt["s1"] = now
	s.watchdogState.LastProgressAt = now.Add(-time.Hour)
	old := s.evaluateWatchdog(now)
	started, release := make(chan struct{}), make(chan struct{})
	s.watchdogEndpoints = func() []string { return []string{"device"} }
	s.watchdogNotifyEndpoint = func(string, string, string, string) error {
		close(started)
		<-release
		return nil
	}
	done := make(chan struct{})
	go func() { s.deliverWatchdogAlert(old); close(done) }()
	<-started
	s.watchdogMu.Lock()
	next := s.watchdogState
	next.IncidentKey = "new-incident"
	next.DetectedAt = now.Add(time.Minute)
	next.DeliveryStatus = "pending"
	next.DeliveryAcks = nil
	s.watchdogState = next
	if err := s.watchdogStore.Set(next); err != nil {
		t.Fatal(err)
	}
	s.watchdogMu.Unlock()
	close(release)
	<-done
	if state := s.watchdogStore.Get(); state.IncidentKey != "new-incident" || state.DeliveryStatus != "pending" || state.DeliveredAt.IsZero() == false {
		t.Fatalf("new incident was acknowledged by old delivery: %#v", state)
	}
}

func TestWatchdogRetriesOnlyFailedEndpoints(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	now := time.Now().UTC()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "backlog", Title: "waiting"}}
	s.listedAt["s1"] = now
	s.watchdogState.LastProgressAt = now.Add(-time.Hour)
	view := s.evaluateWatchdog(now)
	s.watchdogEndpoints = func() []string { return []string{"good", "flaky"} }
	var mu sync.Mutex
	calls := map[string]int{}
	s.watchdogNotifyEndpoint = func(endpoint, _, _, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		calls[endpoint]++
		if endpoint == "flaky" && calls[endpoint] == 1 {
			return errors.New("temporary failure")
		}
		return nil
	}
	s.deliverWatchdogAlert(view)
	if state := s.watchdogStore.Get(); state.DeliveryStatus != "pending" || !state.DeliveryAcks["good"] || state.DeliveryAcks["flaky"] {
		t.Fatalf("partial state = %#v", state)
	}
	retry := s.evaluateWatchdog(now.Add(time.Minute))
	s.deliverWatchdogAlert(retry)
	state := s.watchdogStore.Get()
	if state.DeliveryStatus != "delivered" || calls["good"] != 1 || calls["flaky"] != 2 {
		t.Fatalf("state = %#v calls=%#v", state, calls)
	}
}

func TestWatchdogCompletionFindingUsesTerminalCompletions(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if err := s.audit.Establish(now.Add(-metricsWindow)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.audit.Append(store.AuditEvent{At: now.Add(-time.Duration(i) * time.Hour), Kind: "run", RunID: fmt.Sprintf("run-%d", i), Outcome: string(model.OutcomeSuccess), Confirmed: true, NextStage: "working"}, now); err != nil {
			t.Fatal(err)
		}
	}
	s.watchdogState.LastProgressAt = now
	view := s.evaluateWatchdog(now)
	if !hasFinding(view.Findings, "completion-rate") {
		t.Fatalf("findings = %#v, successful nonterminal churn hid zero completions", view.Findings)
	}
}

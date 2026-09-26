package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

func TestNetworkRecoveryBackoffIsBoundedStableAndJittered(t *testing.T) {
	now := time.Date(2026, 9, 26, 6, 0, 0, 0, time.UTC)
	entry := store.RecoveryEntry{Scope: "source", ID: "github", Class: recoveryNetwork, Key: "timeout"}
	first := nextRecovery(store.RecoveryEntry{}, entry, now)
	again := nextRecovery(store.RecoveryEntry{}, entry, now)
	if first.NotBefore != again.NotBefore {
		t.Fatalf("same recovery produced unstable jitter: %s != %s", first.NotBefore, again.NotBefore)
	}
	if delay := first.NotBefore.Sub(now); delay < 24*time.Second || delay > 36*time.Second {
		t.Fatalf("first network delay = %s, want 30s +/-20%%", delay)
	}

	cur := first
	for range 20 {
		cur = nextRecovery(cur, entry, now)
	}
	if delay := cur.NotBefore.Sub(now); delay > 36*time.Minute {
		t.Fatalf("capped network delay = %s, want at most 30m +20%% jitter", delay)
	}
}

func TestRecoveryAttemptTracksOnlyUnchangedDiagnosis(t *testing.T) {
	now := time.Now()
	old := store.RecoveryEntry{Scope: "item", ID: "s1:1", Class: recoveryPoll, Key: "draft", Attempt: 4}
	same := nextRecovery(old, old, now)
	if same.Attempt != 5 {
		t.Fatalf("same diagnosis attempt = %d, want 5", same.Attempt)
	}
	changed := old
	changed.Key = "ci-pending"
	changed = nextRecovery(old, changed, now)
	if changed.Attempt != 0 {
		t.Fatalf("changed diagnosis attempt = %d, want reset to 0", changed.Attempt)
	}
}

func TestDeadlineRecoveryUsesTheReportedInstant(t *testing.T) {
	now := time.Now()
	deadline := now.Add(17 * time.Minute)
	for _, class := range []string{recoveryUntil, recoveryQuota} {
		got := nextRecovery(store.RecoveryEntry{}, store.RecoveryEntry{Scope: "item", ID: "s1:1", Class: class, NotBefore: deadline}, now)
		if got.NotBefore != deadline {
			t.Errorf("%s notBefore = %s, want reported %s", class, got.NotBefore, deadline)
		}
	}
}

func TestDeadlineRecoveryWaitsUntilTheReportedInstant(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	now := time.Now()
	for i, class := range []string{recoveryUntil, recoveryQuota} {
		id := "s1:" + string(rune('1'+i))
		if err := s.recovery.Put(store.RecoveryEntry{
			Scope: "item", ID: id, Class: class, Key: class,
			NotBefore: now.Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.recoverDue(t.Context(), now)
	for _, id := range []string{"s1:1", "s1:2"} {
		if _, ok := s.recovery.Get("item", id); !ok {
			t.Fatalf("%s was released before its reported instant", id)
		}
	}
	s.recoverDue(t.Context(), now.Add(2*time.Minute))
	for _, id := range []string{"s1:1", "s1:2"} {
		if _, ok := s.recovery.Get("item", id); ok {
			t.Fatalf("%s was not released at its reported instant", id)
		}
	}
}

func TestRecoveryClassesAreClosed(t *testing.T) {
	for _, class := range []string{
		recoveryNetwork, recoveryPoll, recoveryUntil, recoveryQuota, recoveryWorktree,
	} {
		if !recoveryClass(class) {
			t.Errorf("recoveryClass(%q) = false", class)
		}
	}
	if recoveryClass("decision") {
		t.Error("a human decision became an automatic recovery class")
	}
}

func TestTickExpeditesProbesButPreservesAuthoritativeDeadlines(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	for _, entry := range []store.RecoveryEntry{
		{Scope: "item", ID: "s1:poll", Class: recoveryPoll, Key: "draft:7", NotBefore: deadline},
		{Scope: "item", ID: "s1:quota", Class: recoveryQuota, Key: "agent", NotBefore: deadline},
	} {
		if err := s.recovery.Put(entry); err != nil {
			t.Fatal(err)
		}
		s.resting[entry.ID] = true
		s.restingAt[entry.ID] = entry.NotBefore
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.button(ctx, ModeAuto)
	s.tick <- struct{}{}
	waitFor(t, "the poll recovery to become due", func() bool {
		entry, ok := s.recovery.Get("item", "s1:poll")
		return ok && entry.NotBefore.Before(deadline)
	})

	quota, ok := s.recovery.Get("item", "s1:quota")
	if !ok || !quota.NotBefore.Equal(deadline) {
		t.Fatalf("quota deadline = %v, want %v", quota.NotBefore, deadline)
	}
	s.mu.RLock()
	pollResting, quotaResting := s.resting["s1:poll"], s.resting["s1:quota"]
	s.mu.RUnlock()
	if !pollResting || !quotaResting {
		t.Fatalf("tick cleared typed recovery: poll=%v quota=%v", pollResting, quotaResting)
	}
}

func TestTickCannotOverwriteANewerRecoveryObservation(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	old := store.RecoveryEntry{
		Scope: "item", ID: "s1:1", Class: recoveryPoll, Key: "draft:7",
		NotBefore: time.Now().Add(time.Hour),
	}
	newer := old
	newer.Key = "checks:7"
	newer.NotBefore = time.Now().Add(2 * time.Hour)
	if err := s.recovery.Put(newer); err != nil {
		t.Fatal(err)
	}
	expedited := old
	expedited.NotBefore = time.Now()
	if replaced, err := s.recovery.ReplaceIf(old, expedited); err != nil {
		t.Fatal(err)
	} else if replaced {
		t.Fatal("a stale tick snapshot replaced a newer recovery observation")
	}
	if got, _ := s.recovery.Get("item", old.ID); got != newer {
		t.Fatalf("recovery after stale tick = %+v, want newer %+v", got, newer)
	}
}

func TestRefreshCannotDeleteANewerRecoveryObservation(t *testing.T) {
	cfg, r, _ := pausableFor(t)
	s := New(cfg, r)
	old := store.RecoveryEntry{Scope: "item", ID: "s1:1", Class: recoveryPoll, Key: "draft:7"}
	newer := old
	newer.Key = "checks:7"
	if err := s.recovery.Put(newer); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.recovery.DeleteIf(old); err != nil {
		t.Fatal(err)
	} else if deleted {
		t.Fatal("a stale refresh snapshot deleted a newer recovery observation")
	}
	if got, _ := s.recovery.Get("item", old.ID); got != newer {
		t.Fatalf("recovery after stale refresh = %+v, want newer %+v", got, newer)
	}
}

func TestRecoveryProbeAtomicallyClaimsItemAndResources(t *testing.T) {
	cfg, r, dir := deferringPipeline(t)
	started := filepath.Join(dir, "probe-started")
	release := filepath.Join(dir, "probe-release")
	writeScript(t, filepath.Join(dir, "work.sh"), `#!/bin/sh
touch `+started+`
while [ ! -e `+release+` ]; do sleep 0.01; done
printf '%s\n' '{"waiting":{"class":"poll","key":"draft:7","why":"still draft"}}' > "$CONVEYOR_RESULT"
exit 10
`)

	s := New(cfg, r)
	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working"}
	s.state.Items = []model.Item{item}
	entry := store.RecoveryEntry{
		Scope: "item", ID: item.ID, Source: item.Source, Stage: item.Stage,
		Script: s.targetScriptBinding(item.Source, item.Stage),
		Class:  recoveryPoll, Key: "draft:7",
	}
	if err := s.recovery.Put(entry); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.probeRecovery(t.Context(), entry, time.Now())
	}()
	waitFor(t, "the recovery probe to start", func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	if got := s.claim(item, item.Stage, false); got != claimItemBusy {
		if got == claimAccepted {
			s.eng.Locks().Release(item.Source, item.Stage, s.cfg.ResourcesFor(item.Source, item.Stage)...)
			s.working.Delete(item.ID)
		}
		t.Fatalf("concurrent stage claim = %v, want claimItemBusy", got)
	}
	// A person can replace the wait while its read-only probe is in flight.
	// The stale result must not resurrect the deleted ledger entry.
	if err := s.recovery.Delete("item", item.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery probe did not finish")
	}
	if got, ok := s.recovery.Get("item", item.ID); ok {
		t.Fatalf("stale probe restored deleted recovery: %+v", got)
	}
}

func expireSourceRecovery(t *testing.T, s *Server, source string) {
	t.Helper()
	entry, ok := s.recovery.Get("source", source)
	if !ok {
		t.Fatalf("source %q has no recovery to expire", source)
	}
	entry.NotBefore = time.Now().Add(-time.Second)
	if err := s.recovery.Put(entry); err != nil {
		t.Fatal(err)
	}
}

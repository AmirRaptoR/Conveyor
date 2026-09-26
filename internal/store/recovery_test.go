package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryPersistsAndDeletesByIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	s := OpenRecovery(path)
	next := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	want := RecoveryEntry{Scope: "item", ID: "s1:1", Source: "s1", Stage: "approving", Script: "approve", Class: "network", Key: "pr-7", Attempt: 3, NotBefore: next}
	if err := s.Put(want); err != nil {
		t.Fatal(err)
	}

	reopened := OpenRecovery(path)
	got, ok := reopened.Get("item", "s1:1")
	if !ok || got != want {
		t.Fatalf("reopened recovery = %+v, %v; want %+v", got, ok, want)
	}
	if err := reopened.Delete("item", "s1:1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := OpenRecovery(path).Get("item", "s1:1"); ok {
		t.Fatal("deleted recovery returned after restart")
	}
}

func TestRecoveryScopesDoNotCollide(t *testing.T) {
	s := OpenRecovery(filepath.Join(t.TempDir(), "recovery.json"))
	for _, e := range []RecoveryEntry{
		{Scope: "item", ID: "same", Class: "poll"},
		{Scope: "source", ID: "same", Class: "network"},
	} {
		if err := s.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.All(); len(got) != 2 {
		t.Fatalf("recoveries = %+v, want two independently scoped entries", got)
	}
}

func TestRecoveryConditionalMutationsRejectStaleObservations(t *testing.T) {
	s := OpenRecovery(filepath.Join(t.TempDir(), "recovery.json"))
	old := RecoveryEntry{Scope: "item", ID: "s1:1", Class: "poll", Key: "draft:7", Attempt: 1}
	newer := old
	newer.Key = "checks:7"
	if err := s.Put(newer); err != nil {
		t.Fatal(err)
	}
	if replaced, err := s.ReplaceIf(old, RecoveryEntry{Scope: "item", ID: "s1:1", Class: "network"}); err != nil || replaced {
		t.Fatalf("stale ReplaceIf = %v, %v", replaced, err)
	}
	if deleted, err := s.DeleteIf(old); err != nil || deleted {
		t.Fatalf("stale DeleteIf = %v, %v", deleted, err)
	}
	if got, ok := s.Get("item", "s1:1"); !ok || got != newer {
		t.Fatalf("newer recovery was changed: %+v, present=%v", got, ok)
	}
	if deleted, err := s.DeleteIf(newer); err != nil || !deleted {
		t.Fatalf("current DeleteIf = %v, %v", deleted, err)
	}
}

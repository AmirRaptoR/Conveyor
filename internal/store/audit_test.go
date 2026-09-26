package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditPersistsAndBoundsSevenDayEvents(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a := OpenAudit(path)
	if err := a.Establish(now.Add(-8 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := a.Append(AuditEvent{At: now.Add(-8 * 24 * time.Hour), Kind: "human", Action: "old"}, now); err != nil {
		t.Fatal(err)
	}
	if err := a.Append(AuditEvent{At: now, Kind: "human", Action: "pause"}, now); err != nil {
		t.Fatal(err)
	}
	reopened := OpenAudit(path)
	got := reopened.Since(now.Add(-7 * 24 * time.Hour))
	if len(got) != 1 || got[0].Action != "pause" {
		t.Fatalf("events = %#v, want only seven-day event", got)
	}
	if status := reopened.Status(); !status.Healthy || status.ContinuityID == "" {
		t.Fatalf("status = %#v, want validated continuity", status)
	}
}

func TestAuditFailsClosedAfterEstablishedEvidenceIsDeletedOrCorrupt(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(string) error
	}{
		{"deleted", func(path string) error { return os.Remove(path) }},
		{"malformed", func(path string) error { return os.WriteFile(path, []byte(`{"schema":1`), 0o600) }},
		{"valid truncation", func(path string) error {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(path, b[:len(b)/2], 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.jsonl")
			a := OpenAudit(path)
			if err := a.Establish(now); err != nil {
				t.Fatal(err)
			}
			if err := a.Append(AuditEvent{At: now, Kind: "human", Action: "tick"}, now); err != nil {
				t.Fatal(err)
			}
			if err := tc.mutate(path); err != nil {
				t.Fatal(err)
			}
			if status := a.Status(); status.Healthy || status.Error == "" {
				t.Fatalf("live status = %#v, want durable evidence fault", status)
			}
			if status := OpenAudit(path).Status(); status.Healthy || status.Error == "" {
				t.Fatalf("status = %#v, want durable evidence fault", status)
			}
		})
	}
}

func TestAuditDeduplicatesRunRetriesByRunID(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	a := OpenAudit(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err := a.Establish(now); err != nil {
		t.Fatal(err)
	}
	pending := AuditEvent{At: now, Kind: "run", RunID: "run-1", Outcome: "success"}
	if err := a.Append(pending, now); err != nil {
		t.Fatal(err)
	}
	pending.Confirmed = true
	pending.NextStage = "done"
	if err := a.Append(pending, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := a.Since(now.Add(-time.Hour))
	if len(got) != 1 || !got[0].Confirmed || got[0].NextStage != "done" {
		t.Fatalf("events = %#v, want one canonical confirmed run", got)
	}
}

func TestAuditWriteFailureAndRecordCapAreVisible(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	a := OpenAudit(filepath.Join(dir, "audit.jsonl"))
	if err := a.Establish(now); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := a.Append(AuditEvent{At: now, Kind: "human", Action: "tick"}, now); err == nil {
		t.Fatal("append to removed evidence directory succeeded")
	}
	if status := a.Status(); status.Healthy || status.Error == "" {
		t.Fatalf("status = %#v, want visible write fault", status)
	}

	b := OpenAudit(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err := b.Establish(now); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.events = make([]AuditEvent, maxAuditRecords)
	for i := range b.events {
		b.events[i] = AuditEvent{At: now, Kind: "human", Action: strings.Repeat("x", 8)}
	}
	b.mu.Unlock()
	if err := b.Append(AuditEvent{At: now, Kind: "human", Action: "over-cap"}, now); err == nil {
		t.Fatal("record cap did not fail closed")
	}
}

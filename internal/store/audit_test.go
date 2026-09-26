package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAuditPersistsAndBoundsSevenDayEvents(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a := OpenAudit(path)
	if err := a.Append(AuditEvent{At: now.Add(-8 * 24 * time.Hour), Kind: "human", Action: "old"}, now); err != nil {
		t.Fatal(err)
	}
	if err := a.Append(AuditEvent{At: now, Kind: "human", Action: "pause"}, now); err != nil {
		t.Fatal(err)
	}
	got := OpenAudit(path).Since(now.Add(-7 * 24 * time.Hour))
	if len(got) != 1 || got[0].Action != "pause" {
		t.Fatalf("events = %#v, want only seven-day event", got)
	}
}

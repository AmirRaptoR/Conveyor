package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSoakStartReplacesPriorIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "soak.json")
	s := OpenSoak(path)
	old := SoakRecord{Revision: "same", StartedAt: time.Now().Add(-8 * 24 * time.Hour), EvidenceID: "old"}
	if err := s.Set(old); err != nil {
		t.Fatal(err)
	}
	next := SoakRecord{Revision: "same", StartedAt: time.Now(), EvidenceID: "new"}
	if err := s.Set(next); err != nil {
		t.Fatal(err)
	}
	if got := OpenSoak(path).Get(); got.EvidenceID != "new" || !got.StartedAt.Equal(next.StartedAt) {
		t.Fatalf("record = %#v, want reset soak identity", got)
	}
}

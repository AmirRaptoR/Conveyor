package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The whole point: a second holder is refused rather than allowed to sweep a
// live engine's runs to "interrupted".
func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "owner.lock")
	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	defer first.Release()

	_, err = AcquireLock(path)
	var busy *Busy
	if !errors.As(err, &busy) {
		t.Fatalf("second claim = %v, want *Busy", err)
	}
	if busy.PID != os.Getpid() {
		t.Errorf("Busy names pid %d, want %d — the message should say who holds it", busy.PID, os.Getpid())
	}
}

// Releasing hands the claim on, so a finished `tick` does not keep the service
// out of its own data directory.
func TestReleaseFreesTheClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	first, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	_ = second.Release()
}

package server

import (
	"os"
	"path/filepath"
	"testing"
)

// New restricts the data directory to the owner alone — it holds
// order.json, answers.json, push subscriptions, the VAPID keys and every run
// directory underneath runner.Root.
func TestNewSecuresTheDataDir(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	dataDir := cfg.DataDir()
	// New itself creates it (secureDataDir), so this also covers "did not
	// already exist".
	_ = New(cfg, r)
	info, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("data dir mode = %o, want 0700", perm)
	}
}

// A data dir already created looser — an older build, or a person's mkdir —
// is tightened, not left alone.
func TestNewTightensAnAlreadyLooseDataDir(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	dataDir := cfg.DataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = New(cfg, r)
	info, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("data dir mode = %o, want 0700 after tightening", perm)
	}
}

// secureDataDir must not refuse to let the process continue when it cannot
// chmod — a warning naming the path, not a startup failure.
func TestSecureDataDirNeverPanicsWhenChmodFails(t *testing.T) {
	// A path that cannot exist as a directory (its parent is a file) makes
	// MkdirAll fail; secureDataDir must simply return.
	base := t.TempDir()
	blocker := filepath.Join(base, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	secureDataDir(filepath.Join(blocker, "data")) // must not panic
}

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchdogEvidenceDistinguishesFirstUseFromLossOrCorruption(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(string) error
	}{
		{"deleted", os.Remove},
		{"truncated", func(path string) error { return os.WriteFile(path, []byte(`{"schema":1`), 0o600) }},
		{"corrupt", func(path string) error { return os.WriteFile(path, []byte(`not-json`), 0o600) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "watchdog.json")
			w := OpenWatchdog(path)
			if status := w.Status(); status.Established || status.Healthy {
				t.Fatalf("fresh status = %#v", status)
			}
			if err := w.Establish(WatchdogState{LastProgressAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := tc.mutate(path); err != nil {
				t.Fatal(err)
			}
			if status := OpenWatchdog(path).Status(); !status.Established || status.Healthy || status.Error == "" {
				t.Fatalf("lost status = %#v", status)
			}
		})
	}
}

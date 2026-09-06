package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/store"
)

// writeProbeConfig writes a minimal, valid config to dir/conveyor.yaml whose
// auth.origins is exactly origins, and returns its path.
func writeProbeConfig(t *testing.T, dir string, origins []string) string {
	t.Helper()
	var originsYAML strings.Builder
	if len(origins) > 0 {
		originsYAML.WriteString("  origins:\n")
		for _, o := range origins {
			originsYAML.WriteString("    - \"" + o + "\"\n")
		}
	}
	content := "version: 1\n" +
		"auth:\n" + originsYAML.String() +
		"stages:\n  - name: backlog\n  - name: done\n    terminal: true\n" +
		"sources:\n  - name: mock\n    provider:\n      name: mock\n"
	path := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubStatus(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The whole point of the command: a config whose auth.origins all answer
// healthily exits clean, and one where any answers 403 (exactly #54's shape)
// exits non-zero.
func TestCmdProbeExitsOnOriginHealth(t *testing.T) {
	healthy := stubStatus(t, http.StatusOK)
	rejecting := stubStatus(t, http.StatusForbidden)

	t.Run("every origin healthy", func(t *testing.T) {
		cfgPath := writeProbeConfig(t, t.TempDir(), []string{healthy.URL})
		var out bytes.Buffer
		if err := runProbe([]string{"-c", cfgPath, "-wait", "1s", "-notify=false"}, &out); err != nil {
			t.Fatalf("err = %v, output:\n%s", err, out.String())
		}
	})

	t.Run("an origin answering 403 fails the run", func(t *testing.T) {
		cfgPath := writeProbeConfig(t, t.TempDir(), []string{rejecting.URL})
		var out bytes.Buffer
		if err := runProbe([]string{"-c", cfgPath, "-wait", "1s", "-notify=false"}, &out); err == nil {
			t.Fatalf("want an error; output:\n%s", out.String())
		}
		if !strings.Contains(out.String(), rejecting.URL) {
			t.Errorf("output does not name the failing origin:\n%s", out.String())
		}
	})
}

// A missing auth.origins key is exactly the shape of #54 and must not read as
// success — an empty probe set is a failure, not a silent no-op.
func TestCmdProbeEmptySetFails(t *testing.T) {
	cfgPath := writeProbeConfig(t, t.TempDir(), nil)
	var out bytes.Buffer
	err := runProbe([]string{"-c", cfgPath, "-wait", "1s", "-notify=false"}, &out)
	if err == nil {
		t.Fatal("an empty probe set must fail")
	}
	if !strings.Contains(out.String(), "nothing to check") {
		t.Errorf("output does not say there was nothing to check:\n%s", out.String())
	}
}

// -origin adds to the probe set drawn from the config; a config with no
// auth.origins at all still has something to check when -origin is given.
func TestCmdProbeOriginFlagExtendsTheSet(t *testing.T) {
	healthy := stubStatus(t, http.StatusOK)
	cfgPath := writeProbeConfig(t, t.TempDir(), nil)
	var out bytes.Buffer
	if err := runProbe([]string{"-c", cfgPath, "-wait", "1s", "-notify=false", "-origin", healthy.URL}, &out); err != nil {
		t.Fatalf("err = %v, output:\n%s", err, out.String())
	}
}

// -addr is a diagnostic only: it is probed and printed, but never decides the
// exit status either way.
func TestCmdProbeAddrNeverAffectsExitStatus(t *testing.T) {
	loopbackHealthy := stubStatus(t, http.StatusOK)
	rejecting := stubStatus(t, http.StatusForbidden)
	cfgPath := writeProbeConfig(t, t.TempDir(), []string{rejecting.URL})

	var out bytes.Buffer
	err := runProbe([]string{"-c", cfgPath, "-wait", "1s", "-notify=false",
		"-addr", strings.TrimPrefix(loopbackHealthy.URL, "http://")}, &out)
	if err == nil {
		t.Fatalf("a healthy loopback must not turn a rejected configured origin into a pass; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "loopback") {
		t.Errorf("output has no separate loopback diagnostic line:\n%s", out.String())
	}
}

// probe must never contend for the data directory's owner lock: a running
// engine holds it, and probe is read-only.
func TestCmdProbeDoesNotAcquireOwnerLock(t *testing.T) {
	dir := t.TempDir()
	healthy := stubStatus(t, http.StatusOK)
	cfgPath := writeProbeConfig(t, dir, []string{healthy.URL})

	lock, err := store.AcquireLock(filepath.Join(dir, "data", "owner.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	var out bytes.Buffer
	if err := runProbe([]string{"-c", cfgPath, "-wait", "1s", "-notify=false"}, &out); err != nil {
		t.Fatalf("probe contended for the owner lock (or failed some other way): %v\noutput:\n%s", err, out.String())
	}
}

// -notify is gated on failure: a successful probe must not even try to send
// a push, so a data directory with no vapid.json produces no "push:" line at
// all on success, and does on failure (Notify was called and reported it had
// nothing to send to).
func TestCmdProbeOnlyNotifiesOnFailure(t *testing.T) {
	healthy := stubStatus(t, http.StatusOK)
	rejecting := stubStatus(t, http.StatusForbidden)

	t.Run("success: no push line at all", func(t *testing.T) {
		cfgPath := writeProbeConfig(t, t.TempDir(), []string{healthy.URL})
		var out bytes.Buffer
		if err := runProbe([]string{"-c", cfgPath, "-wait", "1s"}, &out); err != nil {
			t.Fatalf("err = %v, output:\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "push:") {
			t.Errorf("a successful probe tried to notify:\n%s", out.String())
		}
	})

	t.Run("failure: push was attempted and reported", func(t *testing.T) {
		cfgPath := writeProbeConfig(t, t.TempDir(), []string{rejecting.URL})
		var out bytes.Buffer
		if err := runProbe([]string{"-c", cfgPath, "-wait", "1s"}, &out); err == nil {
			t.Fatal("want an error")
		}
		if !strings.Contains(out.String(), "push:") {
			t.Errorf("a failed probe with -notify (default) never mentioned push:\n%s", out.String())
		}
	})
}

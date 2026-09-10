package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// cancellableTwoSourcesFor lays down two sources whose "working" stage spawns
// a TERM-resistant grandchild (the same shape runner's own
// TestCancellationReapsProcessGroup uses) and records its pid, so a test can
// confirm cancellation actually reaps a specific run's children rather than
// merely returning.
func cancellableTwoSourcesFor(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	// PIDFILE names where this run's resistant grandchild's pid goes.
	writeScript(t, filepath.Join(dir, "work.sh"), `#!/bin/sh
trap 'exit 0' TERM
(
	trap '' TERM
	exec >/dev/null 2>&1
	echo $$ > "$PIDFILE"
	sleep 60
) &
disown
touch "$STARTED"
sleep 60
`)
	for _, s := range []string{"repo1", "repo2"} {
		if err := os.MkdirAll(filepath.Join(dir, s), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 100ms
concurrency:
  perSource: 2
  perStage: 2
  global: 4
stages:
  - name: backlog
  - name: working
    script: work
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo1
    scripts:
      work:
        script: ./work.sh
        params:
          STARTED: `+filepath.Join(dir, "started1")+`
          PIDFILE: `+filepath.Join(dir, "pid1")+`
  - name: s2
    provider: fake
    workdir: ./repo2
    scripts:
      work:
        script: ./work.sh
        params:
          STARTED: `+filepath.Join(dir, "started2")+`
          PIDFILE: `+filepath.Join(dir, "pid2")+`
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), dir
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				return p
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never got a pid written to it", path)
	return 0
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// The whole feature: cancelling one item's run reaps that run's own children
// (the corrected process-group shutdown path) and leaves a concurrently
// running item on another source completely untouched.
func TestCancelReapsOnlyTheSelectedRunsChildren(t *testing.T) {
	cfg, r, dir := cancellableTwoSourcesFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{
		{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "cancel this one"},
		{ID: "s2:1", Ref: "1", Source: "s2", Stage: "backlog", Title: "leave this one running"},
	}
	if n := s.launch(s.ctx); n != 2 {
		t.Fatalf("launch() started %d, want 2", n)
	}
	waitFor(t, "s1 to start", func() bool { _, err := os.Stat(filepath.Join(dir, "started1")); return err == nil })
	waitFor(t, "s2 to start", func() bool { _, err := os.Stat(filepath.Join(dir, "started2")); return err == nil })
	pid1 := readPID(t, filepath.Join(dir, "pid1"))
	pid2 := readPID(t, filepath.Join(dir, "pid2"))

	// No reason: refused, and nothing is touched.
	req := httptest.NewRequest(http.MethodPost, "/api/items/s1:1/cancel", bytes.NewReader([]byte(`{}`)))
	req.SetPathValue("id", "s1:1")
	w := httptest.NewRecorder()
	s.handleCancel(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cancel with no reason = %d, want 400", w.Code)
	}

	// An id with no run in flight: refused.
	req = httptest.NewRequest(http.MethodPost, "/api/items/no-such-item/cancel", bytes.NewReader([]byte(`{"reason":"x"}`)))
	req.SetPathValue("id", "no-such-item")
	w = httptest.NewRecorder()
	s.handleCancel(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("cancel on an item with nothing running = %d, want 409", w.Code)
	}

	body, _ := json.Marshal(map[string]any{"reason": "runaway job, freeing the slot"})
	req = httptest.NewRequest(http.MethodPost, "/api/items/s1:1/cancel", bytes.NewReader(body))
	req.SetPathValue("id", "s1:1")
	req.SetBasicAuth("amir", "whatever")
	w = httptest.NewRecorder()
	s.handleCancel(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d, want 202: %s", w.Code, w.Body)
	}

	s.mu.RLock()
	audit, ok := s.cancels["s1:1"]
	s.mu.RUnlock()
	if !ok || audit.Reason != "runaway job, freeing the slot" || audit.By != "amir" {
		t.Errorf("audit record = %+v, ok=%v", audit, ok)
	}

	waitFor(t, "s1's transition to finish", func() bool { _, running := s.working.Load("s1:1"); return !running })
	waitFor(t, "s1's grandchild to die", func() bool { return !alive(pid1) })

	// s2 was never asked to stop and is still running, children and all.
	if !alive(pid2) {
		t.Error("cancelling s1's run killed s2's grandchild too")
	}
	if _, running := s.working.Load("s2:1"); !running {
		t.Error("cancelling s1's run affected s2's transition")
	}

	// Clean up: s2's script sleeps 60s with no release mechanism, so cancel
	// it too, through the same path, rather than leaking it past the test.
	body2, _ := json.Marshal(map[string]any{"reason": "test cleanup"})
	req = httptest.NewRequest(http.MethodPost, "/api/items/s2:1/cancel", bytes.NewReader(body2))
	req.SetPathValue("id", "s2:1")
	w = httptest.NewRecorder()
	s.handleCancel(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("cleanup cancel of s2 = %d, want 202", w.Code)
	}
	waitFor(t, "s2's grandchild to die", func() bool { return !alive(pid2) })
}

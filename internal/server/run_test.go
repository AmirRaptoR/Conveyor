package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// writeTestRun lays down a run directory directly, the way the runner would
// have, without actually running a script.
func writeTestRun(t *testing.T, root, day, id string, m model.Run) string {
	t.Helper()
	dir := filepath.Join(root, day, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m.ID = id
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "log.txt"), []byte("00:00:00.000 stdout hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A run created before hundreds of newer ones must still open through
// GET /api/runs/{id} while it is retained — the old code searched only the
// newest 500 and 404'd on anything past that.
func TestRunOlderThan500NewerRunsStillOpens(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)

	writeTestRun(t, r.Root, "2026-08-01", "000000.000-target", model.Run{
		Source: "s1", Kind: "stage", Outcome: model.OutcomeFailure, ExitCode: 1,
	})
	for i := 0; i < 501; i++ {
		writeTestRun(t, r.Root, "2026-09-01", fmtRunID(i), model.Run{
			Source: "s1", Kind: "stage", Outcome: model.OutcomeSuccess,
		})
	}

	req := httptest.NewRequest("GET", "/api/runs/000000.000-target", nil)
	req.SetPathValue("id", "000000.000-target")
	w := httptest.NewRecorder()
	s.handleRun(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got RunMeta
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "000000.000-target" {
		t.Errorf("ID = %q, want 000000.000-target", got.ID)
	}
}

// fmtRunID gives a valid-shaped id (HHMMSS.mmm-<suffix>) that varies with i.
func fmtRunID(i int) string { return fmt.Sprintf("120000.%03d-r%x", i%1000, i) }

// The lookup must be O(day directories), not O(runs): it resolves the ID
// directly against each day directory rather than reading every meta.json to
// find it. Proven by deleting every meta.json except the target's — a scan
// over metadata could not find it, but a lookup by directory name still can.
func TestRunLookupIsByDirectoryNameNotByScanningMetadata(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)

	writeTestRun(t, r.Root, "2026-08-01", "000000.000-target", model.Run{
		Source: "s1", Kind: "stage", Outcome: model.OutcomeSuccess,
	})
	for i := 0; i < 20; i++ {
		dir := writeTestRun(t, r.Root, "2026-08-02", fmtRunID(i), model.Run{
			Source: "s1", Kind: "stage", Outcome: model.OutcomeSuccess,
		})
		if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest("GET", "/api/runs/000000.000-target", nil)
	req.SetPathValue("id", "000000.000-target")
	w := httptest.NewRecorder()
	s.handleRun(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

// An ID that does not match the runner's own shape must be rejected with 400
// before any path is built — never interpreted as a filesystem path.
func TestRunLookupRejectsUnsafeIDsWith400(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	secret := filepath.Join(filepath.Dir(r.Root), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not read me"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"../secret.txt", "", "..%2Fsecret.txt", "/etc/passwd"} {
		req := httptest.NewRequest("GET", "/api/runs/"+id, nil)
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()
		s.handleRun(w, req)
		if w.Code != 400 {
			t.Errorf("id %q: status = %d, want 400; body: %s", id, w.Code, w.Body.String())
		}
		if w.Body.String() != "" && w.Code == 200 {
			t.Errorf("id %q: leaked file contents: %s", id, w.Body.String())
		}
	}
}

// An id that resolves to nothing is 404 until retention has actually removed
// something, and 410 afterwards — naming the horizon, never confused with the
// old arbitrary 500-run lookup cutoff.
func TestRunLookupDistinguishesNeverExistedFromRetained(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)

	req := httptest.NewRequest("GET", "/api/runs/000000.000-nope", nil)
	req.SetPathValue("id", "000000.000-nope")
	w := httptest.NewRecorder()
	s.handleRun(w, req)
	if w.Code != 404 {
		t.Fatalf("before any sweep: status = %d, want 404", w.Code)
	}

	s.mu.Lock()
	s.everSwept = true
	s.sweepHorizon = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	s.mu.Unlock()

	w = httptest.NewRecorder()
	s.handleRun(w, req)
	if w.Code != 410 {
		t.Fatalf("after a sweep removed something: status = %d, want 410", w.Code)
	}
	if !strings.Contains(w.Body.String(), "2026-08-01") {
		t.Errorf("410 body does not name the horizon: %s", w.Body.String())
	}
}

// writeTestRunWithLog is writeTestRun plus a log.txt of n lines, for exercising
// bounded retrieval.
func writeTestRunWithLog(t *testing.T, root, day, id string, n int) string {
	t.Helper()
	dir := writeTestRun(t, root, day, id, model.Run{Source: "s1", Kind: "stage", Outcome: model.OutcomeSuccess})
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "00:00:00.000 stdout line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "log.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// With no query parameters, a large log is still handed back bounded — the
// last 2000 lines — with the true count reported alongside it.
func TestRunLogDefaultsToLast2000LinesAndReportsTotal(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	writeTestRunWithLog(t, r.Root, "2026-08-01", "000000.000-big", 50000)

	req := httptest.NewRequest("GET", "/api/runs/000000.000-big", nil)
	req.SetPathValue("id", "000000.000-big")
	w := httptest.NewRecorder()
	s.handleRun(w, req)

	var got RunMeta
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.TotalLines != 50000 {
		t.Errorf("totalLines = %d, want 50000", got.TotalLines)
	}
	if len(got.Lines) != 2000 {
		t.Fatalf("len(Lines) = %d, want 2000", len(got.Lines))
	}
	if got.Lines[0].Text != "line 48001" || got.Lines[1999].Text != "line 50000" {
		t.Errorf("window = [%q .. %q], want [line 48001 .. line 50000]",
			got.Lines[0].Text, got.Lines[1999].Text)
	}
}

// tail and offset page through a log in units of lines.
func TestRunLogAcceptsTailAndOffset(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	writeTestRunWithLog(t, r.Root, "2026-08-01", "000000.000-paged", 100)

	req := httptest.NewRequest("GET", "/api/runs/000000.000-paged?tail=10&offset=5", nil)
	req.SetPathValue("id", "000000.000-paged")
	w := httptest.NewRecorder()
	s.handleRun(w, req)

	var got RunMeta
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 10 {
		t.Fatalf("len(Lines) = %d, want 10", len(got.Lines))
	}
	if got.Lines[0].Text != "line 6" || got.Lines[9].Text != "line 15" {
		t.Errorf("window = [%q .. %q], want [line 6 .. line 15]", got.Lines[0].Text, got.Lines[9].Text)
	}
	if got.TotalLines != 100 {
		t.Errorf("totalLines = %d, want 100", got.TotalLines)
	}
}

// A symlink inside the run root pointing outside it must not be served, even
// if its name matches a valid run ID.
func TestRunLookupRefusesASymlinkEscape(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)

	day := filepath.Join(r.Root, "2026-08-01")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "meta.json"), []byte(`{"id":"evil"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(day, "000000.000-escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/runs/000000.000-escape", nil)
	req.SetPathValue("id", "000000.000-escape")
	w := httptest.NewRecorder()
	s.handleRun(w, req)
	if w.Code == 200 {
		t.Fatalf("symlink escape was served: %s", w.Body.String())
	}
}

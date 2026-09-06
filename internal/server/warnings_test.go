package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// twoSourcePipeline lays down two sources: s1's list script always fails,
// s2's always succeeds — so a refresh can be asserted to have recorded one
// outcome per source, independently.
func twoSourcePipeline(t *testing.T, poll string) (*config.Config, *runner.Runner) {
	t.Helper()
	dir := t.TempDir()

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 1\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "ok", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "ok", "move.sh"), "#!/bin/sh\nexit 0\n")
	for _, repo := range []string{"repo1", "repo2"} {
		if err := os.MkdirAll(filepath.Join(dir, repo), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pollLine := ""
	if poll != "" {
		pollLine = "poll: " + poll + "\n"
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(pollLine+`version: 1
stages:
  - name: backlog
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo1
  - name: s2
    provider: ok
    workdir: ./repo2
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs"))
}

func sourceView(t *testing.T, s *Server, name string) SourceView {
	t.Helper()
	for _, sv := range s.state.Sources {
		if sv.Name == name {
			return sv
		}
	}
	t.Fatalf("no source view named %q", name)
	return SourceView{}
}

// Engine: SourceView carries lastListedAt and listError, both omitted when
// empty; after one refresh the failing source has listError and no
// lastListedAt, and the succeeding one has a recent lastListedAt and no
// listError.
func TestRefreshRecordsListingOutcomePerSource(t *testing.T) {
	cfg, r := twoSourcePipeline(t, "")
	s := New(cfg, r)
	s.refresh(context.Background())

	s1, s2 := sourceView(t, s, "s1"), sourceView(t, s, "s2")

	if s1.ListError == "" {
		t.Error("s1 (list exits 1): want a non-empty listError")
	}
	if s1.LastListedAt != "" {
		t.Errorf("s1 (list exits 1): want no lastListedAt, got %q", s1.LastListedAt)
	}

	if s2.ListError != "" {
		t.Errorf("s2 (list exits 0): want no listError, got %q", s2.ListError)
	}
	if s2.LastListedAt == "" {
		t.Fatal("s2 (list exits 0): want a lastListedAt")
	}
	got, err := time.Parse(time.RFC3339, s2.LastListedAt)
	if err != nil {
		t.Fatalf("lastListedAt not RFC3339: %v", err)
	}
	if since := time.Since(got); since < 0 || since > time.Second {
		t.Errorf("lastListedAt = %v, want within a second of now", got)
	}
}

// Engine: both fields describe the latest attempt, not a high-water mark.
func TestRefreshOutcomesReflectOnlyTheLatestAttempt(t *testing.T) {
	cfg, r := twoSourcePipeline(t, "")
	s := New(cfg, r)

	// First refresh: s1 fails, as configured above.
	s.refresh(context.Background())
	if sourceView(t, s, "s1").ListError == "" {
		t.Fatal("expected s1 to have failed on the first refresh")
	}

	// s1 now succeeds: rewrite its list script.
	writeScript(t, filepath.Join(cfg.ProvidersDir(), "fake", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[]
JSON
`)
	s.refresh(context.Background())
	s1 := sourceView(t, s, "s1")
	if s1.ListError != "" {
		t.Errorf("after a success: want listError cleared, got %q", s1.ListError)
	}
	if s1.LastListedAt == "" {
		t.Error("after a success: want lastListedAt set")
	}
	successAt := s1.LastListedAt

	// s1 fails again: the earlier successful lastListedAt must survive.
	writeScript(t, filepath.Join(cfg.ProvidersDir(), "fake", "list.sh"), "#!/bin/sh\nexit 1\n")
	s.refresh(context.Background())
	s1 = sourceView(t, s, "s1")
	if s1.ListError == "" {
		t.Error("after a second failure: want listError set")
	}
	if s1.LastListedAt != successAt {
		t.Errorf("after a second failure: lastListedAt = %q, want it unchanged at %q", s1.LastListedAt, successAt)
	}
}

// Engine: /api/state carries pollNs, equal to the configured poll:.
func TestApiStateCarriesPollNs(t *testing.T) {
	cfg, r := twoSourcePipeline(t, "250ms")
	s := New(cfg, r)

	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var got State
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := int64(250 * time.Millisecond); got.PollNs != time.Duration(want) {
		t.Errorf("pollNs = %v, want %v", got.PollNs, time.Duration(want))
	}
}

// Engine: the construction-time sourceViews call yields sources with no
// lastListedAt and no listError — "not listed yet", not a zero time.
func TestNewServerSourcesHaveNoListingHistoryYet(t *testing.T) {
	cfg, r := twoSourcePipeline(t, "")
	s := New(cfg, r)
	for _, sv := range s.state.Sources {
		if sv.LastListedAt != "" {
			t.Errorf("source %q: want no lastListedAt before any refresh, got %q", sv.Name, sv.LastListedAt)
		}
		if sv.ListError != "" {
			t.Errorf("source %q: want no listError before any refresh, got %q", sv.Name, sv.ListError)
		}
	}
}

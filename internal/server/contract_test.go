package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// dedupeCrossSource: first in configuration order wins, matching the rule a
// single source's own listing already applies to itself.
func TestDedupeCrossSourceKeepsTheFirstConfiguredSource(t *testing.T) {
	var warnings []string
	in := []model.Item{
		{ID: "shared:1", Source: "a", Title: "from a"},
		{ID: "only:2", Source: "a", Title: "unique"},
		{ID: "shared:1", Source: "b", Title: "from b"},
	}
	out := dedupeCrossSource(in, func(msg string) { warnings = append(warnings, msg) })
	if len(out) != 2 {
		t.Fatalf("out = %v, want 2 items", out)
	}
	if out[0].Source != "a" || out[1].Source != "a" {
		t.Errorf("out = %+v, want both surviving items from source a", out)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "shared:1") ||
		!strings.Contains(warnings[0], "a") || !strings.Contains(warnings[0], "b") {
		t.Errorf("warnings = %v, want one naming shared:1 and both sources", warnings)
	}
}

// Two sources listing the same id in one poll must never both land on the
// board: the server keys working, blocks, times and the manual order by the
// bare id.
func TestRefreshNeverPublishesTwoItemsWithTheSameID(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "providers", "a", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"dup:1","ref":"1","source":"a","stage":"backlog","title":"from a"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "a", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "b", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"dup:1","ref":"1","source":"b","stage":"backlog","title":"from b"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "b", "move.sh"), "#!/bin/sh\nexit 0\n")
	for _, name := range []string{"repoa", "repob"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
stages:
  - name: backlog
  - name: done
    terminal: true
sources:
  - name: a
    provider: a
    workdir: ./repoa
  - name: b
    provider: b
    workdir: ./repob
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, runner.New(filepath.Join(dir, "runs")))
	s.ctx = context.Background()
	s.refresh(context.Background())

	s.mu.RLock()
	items, warnings := s.state.Items, s.state.Warnings
	s.mu.RUnlock()
	if len(items) != 1 {
		t.Fatalf("items = %+v, want exactly one surviving dup:1", items)
	}
	if items[0].Source != "a" {
		t.Errorf("surviving item is from %q, want a (first in configuration order)", items[0].Source)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "dup:1") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one naming the dup:1 collision", warnings)
	}
}

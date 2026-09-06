package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// twoSourceBoard has one source that always lists successfully and one whose
// list script can be told, via a marker file, to fail instead.
func twoSourceBoard(t *testing.T) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	failMarker := filepath.Join(dir, "fail")
	writeScript(t, filepath.Join(dir, "providers", "good", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"good:1","ref":"1","source":"good","stage":"backlog","title":"steady work"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "good", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "flaky", "list.sh"), `#!/bin/sh
if [ -e "`+failMarker+`" ]; then
  echo "the API is down" >&2
  exit 1
fi
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"flaky:1","ref":"1","source":"flaky","stage":"backlog","title":"unreliable source"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "flaky", "move.sh"), "#!/bin/sh\nexit 0\n")
	for _, name := range []string{"repogood", "repoflaky"} {
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
  - name: good
    provider: good
    workdir: ./repogood
  - name: flaky
    provider: flaky
    workdir: ./repoflaky
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), dir
}

// F03: a source whose List fails has produced no information, not a signal
// that its work vanished — the board must keep what it already knew about
// that source rather than have the wholesale item assignment delete it, and
// the source's warning must still surface.
func TestAFailedListingRetainsThatSourcesItems(t *testing.T) {
	cfg, r, dir := twoSourceBoard(t)
	s := New(cfg, r)
	s.ctx = context.Background()

	s.refresh(s.ctx)
	s.mu.RLock()
	n := len(s.state.Items)
	s.mu.RUnlock()
	if n != 2 {
		t.Fatalf("first listing produced %d items, want 2", n)
	}

	if err := os.WriteFile(filepath.Join(dir, "fail"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.refresh(s.ctx)

	s.mu.RLock()
	got, warnings := ids(s.state.Items), s.state.Warnings
	s.mu.RUnlock()
	if !contains(got, "good:1") || !contains(got, "flaky:1") {
		t.Errorf("items after a failed listing = %v, want both good:1 (fresh) and flaky:1 (retained)", got)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "flaky") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one naming the flaky source's failure", warnings)
	}

	// And a *successful* listing that genuinely omits an item still removes
	// it — tombstones remain out of scope.
	if err := os.Remove(filepath.Join(dir, "fail")); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(dir, "providers", "flaky", "list.sh"), "#!/bin/sh\nexit 0\n")
	s.refresh(s.ctx)
	s.mu.RLock()
	final := ids(s.state.Items)
	s.mu.RUnlock()
	if contains(final, "flaky:1") {
		t.Errorf("items = %v, want flaky:1 gone after a successful listing that omits it", final)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

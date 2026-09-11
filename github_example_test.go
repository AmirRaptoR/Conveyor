// This test keeps conveyor.github.example.yaml honest the way readme_test.go
// keeps README.md's fenced examples honest: `conveyor validate` alone is not
// enough, because provider, agent, script and workdir failures land in
// Source.Problems rather than in Validate (internal/config/config.go,
// resolveSources) — so a config that merely parses could still be unusable.
package readme_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
)

// repoRoot is defined in readme_test.go, in this same package.

// TestGitHubExampleSourcesAreHealthy loads conveyor.github.example.yaml
// exactly as shipped, except for substituting the placeholder `workdir:` for
// a real directory — a missing workdir is itself a Source.Problem, and the
// shipped file deliberately keeps a placeholder one so it can never act on a
// real repository as shipped (see the file's own header comment).
func TestGitHubExampleSourcesAreHealthy(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "conveyor.github.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	const placeholder = "workdir: /path/to/a/real/checkout"
	if !strings.Contains(string(raw), placeholder) {
		t.Fatalf("conveyor.github.example.yaml: expected placeholder %q not found — did the shipped workdir change?", placeholder)
	}

	realWorkdir := t.TempDir()
	patched := strings.Replace(string(raw), placeholder, "workdir: "+realWorkdir, 1)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	// providers/ and agents/ resolve beside the config file; symlink them in
	// so this temp copy resolves exactly as the shipped file does sitting at
	// the repository root (same trick as readme_test.go).
	if err := os.Symlink(filepath.Join(root, "providers"), filepath.Join(dir, "providers")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "agents"), filepath.Join(dir, "agents")); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("conveyor.github.example.yaml: %v", err)
	}
	if len(cfg.Sources) == 0 {
		t.Fatal("conveyor.github.example.yaml: no sources defined")
	}
	for _, s := range cfg.Sources {
		if !s.OK() {
			t.Fatalf("conveyor.github.example.yaml: source %q cannot run: %s", s.Name, strings.Join(s.Problems, "; "))
		}
	}
}

// TestGitHubExampleKeepsPlaceholders is the mirror of the health check: the
// shipped file must still fail to act on a real repository. `workdir:` is
// covered by the substitution above finding the exact placeholder text;
// REPO: is checked directly, since env: is never validated for existence the
// way workdir is.
func TestGitHubExampleKeepsPlaceholders(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "conveyor.github.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "REPO: OWNER/REPO") {
		t.Fatal("conveyor.github.example.yaml: expected placeholder \"REPO: OWNER/REPO\" not found")
	}
}

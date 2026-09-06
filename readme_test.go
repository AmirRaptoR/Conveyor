// This test keeps README.md honest about the schema: every fenced ```yaml
// block that presents a complete config — one with a top-level `version:` key,
// which the loader requires and no fragment carries — is loaded exactly as a
// config sitting at the repository root would be, so a stale example is a test
// failure instead of something a newcomer discovers by pasting it in.
package readme_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
)

// repoRoot is the directory this file lives in — README.md, providers/ and
// agents/ all sit beside it.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this test file's own path")
	}
	return filepath.Dir(file)
}

// yamlBlock is one fenced ```yaml block from README.md, with the 1-based line
// its opening fence sits on — named in every failure, so a broken example is
// found by the line it starts at, not by reading the whole file.
type yamlBlock struct {
	line int
	body string
}

func extractYAMLBlocks(t *testing.T, readme string) []yamlBlock {
	t.Helper()
	lines := strings.Split(readme, "\n")
	var blocks []yamlBlock
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```yaml" {
			continue
		}
		start := i
		var body []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			body = append(body, lines[i])
		}
		if i >= len(lines) {
			t.Fatalf("README.md:%d: ```yaml block never closed", start+1)
		}
		blocks = append(blocks, yamlBlock{line: start + 1, body: strings.Join(body, "\n") + "\n"})
	}
	return blocks
}

// topLevelVersion matches a `version:` key at the start of a line — the
// loader requires it (config.LoadFrom rejects a config with none), so every
// complete config carries one and every fragment in the README does not.
var topLevelVersion = regexp.MustCompile(`(?m)^version:\s*\S`)

func TestReadmeConfigsValidate(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}

	blocks := extractYAMLBlocks(t, string(raw))
	tested := 0
	for _, b := range blocks {
		b := b
		if !topLevelVersion.MatchString(b.body) {
			continue // a fragment, not a complete config
		}
		tested++
		t.Run(fmt.Sprintf("README.md:%d", b.line), func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "conveyor.yaml")
			if err := os.WriteFile(cfgPath, []byte(b.body), 0o644); err != nil {
				t.Fatal(err)
			}
			// Resolve providers/ and agents/ the way they would beside a
			// config that actually sat at the repository root, without
			// writing anything into it: config.LoadFrom takes an explicit
			// providers root, but agents/ always resolves beside the config
			// file (Config.AgentsDir), so a symlink is what stands in for
			// "the repository root" for both at once.
			if err := os.Symlink(filepath.Join(root, "providers"), filepath.Join(dir, "providers")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "agents"), filepath.Join(dir, "agents")); err != nil {
				t.Fatal(err)
			}

			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("README.md:%d: %v", b.line, err)
			}
			for _, s := range cfg.Sources {
				if !s.OK() {
					t.Fatalf("README.md:%d: source %q cannot run: %s", b.line, s.Name, strings.Join(s.Problems, "; "))
				}
			}
		})
	}

	if tested == 0 {
		t.Fatal("no complete (`version:`-carrying) config found in README.md")
	}
}

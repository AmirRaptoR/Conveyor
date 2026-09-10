// Keeps the vendored Claude Code skills (agents/claude/skills/) honest against
// issue #70's acceptance criteria: each is a real, machine-agnostic SKILL.md,
// and the implement skill's branch step actually defers to the branch it was
// handed rather than switching or creating one, matching the worktree
// invariant CLAUDE.md states (`/implement` step 2 defers to a branch it was
// handed").
package readme_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"gopkg.in/yaml.v3"
)

var vendoredSkills = []string{"refine", "implement", "review"}

func skillsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "agents", "claude", "skills")
}

// splitFrontmatter returns the YAML between the opening and closing `---`
// fences and the rest of the file. It fails the test if the file does not
// open with exactly one such block.
func splitFrontmatter(t *testing.T, path, content string) (yamlBody, rest string) {
	t.Helper()
	if !strings.HasPrefix(content, "---\n") {
		t.Fatalf("%s: does not open with a YAML frontmatter fence (---)", path)
	}
	remainder := content[len("---\n"):]
	end := strings.Index(remainder, "\n---")
	if end == -1 {
		t.Fatalf("%s: frontmatter fence opened but never closed", path)
	}
	yamlBody = remainder[:end]
	afterFence := remainder[end+len("\n---"):]
	// A second fence anywhere later in the body would mean more than one
	// frontmatter block; there must be exactly one.
	if strings.Contains(afterFence, "\n---\n") || strings.HasPrefix(strings.TrimLeft(afterFence, "\n"), "---\n") {
		t.Fatalf("%s: contains more than one frontmatter-looking block", path)
	}
	return yamlBody, afterFence
}

func TestVendoredSkillsExistAndParse(t *testing.T) {
	dir := skillsDir(t)
	for _, name := range vendoredSkills {
		name := name
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name, "SKILL.md")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			content := string(raw)

			fm, _ := splitFrontmatter(t, path, content)

			var meta struct {
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
			}
			if err := yaml.Unmarshal([]byte(fm), &meta); err != nil {
				t.Fatalf("%s: frontmatter does not parse as YAML: %v", path, err)
			}
			if meta.Name == "" {
				t.Fatalf("%s: frontmatter name: is empty", path)
			}
			if meta.Name != name {
				t.Fatalf("%s: frontmatter name: %q does not match its own directory name %q", path, meta.Name, name)
			}
			if meta.Description == "" {
				t.Fatalf("%s: frontmatter description: is empty", path)
			}
		})
	}
}

// suspiciousPath matches an absolute filesystem path (two or more segments,
// so a slash command like `/refine` is not itself a false positive) or a
// `~/`-relative one. Scanned generally rather than against a fixed list of
// three strings, per the acceptance criterion.
var suspiciousPath = regexp.MustCompile(`(?:^|[\s"'` + "`" + `(])(/[\w.-]+(?:/[\w.-]+)+|~/[\w./-]*)`)

func TestVendoredSkillsCarryNoMachineSpecificPaths(t *testing.T) {
	dir := skillsDir(t)
	for _, name := range vendoredSkills {
		name := name
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name, "SKILL.md")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, m := range suspiciousPath.FindAllStringSubmatch(string(raw), -1) {
				t.Errorf("%s: looks like an absolute or home-relative filesystem path: %q", path, m[1])
			}
		})
	}
}

// TestImplementSkillDefersToItsBranch is issue #70's third vendored-skill
// criterion: given a checkout already on a branch other than the default,
// following the branch step must leave that branch checked out — no switch,
// no new branch, by any command. Checked by reading the file's actual
// commands (not just its prose) so a reworded step that still runs
// `git checkout main` would be caught.
func TestImplementSkillDefersToItsBranch(t *testing.T) {
	path := filepath.Join(skillsDir(t), "implement", "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	_, body := splitFrontmatter(t, path, content)

	// Isolate the "### 2. Branch" section itself — the actual step a runner
	// follows — rather than the whole file, which also *names* the forbidden
	// commands in its Red Flags list as things to stop on.
	start := strings.Index(body, "### 2. Branch")
	if start == -1 {
		t.Fatalf("%s: no \"### 2. Branch\" section found", path)
	}
	rest := body[start:]
	end := strings.Index(rest, "\n### 3.")
	if end == -1 {
		t.Fatalf("%s: \"### 2. Branch\" section never ends before a step 3", path)
	}
	section := rest[:end]

	// Only commands actually run — inside fenced ```bash blocks — count.
	// The step is allowed to *name* the forbidden commands in prose (it must,
	// to say not to run them); what matters is what a runner following the
	// step would execute.
	var commands []string
	lines := strings.Split(section, "\n")
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			commands = append(commands, line)
		}
	}
	commandBlock := strings.Join(commands, "\n")

	forbidden := []string{"git checkout main", "git switch"}
	for _, f := range forbidden {
		if strings.Contains(commandBlock, f) {
			t.Errorf("%s: branch step runs %q — it must never switch branch", path, f)
		}
	}

	// `git checkout -b` may still appear, but only inside the documented
	// fallback for a checkout that was never assigned a branch at all — never
	// as the unconditional first move.
	if strings.Contains(commandBlock, "git checkout -b") {
		// The step may also *name* the command in prose, earlier, to say not
		// to run it unconditionally — what matters is that the gate sits
		// before the actual fenced command, the last occurrence in the step.
		before := section[:strings.LastIndex(section, "git checkout -b")]
		if !strings.Contains(before, "Only when nothing has assigned a branch") {
			t.Errorf("%s: branch step's `git checkout -b` appears without being gated on \"no branch was assigned\" first", path)
		}
	}

	if !strings.Contains(section, "Defer to the branch you were handed") {
		t.Errorf("%s: branch step does not state the deference explicitly", path)
	}
}

// slashCommand matches a PROMPT that leads with a slash command, e.g.
// "/refine $REF" or "/implement $REF auto" — the adapters treat a leading
// "/" specially (it must lead the message, per agents/claude/refine), so
// this is the same test a running adapter effectively makes.
var slashCommand = regexp.MustCompile(`^/(\S+)`)

// TestExampleSlashCommandsResolveToShippedSkills is the technical note in
// issue #70: every tracked *.example.yaml at the repository root that names a
// slash-command PROMPT must name one this repository actually vendored under
// agents/claude/skills/ — otherwise the config depends on an artefact that
// exists only on one operator's machine, exactly the drift #70 exists to
// close. PROMPT is read from both scripts[].params (which wins) and the
// source's own env:, since a slash command sitting only in env: would
// otherwise escape a test that reads params alone (internal/config/config.go
// layers params over env).
func TestExampleSlashCommandsResolveToShippedSkills(t *testing.T) {
	root := repoRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, "*.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no *.example.yaml found at the repository root")
	}

	checked := 0
	for _, cfgPath := range matches {
		cfgPath := cfgPath
		t.Run(filepath.Base(cfgPath), func(t *testing.T) {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range cfg.Sources {
				for scriptName, spec := range s.Scripts {
					prompt := spec.Params["PROMPT"]
					if prompt == "" {
						prompt = s.Env["PROMPT"]
					}
					m := slashCommand.FindStringSubmatch(prompt)
					if m == nil {
						continue
					}
					checked++
					skill := m[1]
					skillPath := filepath.Join(root, "agents", "claude", "skills", skill, "SKILL.md")
					if _, err := os.Stat(skillPath); err != nil {
						t.Errorf("source %q, script %q: PROMPT names slash command /%s, but %s does not exist", s.Name, scriptName, skill, skillPath)
					}
				}
			}
		})
	}
	if checked == 0 {
		t.Skip("no tracked example config names a slash-command PROMPT yet")
	}
}

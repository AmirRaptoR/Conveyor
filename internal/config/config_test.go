package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// script creates an executable file, making parents as needed.
func script(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// workdir creates the directory a source's scripts run in. It holds no scripts:
// nothing is written into the repository being worked.
func workdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

const stages = `version: 1
stages:
  - name: backlog
  - name: refining
    script: refine
    onSuccess: done
    onFailure: backlog
  - name: done
    terminal: true
  - name: blocked
    terminal: true
sources:
`

// declared is a source providing `refine` from the claude agent.
const declared = `  - name: s1
    provider: github
    workdir: ./repo
    scripts:
      refine:
        agent: claude
`

func write(t *testing.T, dir, sources string) string {
	t.Helper()
	path := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(path, []byte(stages+sources), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// provider lays down a working github provider.
func provider(t *testing.T, dir string) {
	t.Helper()
	script(t, filepath.Join(dir, "providers", "github", "list.sh"))
	script(t, filepath.Join(dir, "providers", "github", "move.sh"))
}

// onboarded is a config dir with a provider, an agent, and a source declaring
// the script its stage asks for.
func onboarded(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	provider(t, dir)
	script(t, filepath.Join(dir, "agents", "claude", "refine"))
	workdir(t, filepath.Join(dir, "repo"))
	return dir, write(t, dir, declared)
}

func TestResolvesProviderAndScripts(t *testing.T) {
	_, path := onboarded(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Sources[0]
	if !s.OK() {
		t.Fatalf("source should be healthy, got problems: %v", s.Problems)
	}
	if got := filepath.Base(s.List); got != "list.sh" {
		t.Errorf("list = %q, want list.sh", got)
	}
	// agent: claude resolves agents/claude/<script name>, the way provider:
	// resolves under providers/.
	if got := s.Paths["refine"]; !strings.HasSuffix(got, "/agents/claude/refine") {
		t.Errorf("refine = %q, want it under agents/claude", got)
	}
}

// An extension is decoration — the runner execs the file directly.
func TestAgentScriptExtensionAgnostic(t *testing.T) {
	for _, name := range []string{"refine", "refine.sh", "refine.py"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			provider(t, dir)
			script(t, filepath.Join(dir, "agents", "claude", name))
			workdir(t, filepath.Join(dir, "repo"))
			cfg, err := Load(write(t, dir, declared))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !cfg.Sources[0].OK() {
				t.Fatalf("problems: %v", cfg.Sources[0].Problems)
			}
		})
	}
}

// script: takes a path, for anything that is not a shipped agent.
func TestExplicitScriptPath(t *testing.T) {
	dir := t.TempDir()
	provider(t, dir)
	script(t, filepath.Join(dir, "mine", "my-refine"))
	workdir(t, filepath.Join(dir, "repo"))
	cfg, err := Load(write(t, dir, `  - name: s1
    provider: github
    workdir: ./repo
    scripts:
      refine:
        script: ./mine/my-refine
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Sources[0].OK() {
		t.Fatalf("problems: %v", cfg.Sources[0].Problems)
	}
	if got := cfg.Sources[0].Paths["refine"]; !strings.HasSuffix(got, "/mine/my-refine") {
		t.Errorf("refine = %q, want the explicit path", got)
	}
}

// The point of the per-source health model: a source that has not been
// onboarded is reported, and every other source keeps working.
func TestUndeclaredScriptIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	provider(t, dir)
	script(t, filepath.Join(dir, "agents", "claude", "refine"))
	workdir(t, filepath.Join(dir, "good"))
	workdir(t, filepath.Join(dir, "bad"))

	cfg, err := Load(write(t, dir, `  - name: good
    provider: github
    workdir: ./good
    scripts:
      refine:
        agent: claude
  - name: bad
    provider: github
    workdir: ./bad
`))
	if err != nil {
		t.Fatalf("an un-onboarded source must not fail the load: %v", err)
	}
	if !cfg.Sources[0].OK() {
		t.Errorf("healthy source dragged down: %v", cfg.Sources[0].Problems)
	}
	if cfg.Sources[1].OK() {
		t.Fatal("source declaring no scripts reported as healthy")
	}
	if !strings.Contains(strings.Join(cfg.Sources[1].Problems, "; "), "does not declare") {
		t.Errorf("problems = %v, want the missing declaration named", cfg.Sources[1].Problems)
	}
}

func TestSourceProblems(t *testing.T) {
	bothForms := `  - name: s1
    provider: github
    workdir: ./repo
    scripts:
      refine:
        agent: claude
        script: ./x
`
	neither := `  - name: s1
    provider: github
    workdir: ./repo
    scripts:
      refine:
        params:
          A: b
`
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, dir string)
		sources string
		want    string
	}{
		{"unknown provider", func(t *testing.T, dir string) {
			script(t, filepath.Join(dir, "agents", "claude", "refine"))
			workdir(t, filepath.Join(dir, "repo"))
		}, declared, `provider "github"`},

		{"unknown agent", func(t *testing.T, dir string) {
			provider(t, dir)
			workdir(t, filepath.Join(dir, "repo"))
		}, declared, `agent "claude"`},

		{"agent lacks the script", func(t *testing.T, dir string) {
			provider(t, dir)
			script(t, filepath.Join(dir, "agents", "claude", "implement"))
			workdir(t, filepath.Join(dir, "repo"))
		}, declared, "no refine script"},

		{"both agent and script", func(t *testing.T, dir string) {
			provider(t, dir)
			workdir(t, filepath.Join(dir, "repo"))
		}, bothForms, "pick one"},

		{"neither agent nor script", func(t *testing.T, dir string) {
			provider(t, dir)
			workdir(t, filepath.Join(dir, "repo"))
		}, neither, "either agent: or script:"},

		{"missing workdir", func(t *testing.T, dir string) {
			provider(t, dir)
			script(t, filepath.Join(dir, "agents", "claude", "refine"))
		}, declared, "workdir"},

		{"not executable", func(t *testing.T, dir string) {
			provider(t, dir)
			workdir(t, filepath.Join(dir, "repo"))
			p := filepath.Join(dir, "agents", "claude", "refine")
			script(t, p)
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatal(err)
			}
		}, declared, "not executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			cfg, err := Load(write(t, dir, tc.sources))
			if err != nil {
				t.Fatalf("must not be fatal: %v", err)
			}
			got := strings.Join(cfg.Sources[0].Problems, "; ")
			if !strings.Contains(got, tc.want) {
				t.Errorf("problems = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// Schema errors stay fatal: they are wrong for every source at once, so there
// is nothing left to degrade to.
func TestSchemaErrorsAreFatal(t *testing.T) {
	for _, tc := range []struct{ name, sources, want string }{
		{"no provider", "  - name: s1\n    workdir: .\n", "provider is required"},
		{"duplicate name", "  - name: s1\n    provider: github\n  - name: s1\n    provider: github\n", "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			provider(t, dir)
			_, err := Load(write(t, dir, tc.sources))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// An inline stage runs the same body for every source, so it needs nothing from
// them and can never make one unusable.
func TestInlineStageNeedsNothingFromSources(t *testing.T) {
	dir := t.TempDir()
	provider(t, dir)
	workdir(t, filepath.Join(dir, "repo"))
	path := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(path, []byte(`version: 1
stages:
  - name: backlog
  - name: announced
    run: |
      #!/bin/sh
      echo hello
    onSuccess: done
    onFailure: done
  - name: done
    terminal: true
  - name: blocked
    terminal: true
sources:
  - name: s1
    provider: github
    workdir: ./repo
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Sources[0].OK() {
		t.Fatalf("inline stage demanded something of the source: %v", cfg.Sources[0].Problems)
	}
}

func TestStageRunRules(t *testing.T) {
	both := "    script: refine\n    run: |\n      #!/bin/sh\n      true\n"
	noShebang := "    run: |\n      echo hello\n"
	for _, tc := range []struct{ name, stage, want string }{
		{"both forms", both, "pick one"},
		// The engine writes the file and execs it; the kernel picks the
		// interpreter from the shebang, and there is none to pick without one.
		{"no shebang", noShebang, "shebang"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			provider(t, dir)
			workdir(t, filepath.Join(dir, "repo"))
			path := filepath.Join(dir, "conveyor.yaml")
			body := "version: 1\nstages:\n  - name: backlog\n  - name: doing\n" + tc.stage +
				"    onSuccess: done\n    onFailure: done\n  - name: done\n    terminal: true\n" +
				"  - name: blocked\n    terminal: true\nsources:\n  - name: s1\n" +
				"    provider: github\n    workdir: ./repo\n"
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestTerminalStageCannotRun(t *testing.T) {
	dir := t.TempDir()
	provider(t, dir)
	path := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(path, []byte(`version: 1
stages:
  - name: backlog
  - name: done
    terminal: true
    script: whatever
sources:
  - name: s1
    provider: github
    workdir: .
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("error = %v, want it to mention terminal", err)
	}
}

// A working config usually lives outside the checkout, so it must be able to
// say where the shipped agents and providers are.
func TestRootsOverridable(t *testing.T) {
	shared := t.TempDir()
	script(t, filepath.Join(shared, "providers", "github", "list.sh"))
	script(t, filepath.Join(shared, "providers", "github", "move.sh"))
	script(t, filepath.Join(shared, "agents", "claude", "refine"))

	elsewhere := t.TempDir()
	workdir(t, filepath.Join(elsewhere, "repo"))
	path := filepath.Join(elsewhere, "conveyor.yaml")
	body := "providers: " + filepath.Join(shared, "providers") + "\n" +
		"agents: " + filepath.Join(shared, "agents") + "\n" + stages + declared
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Sources[0].OK() {
		t.Fatalf("problems: %v", cfg.Sources[0].Problems)
	}
	if !strings.HasPrefix(cfg.Sources[0].Paths["refine"], shared) {
		t.Errorf("refine = %q, want it under %q", cfg.Sources[0].Paths["refine"], shared)
	}
}

func TestRootsDefaultBesideConfig(t *testing.T) {
	dir, path := onboarded(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct{ got, want string }{
		{cfg.AgentsDir(), filepath.Join(dir, "agents")},
		{cfg.ProvidersDir(), filepath.Join(dir, "providers")},
	} {
		if tc.got != tc.want {
			t.Errorf("root = %q, want %q", tc.got, tc.want)
		}
	}
}

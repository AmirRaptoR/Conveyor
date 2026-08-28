package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workdir creates the directory a source's scripts will run in. It holds no
// scripts any more — those live beside the config, in conveyor.d/<source>/.
func workdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

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

const stages = `version: 1
stages:
  - name: backlog
  - name: refining
    work: true
    onSuccess: done
    onFailure: backlog
  - name: done
    terminal: true
  - name: blocked
    terminal: true
sources:
`

func write(t *testing.T, dir, sources string) string {
	t.Helper()
	path := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(path, []byte(stages+sources), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// onboarded lays out a config dir with a working github provider and one repo
// that has its stage scripts in place.
func onboarded(t *testing.T) (dir string, path string) {
	t.Helper()
	dir = t.TempDir()
	script(t, filepath.Join(dir, "providers", "github", "list.sh"))
	script(t, filepath.Join(dir, "providers", "github", "move.sh"))
	script(t, filepath.Join(dir, "conveyor.d", "s1", "refining"))
	workdir(t, filepath.Join(dir, "repo"))
	return dir, write(t, dir, `  - name: s1
    provider: github
    workdir: ./repo
`)
}

func TestResolvesProviderAndStageScripts(t *testing.T) {
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
	// Scripts live beside the config, keyed by source name — nothing is written
	// into the repo being worked, so an agent told to "commit and push" cannot
	// sweep the pipeline into somebody's project.
	if got := s.Scripts["refining"]; !strings.HasSuffix(got, "/conveyor.d/s1/refining") {
		t.Errorf("refining script = %q, want it under conveyor.d/s1", got)
	}
}

// An extension is decoration — the runner execs the file directly.
func TestStageScriptExtensionAgnostic(t *testing.T) {
	for _, name := range []string{"refining", "refining.sh", "refining.py"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			script(t, filepath.Join(dir, "providers", "github", "list.sh"))
			script(t, filepath.Join(dir, "providers", "github", "move.sh"))
			script(t, filepath.Join(dir, "conveyor.d", "s1", name))
			workdir(t, filepath.Join(dir, "repo"))
			cfg, err := Load(write(t, dir, "  - name: s1\n    provider: github\n    workdir: ./repo\n"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !cfg.Sources[0].OK() {
				t.Fatalf("problems: %v", cfg.Sources[0].Problems)
			}
		})
	}
}

// The point of the whole per-source health model: a repo that has not been
// onboarded is reported, and every other repo keeps working.
func TestUnonboardedRepoIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	script(t, filepath.Join(dir, "providers", "github", "list.sh"))
	script(t, filepath.Join(dir, "providers", "github", "move.sh"))
	script(t, filepath.Join(dir, "conveyor.d", "good", "refining"))
	workdir(t, filepath.Join(dir, "good"))
	workdir(t, filepath.Join(dir, "bad")) // exists, but was never onboarded

	cfg, err := Load(write(t, dir, `  - name: good
    provider: github
    workdir: ./good
  - name: bad
    provider: github
    workdir: ./bad
`))
	if err != nil {
		t.Fatalf("an un-onboarded repo must not fail the load: %v", err)
	}
	if !cfg.Sources[0].OK() {
		t.Errorf("healthy source dragged down: %v", cfg.Sources[0].Problems)
	}
	if cfg.Sources[1].OK() {
		t.Fatal("un-onboarded source reported as healthy")
	}
	if !strings.Contains(strings.Join(cfg.Sources[1].Problems, "; "), `stage "refining"`) {
		t.Errorf("problems = %v, want the missing stage named", cfg.Sources[1].Problems)
	}
}

func TestSourceProblems(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"unknown provider", func(t *testing.T, dir string) {
			script(t, filepath.Join(dir, "conveyor.d", "s1", "refining"))
			workdir(t, filepath.Join(dir, "repo"))
		}, `provider "github"`},
		{"provider missing move", func(t *testing.T, dir string) {
			script(t, filepath.Join(dir, "providers", "github", "list.sh"))
			script(t, filepath.Join(dir, "conveyor.d", "s1", "refining"))
			workdir(t, filepath.Join(dir, "repo"))
		}, "no move script"},
		{"ambiguous provider script", func(t *testing.T, dir string) {
			script(t, filepath.Join(dir, "providers", "github", "list.sh"))
			script(t, filepath.Join(dir, "providers", "github", "list.py"))
			script(t, filepath.Join(dir, "providers", "github", "move.sh"))
			script(t, filepath.Join(dir, "conveyor.d", "s1", "refining"))
			workdir(t, filepath.Join(dir, "repo"))
		}, "ambiguous list script"},
		{"missing workdir", func(t *testing.T, dir string) {
			script(t, filepath.Join(dir, "providers", "github", "list.sh"))
			script(t, filepath.Join(dir, "providers", "github", "move.sh"))
		}, "workdir"},
		{"stage script not executable", func(t *testing.T, dir string) {
			script(t, filepath.Join(dir, "providers", "github", "list.sh"))
			script(t, filepath.Join(dir, "providers", "github", "move.sh"))
			workdir(t, filepath.Join(dir, "repo"))
			p := filepath.Join(dir, "conveyor.d", "s1", "refining")
			script(t, p)
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "not executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			cfg, err := Load(write(t, dir, "  - name: s1\n    provider: github\n    workdir: ./repo\n"))
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
			script(t, filepath.Join(dir, "providers", "github", "list.sh"))
			script(t, filepath.Join(dir, "providers", "github", "move.sh"))
			_, err := Load(write(t, dir, tc.sources))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A source naming its two scripts separately is how a GitHub list gets paired
// with an Azure move. KnownFields makes the old schema a loud error rather than
// a silently ignored key.
func TestSeparateScriptPathsRejected(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(write(t, dir, `  - name: s1
    list: ./providers/github/list.sh
    move: ./providers/azure/move.sh
`))
	if err == nil {
		t.Fatal("expected mixed list/move paths to be rejected")
	}
	if !strings.Contains(err.Error(), "list") {
		t.Errorf("error = %v, want it to name the unknown 'list' field", err)
	}
}

// A stage that runs nothing needs no script in any repo, so declaring work is
// what makes onboarding mandatory — and terminal stages can never do work.
func TestTerminalStageCannotWork(t *testing.T) {
	dir := t.TempDir()
	script(t, filepath.Join(dir, "providers", "github", "list.sh"))
	script(t, filepath.Join(dir, "providers", "github", "move.sh"))
	path := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(path, []byte(`version: 1
stages:
  - name: backlog
  - name: done
    terminal: true
    work: true
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

// A working config usually lives outside the conveyor checkout, so it must be
// able to say where providers/ is.
func TestProvidersDirOverride(t *testing.T) {
	shared := t.TempDir()
	script(t, filepath.Join(shared, "providers", "github", "list.sh"))
	script(t, filepath.Join(shared, "providers", "github", "move.sh"))

	elsewhere := t.TempDir()
	script(t, filepath.Join(elsewhere, "conveyor.d", "s1", "refining"))
	workdir(t, filepath.Join(elsewhere, "repo"))
	path := filepath.Join(elsewhere, "conveyor.yaml")
	if err := os.WriteFile(path, []byte("providers: "+filepath.Join(shared, "providers")+"\n"+stages+`  - name: s1
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
		t.Fatalf("problems: %v", cfg.Sources[0].Problems)
	}
	if !strings.HasPrefix(cfg.Sources[0].List, shared) {
		t.Errorf("list = %q, want it under %q", cfg.Sources[0].List, shared)
	}
}

// Without the key, providers/ is beside the config as before.
func TestProvidersDirDefaults(t *testing.T) {
	dir, path := onboarded(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "providers"); cfg.ProvidersDir() != want {
		t.Errorf("ProvidersDir = %q, want %q", cfg.ProvidersDir(), want)
	}
}

// Scripts must not be picked up from the repo being worked: putting them there
// is what lets an agent told to "commit and push" sweep the pipeline into
// somebody's project.
func TestScriptsNotReadFromWorkdir(t *testing.T) {
	dir := t.TempDir()
	script(t, filepath.Join(dir, "providers", "github", "list.sh"))
	script(t, filepath.Join(dir, "providers", "github", "move.sh"))
	// the old location, which must now be ignored
	script(t, filepath.Join(dir, "repo", ".conveyor", "refining"))

	cfg, err := Load(write(t, dir, "  - name: s1\n    provider: github\n    workdir: ./repo\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sources[0].OK() {
		t.Fatal("a script inside the worked repo was accepted; it must be ignored")
	}
}

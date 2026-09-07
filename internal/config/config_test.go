package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
  - name: done
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
  - name: done
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
				"    onSuccess: done\n  - name: done\n    terminal: true\nsources:\n  - name: s1\n" +
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

// provider: accepts a bare name or a block, and its params reach only the
// provider's own scripts — labels are GitHub's vocabulary, and a stage script
// running an agent has no use for them.
func TestProviderBlockAndScoping(t *testing.T) {
	dir := t.TempDir()
	provider(t, dir)
	script(t, filepath.Join(dir, "agents", "claude", "refine"))
	workdir(t, filepath.Join(dir, "repo"))

	cfg, err := Load(write(t, dir, `  - name: s1
    workdir: ./repo
    provider:
      name: github
      params:
        STAGE_LABELS: "refining=status:refining"
    env:
      REPO: owner/repo
    scripts:
      refine:
        agent: claude
        params:
          PROMPT: "do it"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Sources[0]
	if !s.OK() {
		t.Fatalf("problems: %v", s.Problems)
	}
	if s.Provider.Name != "github" {
		t.Errorf("provider name = %q, want github", s.Provider.Name)
	}
	// list and move get the provider's params on top of env
	pe := s.ProviderEnv()
	if pe["STAGE_LABELS"] == "" || pe["REPO"] != "owner/repo" {
		t.Errorf("ProviderEnv = %v, want both STAGE_LABELS and REPO", pe)
	}
	// the source's own env must not have absorbed them
	if _, leaked := s.Env["STAGE_LABELS"]; leaked {
		t.Error("provider params leaked into env, which every stage script sees")
	}
}

// The one-line form stays valid for a provider needing no configuration.
func TestProviderScalarShorthand(t *testing.T) {
	_, path := onboarded(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sources[0].Provider.Name != "github" {
		t.Errorf("provider = %+v, want name github", cfg.Sources[0].Provider)
	}
}

// Blocked stopped being a stage, so a config still routing to one is describing
// a pipeline that does not exist. Naming the key beats ignoring it: KnownFields
// would have called it a typo, and obeying half the routing silently is worse
// than refusing to start.
func TestRemovedRoutesAreNamed(t *testing.T) {
	for _, key := range []string{"onFailure", "onBlocked"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			provider(t, dir)
			script(t, filepath.Join(dir, "agents", "claude", "refine"))
			workdir(t, filepath.Join(dir, "repo"))
			path := filepath.Join(dir, "conveyor.yaml")
			body := "version: 1\nstages:\n  - name: backlog\n  - name: refining\n" +
				"    script: refine\n    onSuccess: done\n    " + key + ": backlog\n" +
				"  - name: done\n    terminal: true\nsources:\n" + declared
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("a removed route loaded without complaint")
			}
			for _, want := range []string{key, "has been removed", "marks the item blocked"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

// An agent is whatever a source's scripts name, never a list of its own: two
// places to record the same fact is two places to get it wrong.
// doctor is a reserved scripts: key that no stage names — resolved on its own
// account, not by the per-stage loop.
func TestDoctorScriptResolves(t *testing.T) {
	dir, path := onboarded(t)
	script(t, filepath.Join(dir, "agents", "claude", "doctor"))
	body := stages + declared + `      doctor:
        agent: claude
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Sources[0]
	if !s.OK() {
		t.Fatalf("source should be healthy, got problems: %v", s.Problems)
	}
	if got := s.Paths["doctor"]; !strings.HasSuffix(got, "/agents/claude/doctor") {
		t.Errorf("doctor = %q, want it under agents/claude", got)
	}
}

// A doctor: naming an agent that does not exist is that source's problem, and
// only that source's.
func TestDoctorScriptBadAgentIsReported(t *testing.T) {
	dir, path := onboarded(t)
	// agents/claude/doctor is deliberately never created.
	body := stages + declared + `      doctor:
        agent: claude
  - name: s2
    provider: github
    workdir: ./repo2
    scripts:
      refine:
        agent: claude
`
	workdir(t, filepath.Join(dir, "repo2"))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s1 := cfg.Sources[0]
	if s1.OK() {
		t.Fatal("source with a broken doctor: reported healthy")
	}
	if !strings.Contains(strings.Join(s1.Problems, "; "), `"doctor"`) {
		t.Errorf("problems = %v, want the doctor script named", s1.Problems)
	}
	if _, set := s1.Paths["doctor"]; set {
		t.Error("doctor should not resolve when its agent script is missing")
	}
	s2 := cfg.Sources[1]
	if !s2.OK() {
		t.Errorf("a broken doctor: in one source dragged down another: %v", s2.Problems)
	}
}

// No doctor: declared is not a problem — it loads clean, and that source's
// marked items are simply left out of a sweep.
func TestNoDoctorScriptLoadsClean(t *testing.T) {
	_, path := onboarded(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Sources[0]
	if !s.OK() {
		t.Fatalf("source should be healthy, got problems: %v", s.Problems)
	}
	if _, set := s.Paths["doctor"]; set {
		t.Error("doctor should not be set when the source declares none")
	}
}

func TestAgentsComeFromTheSourcesThatUseThem(t *testing.T) {
	dir := t.TempDir()
	for _, a := range []string{"claude", "codex"} {
		if err := os.MkdirAll(filepath.Join(dir, "agents", a), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Only one of them can say how it is doing; the other is silent, which is
	// not a misconfiguration.
	if err := os.WriteFile(filepath.Join(dir, "agents", "codex", "status"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Dir: dir, Agents: filepath.Join(dir, "agents"), Sources: []Source{
		{Name: "a", Scripts: map[string]ScriptSpec{
			"refine": {Agent: "claude"}, "review": {Agent: "codex"}}},
		{Name: "b", Scripts: map[string]ScriptSpec{"refine": {Agent: "claude"}}},
		{Name: "c", Scripts: map[string]ScriptSpec{"deploy": {Script: "./ship.sh"}}},
	}}
	got := cfg.AgentsInUse()
	if len(got) != 2 {
		t.Fatalf("agents = %v, want claude and codex once each", got)
	}
	if got[0].Name != "claude" || got[0].Status != "" {
		t.Errorf("claude = %+v, want no status script", got[0])
	}
	if got[1].Name != "codex" || got[1].Status == "" {
		t.Errorf("codex = %+v, want its status script found", got[1])
	}
}

// logsConfig writes a config with an explicit logs: block, distinct from write()
// which always leaves logs unset so defaulting can be exercised on its own.
func logsConfig(t *testing.T, dir, logs string) string {
	t.Helper()
	path := filepath.Join(dir, "conveyor.yaml")
	body := "version: 1\n" + logs + stages[len("version: 1\n"):] + declared
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSweepAtMustBeHHMM(t *testing.T) {
	for _, tc := range []struct{ name, sweepAt string }{
		{"not a time", "logs:\n  sweepAt: soon\n"},
		{"minutes out of range", "logs:\n  sweepAt: \"04:75\"\n"},
		{"hours out of range", "logs:\n  sweepAt: \"25:00\"\n"},
		{"12-hour form", "logs:\n  sweepAt: \"4:00pm\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			provider(t, dir)
			script(t, filepath.Join(dir, "agents", "claude", "refine"))
			workdir(t, filepath.Join(dir, "repo"))
			_, err := Load(logsConfig(t, dir, tc.sweepAt))
			if err == nil || !strings.Contains(err.Error(), "sweepAt") {
				t.Fatalf("error = %v, want it to name sweepAt", err)
			}
		})
	}
}

func TestSweepAtValidPasses(t *testing.T) {
	dir := t.TempDir()
	provider(t, dir)
	script(t, filepath.Join(dir, "agents", "claude", "refine"))
	workdir(t, filepath.Join(dir, "repo"))
	cfg, err := Load(logsConfig(t, dir, "logs:\n  sweepAt: \"23:59\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logs.SweepAt != "23:59" {
		t.Errorf("SweepAt = %q, want 23:59", cfg.Logs.SweepAt)
	}
}

func TestExplicitZeroRetentionIsALoadError(t *testing.T) {
	dir := t.TempDir()
	provider(t, dir)
	script(t, filepath.Join(dir, "agents", "claude", "refine"))
	workdir(t, filepath.Join(dir, "repo"))
	_, err := Load(logsConfig(t, dir, "logs:\n  retention: 0\n"))
	if err == nil || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("error = %v, want it to name retention", err)
	}
}

func TestOmittedRetentionStillDefaultsTo30Days(t *testing.T) {
	dir := t.TempDir()
	provider(t, dir)
	script(t, filepath.Join(dir, "agents", "claude", "refine"))
	workdir(t, filepath.Join(dir, "repo"))
	cfg, err := Load(write(t, dir, declared))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logs.Retention.D() != 30*24*time.Hour {
		t.Errorf("Retention = %v, want 30d default", cfg.Logs.Retention.D())
	}
}

// loadYAML writes a whole config, lays down the provider and agent it names,
// and loads it — for the cases the shared `stages` prefix cannot express.
func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	provider(t, dir)
	script(t, filepath.Join(dir, "agents", "claude", "do"))
	path := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// A resource named on a stage but never given a limit is a load error. The
// alternative is that it silently means "unlimited", which is precisely what
// somebody was trying to prevent by naming it.
func TestAResourceWithNoLimitIsALoadError(t *testing.T) {
	_, err := loadYAML(t, `
version: 1
resources:
  claude: 1
stages:
  - name: work
    script: do
    resources: [claude, codex]
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: s1
    provider: github
    scripts:
      do: {agent: claude}
`)
	if err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("err = %v, want it to name the undeclared resource", err)
	}
}

// A source may say its version of a stage spends something different — the
// same stage is Claude in one repository and Codex in the next — and `[]` says
// it spends nothing, which is not the same as saying nothing at all.
func TestASourceOverridesAStagesResources(t *testing.T) {
	cfg, err := loadYAML(t, `
version: 1
resources:
  claude: 1
  codex: 1
stages:
  - name: work
    script: do
    resources: [claude]
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: uses-stage-default
    provider: github
    scripts:
      do: {agent: claude}
  - name: uses-codex
    provider: github
    scripts:
      do: {agent: claude, resources: [codex]}
  - name: spends-nothing
    provider: github
    scripts:
      do: {agent: claude, resources: []}
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		source string
		want   []string
	}{
		{"uses-stage-default", []string{"claude"}},
		{"uses-codex", []string{"codex"}},
		{"spends-nothing", []string{}},
	} {
		got := cfg.ResourcesFor(c.source, "work")
		if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
			t.Errorf("ResourcesFor(%q) = %v, want %v", c.source, got, c.want)
		}
	}
}

// The route a config actually takes is not the order its stages were written
// in. `onSuccess` unset falls through to the next stage declared, but an
// explicit one may point anywhere — so a stage inserted before `done` while
// the stage above it still says `onSuccess: done` is written into the line and
// never entered by anything.
func TestAStageNothingRoutesIntoIsNotOnTheLine(t *testing.T) {
	cfg, err := loadYAML(t, `
version: 1
stages:
  - name: start
    onSuccess: finish
  - name: skipped
    script: do
    onSuccess: finish
  - name: finish
    terminal: true
sources:
  - name: s1
    provider: github
    scripts:
      do: {agent: claude}
`)
	if err != nil {
		t.Fatal(err)
	}
	reached := map[string]bool{}
	for cur := cfg.Stages[0].Name; cur != "" && !reached[cur]; {
		reached[cur] = true
		st, ok := cfg.Stage(cur)
		if !ok {
			break
		}
		cur = st.OnSuccess
	}
	if reached["skipped"] {
		t.Error("skipped is reachable; the fixture no longer reproduces the bug")
	}
	if !reached["start"] || !reached["finish"] {
		t.Errorf("reached = %v, want the two stages actually on the line", reached)
	}
}

// And the ordinary case: an omitted onSuccess falls through to the next stage
// declared, so a plain list of stages is a line without anyone saying so.
func TestAnOmittedOnSuccessFallsThroughToTheNextStage(t *testing.T) {
	cfg, err := loadYAML(t, `
version: 1
stages:
  - name: one
  - name: two
    script: do
  - name: three
    terminal: true
sources:
  - name: s1
    provider: github
    scripts:
      do: {agent: claude}
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"one", "two"}, {"two", "three"}, {"three", ""}} {
		st, _ := cfg.Stage(c[0])
		if st.OnSuccess != c[1] {
			t.Errorf("%s onSuccess = %q, want %q", c[0], st.OnSuccess, c[1])
		}
	}
}

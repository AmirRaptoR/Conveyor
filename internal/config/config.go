// Package config loads and validates conveyor.yaml. Validation is strict and
// happens once at startup: a stage graph that cannot terminate, or a script that
// does not exist, is a configuration bug and should never become a runtime
// surprise halfway through a pipeline.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version     int         `yaml:"version"`
	Concurrency Concurrency `yaml:"concurrency"`
	Poll        Duration    `yaml:"poll"`
	Timeout     Duration    `yaml:"timeout"`
	// RetryStalled is how often to clear every mark when *every* item is
	// marked and the line cannot move at all. A total stall usually means the
	// outside world broke — an agent over its usage limit, an expired
	// credential — and those fix themselves; a board that stays stopped until
	// someone looks at it does not. Unset means never, and the guard is
	// deliberately "everything": while any item can still move, a mark is a
	// decision and clearing it would just spend an agent to be told again.
	RetryStalled Duration `yaml:"retryStalled"`
	Logs         Logs     `yaml:"logs"`
	Stages       []Stage  `yaml:"stages"`
	Sources      []Source `yaml:"sources"`

	// Agents is where agent folders live, resolved like Providers.
	Agents string `yaml:"agents"`

	// Providers is where provider folders live. Defaults to providers/ beside
	// the config, which is only right when the config sits in the conveyor
	// checkout — a working config usually does not, so it can be pointed
	// anywhere.
	Providers string `yaml:"providers"`

	// Dir is the directory the config was loaded from; relative script paths
	// resolve against it so a config is portable.
	Dir string `yaml:"-"`
}

type Concurrency struct {
	// PerSource is 1 and validated as 1: a source maps to a git worktree, and
	// two agents in one checkout corrupt each other. It is in the schema to be
	// explicit about the constraint, not to be tuned.
	PerSource int `yaml:"perSource"`
	// PerStage bounds how many items occupy one stage at a time. One makes the
	// pipeline behave like a line: a station works a single item while other
	// stations run their own.
	PerStage int `yaml:"perStage"`
	// Global caps the total in flight. Every slot is an agent.
	Global int `yaml:"global"`
}

type Logs struct {
	Retention Duration `yaml:"retention"`
	SweepAt   string   `yaml:"sweepAt"`
}

type Stage struct {
	Name string `yaml:"name"`
	// Script names what runs on entering this stage — a key every source must
	// provide, resolved to conveyor.d/<source>/<script>. Empty means nothing
	// runs and the stage is a queue.
	//
	// It is a name, not a path and not a command: the stage says WHICH script,
	// each source says what that script IS. Two stages may share one name, and
	// a stage may be called something different from the script it runs.
	//
	// This is the only thing a stage says about execution. How to run the work
	// — which agent, which model, which tools — belongs to the source's script,
	// because it differs per repository and the engine must never know.
	Script string `yaml:"script"`
	// Run is a script body written inline in the config, for a stage where
	// every source does the same thing. It cannot vary per source — that is
	// what `script:` is for — so keep it small: anything with real logic in it
	// wants to be a file, where a shell can check it and a person can run it.
	// It must start with a shebang; the engine writes it out and execs it.
	Run     string   `yaml:"run"`
	Timeout Duration `yaml:"timeout"`
	// OnSuccess is the only route out. Everything else leaves the item where it
	// is: a non-zero exit marks it blocked in place, and a mark is not a
	// destination.
	OnSuccess string `yaml:"onSuccess"`
	// MaxAttempts is how many times a failing stage is re-run before the item is
	// marked. Unset means one — the first failure marks it — because a failure
	// that neither routes nor marks would be re-run on every poll forever.
	MaxAttempts int  `yaml:"maxAttempts"`
	Terminal    bool `yaml:"terminal"`

	// Removed, and still parsed so the error can say so. Blocked used to be a
	// stage an item was carried to; it is a mark it wears where it stopped, and
	// a config still routing to a dead-end column would be silently obeyed if
	// these were simply unknown keys.
	OnFailure string `yaml:"onFailure"`
	OnBlocked string `yaml:"onBlocked"`

	// Reserved for v2 and inert in v1: parsed and validated now so adding
	// human-driven moves later is not a schema migration. See DESIGN.md.
	Manual      bool `yaml:"manual"`
	AllowManual bool `yaml:"allowManual"`
}

type Source struct {
	Name     string            `yaml:"name"`
	Provider Provider          `yaml:"provider"`
	Workdir  string            `yaml:"workdir"`
	Env      map[string]string `yaml:"env"`

	// List and Move are resolved from Provider at load time and are never set
	// in YAML. Naming the two scripts independently would let a source pair
	// GitHub's list with Azure's move — a mismatch nothing downstream could
	// detect, because by then they are just two executable paths.
	List string `yaml:"-"`
	Move string `yaml:"-"`

	// Scripts maps stage name -> the absolute script in this source's repo.
	// Problems is why this source cannot run: an un-onboarded repo, a missing
	// provider script. Both are resolved at load and never fatal — one
	// misconfigured repo must not stop every other repo from being worked.
	// Scripts is what this source provides for the names its stages ask for.
	// Declaring them here rather than by convention on disk means onboarding a
	// source is one config block, and which agent runs a stage is visible
	// without opening a file.
	Scripts map[string]ScriptSpec `yaml:"scripts"`

	// Paths is Scripts resolved to absolute files, keyed by script name.
	Paths    map[string]string `yaml:"-"`
	Problems []string          `yaml:"-"`
}

// Provider is how this source reaches its backend. It accepts either a bare
// name or a block with params:
//
//	provider: github
//	provider:
//	  name: github
//	  params:
//	    STAGE_LABELS: |
//	      refining=status:refining
//
// Params reach only list and move. They are the provider's vocabulary — labels
// are a GitHub idea, and a stage script running an agent has no use for them.
type Provider struct {
	Name   string            `yaml:"name"`
	Params map[string]string `yaml:"params"`
}

// UnmarshalYAML accepts the scalar shorthand so a provider needing no
// configuration stays one line.
func (p *Provider) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&p.Name)
	}
	type raw Provider // avoids recursing into this method
	return n.Decode((*raw)(p))
}

// ScriptSpec is one entry in a source's scripts: where the executable is, and
// the parameters it needs. Exactly one of Agent or Script.
type ScriptSpec struct {
	// Agent names a folder under agents/, resolved the same way `provider:`
	// resolves under providers/ — agent: claude finds agents/claude/<name>,
	// where <name> is this entry's key.
	Agent string `yaml:"agent"`
	// Script is an explicit path, for something that is not a shipped agent.
	// Absolute, ~/, or relative to the config file.
	Script string `yaml:"script"`
	// Params reach only this script, layered over the source's env. Per-script
	// rather than per-source because two stages both want to be handed a
	// PROMPT, and at source level the second would overwrite the first.
	Params map[string]string `yaml:"params"`
}

// Duration accepts "30d", "90m", "5m" — Go's ParseDuration has no day unit, and
// a retention window is far more natural in days.
type Duration time.Duration

var dayRe = regexp.MustCompile(`^(\d+)d$`)

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	s := strings.TrimSpace(n.Value)
	if s == "" {
		return nil
	}
	if m := dayRe.FindStringSubmatch(s); m != nil {
		var days int
		if _, err := fmt.Sscanf(m[1], "%d", &days); err != nil {
			return fmt.Errorf("bad duration %q: %w", s, err)
		}
		*d = Duration(time.Duration(days) * 24 * time.Hour)
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Load reads, defaults and validates a config. It returns every problem found,
// not just the first, so one run of `validate` fixes a whole file.
// Load reads a config, resolving providers from the config or the binary.
func Load(path string) (*Config, error) { return LoadFrom(path, "") }

// LoadFrom is Load with an explicit providers root, as passed on the command
// line. It must be applied before resolution, not after: resolution is what
// turns a provider name into script paths.
func LoadFrom(path, providers string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // a typo'd key is an error, not a silent default
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.Dir = filepath.Dir(abs)
	if providers != "" {
		c.Providers = providers
	}
	c.applyDefaults()
	c.resolveSources()
	if errs := c.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("%s is invalid:\n  - %s", path, strings.Join(errs, "\n  - "))
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Concurrency.PerSource == 0 {
		c.Concurrency.PerSource = 1
	}
	if c.Concurrency.Global == 0 {
		c.Concurrency.Global = 1
	}
	if c.Concurrency.PerStage == 0 {
		c.Concurrency.PerStage = 1
	}
	if c.Poll == 0 {
		c.Poll = Duration(5 * time.Minute)
	}
	if c.Timeout == 0 {
		c.Timeout = Duration(90 * time.Minute)
	}
	if c.Logs.Retention == 0 {
		c.Logs.Retention = Duration(30 * 24 * time.Hour)
	}
	if c.Logs.SweepAt == "" {
		c.Logs.SweepAt = "04:00"
	}
	// A stage with no explicit success target advances to the next stage in
	// order; the last stage has nowhere to go and must be terminal.
	for i := range c.Stages {
		s := &c.Stages[i]
		if s.Timeout == 0 {
			s.Timeout = c.Timeout
		}
		if s.OnSuccess == "" && !s.Terminal && i+1 < len(c.Stages) {
			s.OnSuccess = c.Stages[i+1].Name
		}
	}
}

// Stage returns a stage by name.
func (c *Config) Stage(name string) (*Stage, bool) {
	for i := range c.Stages {
		if c.Stages[i].Name == name {
			return &c.Stages[i], true
		}
	}
	return nil, false
}

// runs reports whether entering this stage executes anything.
func (s Stage) runs() bool { return s.Script != "" || s.Run != "" }

// Runs is runs, for callers outside this package.
func (s Stage) Runs() bool { return s.runs() }

// DataDir is where conveyor keeps the little it cannot re-derive by asking the
// sources: run history, and the manual order. Beside the config, because it is
// machine state and not project source.
func (c *Config) DataDir() string { return filepath.Join(c.Dir, "data") }

// StageNames is what gets handed to a list script so a source knows which
// stage names it is allowed to emit.
func (c *Config) StageNames() []string {
	out := make([]string, len(c.Stages))
	for i, s := range c.Stages {
		out[i] = s.Name
	}
	return out
}

// ResolveScript turns a config-relative script path into an absolute one.
func (c *Config) ResolveScript(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// ProvidersDir holds one directory per provider, named exactly as a source's
// `provider:` names it. Adding a provider is creating a folder.
// The layout inside is fixed — <root>/<provider>/<verb> — but the root moves:
// -providers on the command line wins, then `providers:` in the config, then
// the directory holding the binary, which is where a release puts them.
// AgentsDir holds one directory per agent, named as a script's `agent:` names
// it. Resolved exactly like ProvidersDir.
func (c *Config) AgentsDir() string { return c.rootDir(c.Agents, "agents") }

// ProvidersDir holds one directory per provider.
func (c *Config) ProvidersDir() string { return c.rootDir(c.Providers, "providers") }

// rootDir resolves a shipped-asset root: the configured value if set, else the
// directory holding the binary, else the config's own directory — `go run`
// builds into a temp dir, so the last one keeps development working.
func (c *Config) rootDir(configured, name string) string {
	if configured != "" {
		p := expandHome(configured)
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(c.Dir, p)
	}
	if exe, err := os.Executable(); err == nil {
		if beside := filepath.Join(filepath.Dir(exe), name); isDir(beside) {
			return beside
		}
	}
	return filepath.Join(c.Dir, name)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Agent is one agent a source calls, and the status script it provides — the
// optional agents/<name>/status, which reports how that agent is doing.
type Agent struct {
	Name   string `json:"name"`
	Status string `json:"-"` // absolute path; empty when the agent has none
}

// AgentFor names the agent a source runs a stage on, or "" when that stage
// runs a plain script, an inline body, or nothing at all.
//
// The scheduler needs this to know whose quota a transition would spend: a
// usage limit belongs to an agent, not to a source or a stage, and one agent
// is shared by every stage that names it across every repository.
func (c *Config) AgentFor(source, stage string) string {
	st, ok := c.Stage(stage)
	if !ok || st.Script == "" {
		return ""
	}
	for _, src := range c.Sources {
		if src.Name == source {
			return src.Scripts[st.Script].Agent
		}
	}
	return ""
}

// AgentsInUse lists the agents this config's sources actually call.
//
// Derived, never declared: an agent is not a thing you enrol, it is whatever a
// source's script block names. A top-level agents: list would be a second place
// to keep the same fact, and the two would disagree the first time a source
// switched adapters.
func (c *Config) AgentsInUse() []Agent {
	seen := map[string]bool{}
	var out []Agent
	for _, src := range c.Sources {
		for _, spec := range src.Scripts {
			if spec.Agent == "" || seen[spec.Agent] {
				continue
			}
			seen[spec.Agent] = true
			a := Agent{Name: spec.Agent}
			// Optional, and silence is the answer when there is none: an agent
			// that cannot say how it is doing is not a misconfiguration.
			if path, err := findScript(filepath.Join(c.AgentsDir(), spec.Agent), "status"); err == nil {
				a.Status = path
			}
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// providerScript finds providers/<provider>/<verb>.
func (c *Config) providerScript(provider, verb string) (string, error) {
	dir := filepath.Join(c.ProvidersDir(), provider)
	if fi, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("provider %q: %v", provider, err)
	} else if !fi.IsDir() {
		return "", fmt.Errorf("provider %q: %s is not a directory", provider, dir)
	}
	path, err := findScript(dir, verb)
	if err != nil {
		return "", fmt.Errorf("provider %q: %v", provider, err)
	}
	return path, nil
}

// findScript finds dir/<name>, with or without an extension: name.sh, name.py
// and a compiled `name` are equivalent, because the runner execs the file
// directly and never consults an interpreter. Exactly one match is required —
// two would make the choice depend on glob order.
func findScript(dir, name string) (string, error) {
	cand, _ := filepath.Glob(filepath.Join(dir, name+".*"))
	cand = append(cand, filepath.Join(dir, name))

	var found []string
	for _, m := range cand {
		if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
			found = append(found, m)
		}
	}
	sort.Strings(found)

	switch len(found) {
	case 0:
		return "", fmt.Errorf("no %s script in %s", name, dir)
	case 1:
		return found[0], nil
	default:
		names := make([]string, len(found))
		for i, f := range found {
			names[i] = filepath.Base(f)
		}
		return "", fmt.Errorf("ambiguous %s script (%s)", name, strings.Join(names, ", "))
	}
}

// resolveSources fills in each source's script paths and records why it cannot
// run. Nothing here is fatal: onboarding a repo is configuration work, and a
// repo half-way through it must be reported as broken, not crash the engine and
// take every healthy repo down with it.
func (c *Config) resolveSources() {
	for i := range c.Sources {
		s := &c.Sources[i]
		s.Paths = map[string]string{}
		s.Problems = nil
		note := func(f string, a ...any) { s.Problems = append(s.Problems, fmt.Sprintf(f, a...)) }

		if s.Provider.Name != "" {
			// The directory is checked once, not once per verb: a missing
			// provider folder is one problem, and saying it twice buries the
			// real list under noise.
			dir := filepath.Join(c.ProvidersDir(), s.Provider.Name)
			if fi, err := os.Stat(dir); err != nil {
				note("provider %q: %v", s.Provider.Name, err)
			} else if !fi.IsDir() {
				note("provider %q: %s is not a directory", s.Provider.Name, dir)
			} else {
				// Fixed order, not a map: problem lists are read by humans and
				// compared by tests.
				for _, v := range []struct {
					verb string
					dst  *string
				}{{"list", &s.List}, {"move", &s.Move}} {
					path, err := c.providerScript(s.Provider.Name, v.verb)
					if err != nil {
						note("%v", err)
						continue
					}
					if errs := checkScript(path, v.verb+" script"); len(errs) > 0 {
						note("%s", strings.Join(errs, "; "))
						continue
					}
					*v.dst = path
				}
			}
		}

		// The workdir is where scripts run, not where they live, so a missing
		// one no longer hides the script problems — report both.
		if !isDir(c.Workdir(*s)) {
			note("workdir %s: not a directory", c.Workdir(*s))
		}

		for _, st := range c.Stages {
			// An inline stage needs nothing from the source: the same body runs
			// everywhere, so it can never make a source unusable.
			if st.Script == "" {
				continue
			}
			spec, ok := s.Scripts[st.Script]
			if !ok {
				note("stage %q needs a script named %q, which this source does not declare", st.Name, st.Script)
				continue
			}
			path, err := c.resolveScriptSpec(st.Script, spec)
			if err != nil {
				note("script %q: %v", st.Script, err)
				continue
			}
			if errs := checkScript(path, fmt.Sprintf("script %q", st.Script)); len(errs) > 0 {
				note("%s", strings.Join(errs, "; "))
				continue
			}
			s.Paths[st.Script] = path
		}
	}
}

// resolveScriptSpec turns one scripts: entry into an absolute executable.
func (c *Config) resolveScriptSpec(name string, spec ScriptSpec) (string, error) {
	switch {
	case spec.Agent != "" && spec.Script != "":
		return "", fmt.Errorf("has both agent: and script: — pick one")
	case spec.Agent != "":
		dir := filepath.Join(c.AgentsDir(), spec.Agent)
		if !isDir(dir) {
			return "", fmt.Errorf("agent %q: %s is not a directory", spec.Agent, dir)
		}
		path, err := findScript(dir, name)
		if err != nil {
			return "", fmt.Errorf("agent %q: %v", spec.Agent, err)
		}
		return path, nil
	case spec.Script != "":
		p := expandHome(spec.Script)
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.Dir, p)
		}
		return p, nil
	default:
		return "", fmt.Errorf("needs either agent: or script:")
	}
}

// ProviderEnv is what the provider's own scripts get: what the source is, plus
// the provider's params. Stage scripts never see the params — they are the
// provider's vocabulary, not the pipeline's.
func (s Source) ProviderEnv() map[string]string {
	if len(s.Provider.Params) == 0 {
		return s.Env
	}
	out := make(map[string]string, len(s.Env)+len(s.Provider.Params))
	for k, v := range s.Env {
		out[k] = v
	}
	for k, v := range s.Provider.Params {
		out[k] = v
	}
	return out
}

// OK reports whether this source can be worked.
func (s Source) OK() bool { return len(s.Problems) == 0 }

func (c *Config) Validate() []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if c.Version != 1 {
		add("version must be 1, got %d", c.Version)
	}
	if c.Concurrency.PerStage < 1 {
		add("concurrency.perStage must be at least 1, got %d", c.Concurrency.PerStage)
	}
	if c.Concurrency.PerSource < 1 {
		// Above 1 is safe only because an item works in its own git worktree
		// rather than in the source's checkout — see agents/_worktree. Two
		// agents in one directory overwrite each other; two worktrees of one
		// repository share only the object store, which git makes safe.
		add("concurrency.perSource must be at least 1, got %d", c.Concurrency.PerSource)
	}
	if c.Concurrency.Global < 1 {
		add("concurrency.global must be at least 1")
	}

	if len(c.Stages) < 2 {
		add("at least two stages are required, got %d", len(c.Stages))
	}
	seen := map[string]bool{}
	for i, s := range c.Stages {
		switch {
		case s.Name == "":
			add("stages[%d]: name is required", i)
			continue
		case seen[s.Name]:
			add("stage %q: duplicate name", s.Name)
			continue
		}
		seen[s.Name] = true

		if s.Terminal && s.runs() {
			add("stage %q: terminal stages cannot run a script", s.Name)
		}
		if s.Script != "" && s.Run != "" {
			add("stage %q: has both script: and run: — pick one", s.Name)
		}
		if s.Run != "" && !strings.HasPrefix(s.Run, "#!") {
			add("stage %q: run: must start with a shebang; the engine execs it and never picks an interpreter", s.Name)
		}
		if s.MaxAttempts < 0 {
			add("stage %q: maxAttempts cannot be negative", s.Name)
		}
		// A non-terminal stage that runs a script must be able to leave.
		if !s.Terminal && s.runs() && s.OnSuccess == "" {
			add("stage %q: runs a script but has no onSuccess and is not terminal — items would have nowhere to go", s.Name)
		}
		// Named rather than ignored: a config still routing failures to a
		// dead-end column is describing a pipeline that no longer exists, and
		// obeying half of it silently is worse than refusing to start.
		for _, gone := range []struct{ key, was string }{
			{"onFailure", s.OnFailure}, {"onBlocked", s.OnBlocked},
		} {
			if gone.was != "" {
				add("stage %q: %s: has been removed — a non-zero exit now marks the item blocked in the stage it stopped in, and onSuccess is the only route. Delete the key (%s: %s)",
					s.Name, gone.key, gone.key, gone.was)
			}
		}
	}
	// Targets must exist. Checked after the name set is complete so ordering
	// in the file does not matter.
	for _, s := range c.Stages {
		if s.OnSuccess != "" && !seen[s.OnSuccess] {
			add("stage %q: onSuccess points at unknown stage %q", s.Name, s.OnSuccess)
		}
	}

	if len(c.Sources) == 0 {
		add("at least one source is required")
	}
	srcSeen := map[string]bool{}
	for i, s := range c.Sources {
		if s.Name == "" {
			add("sources[%d]: name is required", i)
			continue
		}
		if srcSeen[s.Name] {
			add("source %q: duplicate name", s.Name)
			continue
		}
		srcSeen[s.Name] = true
		if s.Provider.Name == "" {
			add("source %q: provider is required", s.Name)
		}
	}
	return errs
}

// checkScript catches the two failures that would otherwise only appear
// mid-pipeline: the file is missing, or it is not executable.
func checkScript(path, what string) []string {
	fi, err := os.Stat(path)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", what, err)}
	}
	if fi.IsDir() {
		return []string{fmt.Sprintf("%s: %s is a directory", what, path)}
	}
	if fi.Mode()&0o111 == 0 {
		return []string{fmt.Sprintf("%s: %s is not executable (chmod +x)", what, path)}
	}
	return nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// Workdir returns the source's working directory, absolute and home-expanded.
func (c *Config) Workdir(s Source) string {
	if s.Workdir == "" {
		return c.Dir
	}
	p := expandHome(s.Workdir)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

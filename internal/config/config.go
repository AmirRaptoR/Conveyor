// Package config loads and validates agent-team.yaml. Validation is strict and
// happens once at startup: a stage graph that cannot terminate, or a script that
// does not exist, is a configuration bug and should never become a runtime
// surprise halfway through a pipeline.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version     int         `yaml:"version"`
	Concurrency Concurrency `yaml:"concurrency"`
	Poll        Duration    `yaml:"poll"`
	Timeout     Duration    `yaml:"timeout"`
	Logs        Logs        `yaml:"logs"`
	Stages      []Stage     `yaml:"stages"`
	Sources     []Source    `yaml:"sources"`

	// Dir is the directory the config was loaded from; relative script paths
	// resolve against it so a config is portable.
	Dir string `yaml:"-"`
}

type Concurrency struct {
	PerSource int `yaml:"perSource"`
	Global    int `yaml:"global"`
}

type Logs struct {
	Retention Duration `yaml:"retention"`
	SweepAt   string   `yaml:"sweepAt"`
}

type Stage struct {
	Name        string   `yaml:"name"`
	OnEnter     string   `yaml:"onEnter"`
	Timeout     Duration `yaml:"timeout"`
	OnSuccess   string   `yaml:"onSuccess"`
	OnFailure   string   `yaml:"onFailure"`
	OnBlocked   string   `yaml:"onBlocked"`
	MaxAttempts int      `yaml:"maxAttempts"`
	Terminal    bool     `yaml:"terminal"`

	// Reserved for v2 and inert in v1: parsed and validated now so adding
	// human-driven moves later is not a schema migration. See DESIGN.md.
	Manual      bool `yaml:"manual"`
	AllowManual bool `yaml:"allowManual"`
}

type Source struct {
	Name    string            `yaml:"name"`
	Workdir string            `yaml:"workdir"`
	List    string            `yaml:"list"`
	Move    string            `yaml:"move"`
	Env     map[string]string `yaml:"env"`
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
func Load(path string) (*Config, error) {
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
	c.applyDefaults()
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
	hasBlocked := false
	for _, s := range c.Stages {
		if s.Name == "blocked" {
			hasBlocked = true
		}
	}
	for i := range c.Stages {
		s := &c.Stages[i]
		if s.Timeout == 0 {
			s.Timeout = c.Timeout
		}
		if s.OnSuccess == "" && !s.Terminal && i+1 < len(c.Stages) {
			s.OnSuccess = c.Stages[i+1].Name
		}
		// A blocked item must always leave the stage it blocked in. Staying
		// would mean the scheduler re-runs the same script on the next poll,
		// which is the exact infinite loop this design exists to avoid.
		if s.OnBlocked == "" && s.OnEnter != "" && hasBlocked {
			s.OnBlocked = "blocked"
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

func (c *Config) Validate() []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if c.Version != 1 {
		add("version must be 1, got %d", c.Version)
	}
	if c.Concurrency.PerSource != 1 {
		// Not a style preference: a source maps to a git worktree, and two
		// agents in one checkout corrupt each other.
		add("concurrency.perSource must be 1 in v1 (got %d)", c.Concurrency.PerSource)
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

		if s.Terminal && s.OnEnter != "" {
			add("stage %q: terminal stages cannot have onEnter", s.Name)
		}
		if s.OnEnter != "" {
			errs = append(errs, checkScript(c.ResolveScript(s.OnEnter), fmt.Sprintf("stage %q onEnter", s.Name))...)
		}
		if s.MaxAttempts < 0 {
			add("stage %q: maxAttempts cannot be negative", s.Name)
		}
		// A non-terminal stage that runs a script must be able to leave.
		if !s.Terminal && s.OnEnter != "" && s.OnSuccess == "" {
			add("stage %q: has onEnter but no onSuccess and is not terminal — items would have nowhere to go", s.Name)
		}
		// Without a blocked target the item stays put and the scheduler runs
		// the same script again on the next poll.
		if s.OnEnter != "" && s.OnBlocked == "" {
			add("stage %q: has onEnter but no onBlocked, and there is no stage named \"blocked\" to default to — a blocked item would be re-run forever", s.Name)
		}
	}
	// Targets must exist. Checked after the name set is complete so ordering
	// in the file does not matter.
	for _, s := range c.Stages {
		for label, target := range map[string]string{
			"onSuccess": s.OnSuccess, "onFailure": s.OnFailure, "onBlocked": s.OnBlocked,
		} {
			if target != "" && !seen[target] {
				add("stage %q: %s points at unknown stage %q", s.Name, label, target)
			}
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
		if s.List == "" {
			add("source %q: list script is required", s.Name)
		} else {
			errs = append(errs, checkScript(c.ResolveScript(s.List), fmt.Sprintf("source %q list", s.Name))...)
		}
		if s.Move == "" {
			add("source %q: move script is required", s.Name)
		} else {
			errs = append(errs, checkScript(c.ResolveScript(s.Move), fmt.Sprintf("source %q move", s.Name))...)
		}
		if s.Workdir != "" {
			if _, err := os.Stat(expandHome(s.Workdir)); err != nil {
				add("source %q: workdir %s: %v", s.Name, s.Workdir, err)
			}
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

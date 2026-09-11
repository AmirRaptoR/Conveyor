// Package enroll holds the shape of a provider's source.template.yaml and
// the logic for turning it — plus a source name, a workdir and a set of
// answers — into the sources: block conveyor enroll prints. See
// docs/CONTRACTS.md.
//
// This package never touches stdin or stdout: it is handed answers and
// writers by its caller, cmd/conveyor, so a Go test can drive the whole
// substitution and validation logic without a terminal.
package enroll

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Reserved names the engine itself supplies — the source name, its workdir,
// and the rendered scripts: block. A provider's own prompt claiming one of
// these is a template error: the engine's answer for it would either
// collide with or silently shadow the provider's own question.
const (
	NameSource  = "SOURCE"
	NameWorkdir = "WORKDIR"
	NameScripts = "SCRIPTS"
)

var reserved = map[string]bool{NameSource: true, NameWorkdir: true, NameScripts: true}

// promptNameRe is what a provider's own prompt may be called. It becomes
// part of a placeholder ({{NAME}}) and an -answer flag's key, so it is kept
// to the boring subset that needs no quoting anywhere — the same reasoning
// config.nameRe applies to an action's name.
var promptNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// secretRe catches a prompt name shaped like a credential — case
// insensitively, as a substring, so TOKEN, GH_TOKEN and API_KEY all match.
// enroll refuses to collect anything matching it: everything it collects it
// prints, on stdout, in the clear, and a secret belongs in a source's env:
// where a person can type it once and it is never echoed anywhere.
var secretRe = regexp.MustCompile(`(?i)token|secret|password|key|credential`)

// placeholderRe matches one {{NAME}} placeholder anywhere in a line.
var placeholderRe = regexp.MustCompile(`\{\{([A-Z][A-Z0-9_]*)\}\}`)

// anyPlaceholderRe is broader — used only to detect what is left over after
// a real substitution pass, including a name outside the usual shape, which
// must never be silently ignored.
var anyPlaceholderRe = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// Prompt is one question a provider's source.template.yaml asks, beyond the
// source name and workdir the engine already asks for itself.
type Prompt struct {
	Name    string `yaml:"name"`
	Prompt  string `yaml:"prompt"`
	Example string `yaml:"example"`
	Default string `yaml:"default"`
}

// Template is one provider's providers/<name>/source.template.yaml: the
// questions it asks, and the sources: entry — with {{NAME}} placeholders —
// they fill in.
//
// Deliberately not a resolvable verb: provider: resolves only list, move
// and (optionally) preflight, so a template file can never be mistaken for
// a script — the same care onboard.sh and selfcheck.sh already document for
// their own non-verb names.
type Template struct {
	Prompts  []Prompt `yaml:"prompts"`
	Template string   `yaml:"template"`
}

// LoadTemplate reads and statically validates path. Every failure is a
// refusal naming the file and the offending entry: a malformed template
// must never produce a partial draft, because a person pasting half of one
// into their config would not know what was missing.
func LoadTemplate(path string) (*Template, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var t Template
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if strings.TrimSpace(t.Template) == "" {
		return nil, fmt.Errorf("%s: missing template:", path)
	}

	seen := map[string]bool{}
	for _, p := range t.Prompts {
		switch {
		case !promptNameRe.MatchString(p.Name):
			return nil, fmt.Errorf("%s: prompt %q: name must match %s", path, p.Name, promptNameRe.String())
		case reserved[p.Name]:
			return nil, fmt.Errorf("%s: prompt %q: %s is reserved for the engine's own question", path, p.Name, p.Name)
		case seen[p.Name]:
			return nil, fmt.Errorf("%s: prompt %q: declared twice", path, p.Name)
		case secretRe.MatchString(p.Name):
			return nil, fmt.Errorf(
				"%s: prompt %q looks like it collects a secret — enroll prints everything it collects, "+
					"on stdout, in the clear, so this belongs in the source's env: by hand instead", path, p.Name)
		case !strings.Contains(t.Template, "{{"+p.Name+"}}"):
			return nil, fmt.Errorf("%s: prompt %q never appears in template:", path, p.Name)
		}
		seen[p.Name] = true
	}
	return &t, nil
}

// ScriptChoice is which agent: or script: a stage's script name resolves to
// in the drafted source — one per distinct script name any stage requires,
// plus doctor when the operator opts into it.
type ScriptChoice struct {
	Name   string
	Agent  string
	Script string
}

// BuildScripts renders the scripts: block's children for the given choices,
// in the order given: each is a two-space-indented key carrying either
// agent: or script:, exactly the shape the rest of the config already
// writes by hand. This is raw YAML text, not a value to be scalar-encoded —
// it is what {{SCRIPTS}} substitutes to, indented by Fill to whatever
// column the placeholder itself sits at.
func BuildScripts(choices []ScriptChoice) string {
	var b strings.Builder
	for i, c := range choices {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s:\n", c.Name)
		if c.Agent != "" {
			fmt.Fprintf(&b, "  agent: %s", c.Agent)
		} else {
			fmt.Fprintf(&b, "  script: %s", c.Script)
		}
	}
	return b.String()
}

// ParseScriptChoice reads an answer to "which agent: or script: provides
// this" — "agent:NAME" or "script:PATH" — into the two fields BuildScripts
// renders. Anything else is an error naming the two forms it accepts.
func ParseScriptChoice(name, answer string) (ScriptChoice, error) {
	switch {
	case strings.HasPrefix(answer, "agent:"):
		v := strings.TrimSpace(strings.TrimPrefix(answer, "agent:"))
		if v == "" {
			return ScriptChoice{}, fmt.Errorf("%q: agent: needs a name", name)
		}
		return ScriptChoice{Name: name, Agent: v}, nil
	case strings.HasPrefix(answer, "script:"):
		v := strings.TrimSpace(strings.TrimPrefix(answer, "script:"))
		if v == "" {
			return ScriptChoice{}, fmt.Errorf("%q: script: needs a path", name)
		}
		return ScriptChoice{Name: name, Script: v}, nil
	default:
		return ScriptChoice{}, fmt.Errorf("%q: want agent:<name> or script:<path>, got %q", name, answer)
	}
}

// encodeScalar turns an arbitrary string into a YAML-safe scalar — always
// double-quoted, so it never spans more than one physical line for any
// realistic prompt answer and always round-trips back to exactly v. Built by
// hand rather than left to yaml.Marshal's own style heuristics, which choose
// a literal block style for a string containing a newline — exactly the
// shape that would break {{NAME}} substitution mid-line.
func encodeScalar(v string) (string, error) {
	n := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
	b, err := yaml.Marshal(&n)
	if err != nil {
		return "", fmt.Errorf("encoding %q as YAML: %w", v, err)
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// indentBlock prefixes every line of text with indent — what a multi-line
// raw value (SCRIPTS) needs when its placeholder sits at some column other
// than zero.
func indentBlock(text, indent string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}
	return strings.Join(lines, "\n")
}

// wholeLinePlaceholder reports the single placeholder name when line
// consists of nothing but leading whitespace followed by exactly one
// {{NAME}} and nothing else — the shape that lets {{SCRIPTS}} carry a
// multi-line value indented to its own column, as opposed to one substituted
// inline alongside other text.
func wholeLinePlaceholder(line string) (indent, name string, ok bool) {
	trimmed := strings.TrimLeft(line, " ")
	indent = line[:len(line)-len(trimmed)]
	m := placeholderRe.FindStringSubmatch(trimmed)
	if m == nil || m[0] != trimmed {
		return "", "", false
	}
	return indent, m[1], true
}

// Fill substitutes SOURCE, WORKDIR, SCRIPTS and the template's own prompt
// answers into tmpl.Template. Substitution is single-pass and never
// recursive: it walks the original template text exactly once, so a value
// containing literal {{X}} text is emitted as-is rather than substituted a
// second time — and, just as importantly, is never mistaken for a
// placeholder left unresolved: every check for "did this placeholder
// resolve" runs against the original template line, before substitution,
// never against the finished output, which may legitimately contain
// {{-shaped text that a person typed as an answer.
//
// scripts is BuildScripts's own raw output and is inserted verbatim,
// indented to whatever column its placeholder sits at on its own line;
// source, workdir and every entry in answers are ordinary prompt answers,
// each encoded as a YAML-safe scalar before being spliced in.
//
// Returns an error naming the first placeholder still unresolved — either
// outside the usual {{NAME}} shape, or a name with no value — a template
// error LoadTemplate's own static checks could not have caught, kept here as
// a hard guard against ever printing a partial draft.
func Fill(tmpl *Template, source, workdir, scripts string, answers map[string]string) (string, error) {
	values := map[string]string{NameSource: source, NameWorkdir: workdir}
	for k, v := range answers {
		values[k] = v
	}

	lines := strings.Split(tmpl.Template, "\n")
	for i, line := range lines {
		if indent, name, ok := wholeLinePlaceholder(line); ok && name == NameScripts {
			lines[i] = indentBlock(scripts, indent)
			continue
		}

		// Validate every {{...}} in the ORIGINAL line before substituting
		// anything on it: a malformed shape, or a name with no value, is
		// unresolved by this pass. Checked here rather than by re-scanning
		// the substituted line, because a substituted value can legitimately
		// contain literal {{X}} text that must never trip this check.
		for _, m := range anyPlaceholderRe.FindAllString(line, -1) {
			sub := placeholderRe.FindStringSubmatch(m)
			if sub == nil {
				return "", fmt.Errorf("unresolved placeholder %s left after substitution", m)
			}
			if name := sub[1]; name != NameScripts {
				if _, ok := values[name]; !ok {
					return "", fmt.Errorf("unresolved placeholder %s left after substitution", m)
				}
			}
		}

		var encErr error
		lines[i] = placeholderRe.ReplaceAllStringFunc(line, func(m string) string {
			name := placeholderRe.FindStringSubmatch(m)[1]
			if name == NameScripts {
				// SCRIPTS used inline rather than on its own line: still
				// substituted, just without the indent treatment above.
				return scripts
			}
			enc, err := encodeScalar(values[name]) // presence already checked above
			if err != nil {
				encErr = err
				return m
			}
			return enc
		})
		if encErr != nil {
			return "", encErr
		}
	}
	return strings.Join(lines, "\n"), nil
}

// ReservedNames lists the names a provider prompt must not claim, sorted —
// for an error message that names all of them.
func ReservedNames() []string {
	out := make([]string, 0, len(reserved))
	for n := range reserved {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

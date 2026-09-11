package enroll

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// sourceNameRe is what a plausible source name looks like: no whitespace, no
// YAML-breaking punctuation, nothing that would need quoting once it lands
// in a sources: block.
var sourceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ValidateSourceName reports why name cannot be enrolled: not a plausible
// name, or already used by a source the loaded config carries. Checked
// before any other prompt is answered, so a doomed enrollment fails at the
// very first question rather than after a person has typed in a provider's
// worth of answers.
func ValidateSourceName(name string, existing []string) error {
	if !sourceNameRe.MatchString(name) {
		return fmt.Errorf("%q is not a plausible source name (want %s)", name, sourceNameRe.String())
	}
	for _, e := range existing {
		if e == name {
			return fmt.Errorf("a source named %q already exists in this config", name)
		}
	}
	return nil
}

// Asker asks one question at a time, consuming Answers (typically from
// repeated -answer NAME=VALUE flags) before ever touching In — so a fully
// scripted invocation with every question covered reads no stdin at all,
// which is what makes an enroll run testable without a terminal.
type Asker struct {
	// Answers is consumed as it is used: a name deleted from this map has
	// been asked and answered. Whatever remains once a flow finishes is an
	// -answer nobody asked for — the caller's job to report as an error.
	Answers map[string]string
	// In is where an unanswered question is read from. nil means there is
	// no interactive fallback at all: every question must arrive via
	// Answers, and anything else is an immediate error naming the flag that
	// would have supplied it.
	In *bufio.Reader
	// Out is where the prompt itself, its example and its note are
	// written — never Answers, never the value once read back, so this is
	// safe to point at stderr even for a prompt that turns out to hold
	// something sensitive.
	Out io.Writer
}

// Ask returns the answer to one named question: prompt is what is shown,
// example is an optional "(e.g. …)" suffix, and def is what an empty
// interactive answer takes — re-asking instead when def is "".
func (a *Asker) Ask(name, prompt, example, def string) (string, error) {
	if v, ok := a.Answers[name]; ok {
		delete(a.Answers, name)
		return v, nil
	}
	for {
		msg := prompt
		if example != "" {
			msg += fmt.Sprintf(" (e.g. %s)", example)
		}
		if def != "" {
			msg += fmt.Sprintf(" [%s]", def)
		}
		fmt.Fprintf(a.Out, "%s: ", msg)

		if a.In == nil {
			return "", fmt.Errorf("no answer for %q and no terminal to ask on (use -answer %s=...)", name, name)
		}
		line, err := a.In.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			// A piped stream's last line, with no trailing newline, still
			// reads as an EOF-bearing ReadString — and still counts as an
			// answer.
			return line, nil
		}
		if def != "" {
			return def, nil
		}
		if err != nil {
			// EOF (or another read failure) with nothing left to fall back
			// on: re-asking would only hit the same EOF again.
			return "", fmt.Errorf("no answer for %q: %w", name, err)
		}
		fmt.Fprintf(a.Out, "  %q needs an answer\n", name)
	}
}

// AskBool is Ask for a yes/no question, read as y/yes/n/no/true/false
// (case-insensitive); an empty answer takes def. Anything else re-asks
// (interactively) or is an error (from -answer, which gets one shot).
func (a *Asker) AskBool(name, prompt string, def bool) (bool, error) {
	defStr := map[bool]string{true: "y", false: "n"}[def]
	for {
		s, err := a.Ask(name, prompt, "", defStr)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "y", "yes", "true":
			return true, nil
		case "n", "no", "false":
			return false, nil
		}
		if a.In == nil {
			return false, fmt.Errorf("%q must be yes or no, got %q", name, s)
		}
		fmt.Fprintf(a.Out, "  please answer yes or no\n")
	}
}

// Leftover reports the -answer names nobody asked for, sorted — an unknown
// name is always an error, never silently ignored.
func (a *Asker) Leftover() []string {
	if len(a.Answers) == 0 {
		return nil
	}
	out := make([]string, 0, len(a.Answers))
	for k := range a.Answers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

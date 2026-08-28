package server

import "testing"

// The runner writes "<time> <stream> <text>", and the text is whatever a script
// emitted — including timestamps, spaces and colons of its own. Splitting more
// than twice would corrupt somebody's output.
func TestParseLogKeepsTextIntact(t *testing.T) {
	in := "15:36:20.076 stdout refining midgame #60 — marked cell not visible\n" +
		"15:36:21.001 stderr blocked: needs a product decision\n" +
		"15:36:22.500 stdout   · Bash: gh issue view 60 --repo o/r\n"
	got := parseLog(in)

	if len(got) != 3 {
		t.Fatalf("parsed %d lines, want 3", len(got))
	}
	for i, want := range []struct{ stream, text string }{
		{"stdout", "refining midgame #60 — marked cell not visible"},
		{"stderr", "blocked: needs a product decision"},
		{"stdout", "  · Bash: gh issue view 60 --repo o/r"},
	} {
		if got[i].Stream != want.stream || got[i].Text != want.text {
			t.Errorf("line %d = (%q, %q), want (%q, %q)",
				i, got[i].Stream, got[i].Text, want.stream, want.text)
		}
	}
}

// A blank line is spacing an agent chose; dropping it runs its paragraphs
// together.
func TestParseLogKeepsBlankLines(t *testing.T) {
	got := parseLog("15:36:20.076 stdout one\n\n15:36:20.080 stdout two\n")
	if len(got) != 3 || got[1].Text != "" {
		t.Fatalf("parsed %#v, want a blank line between the two", got)
	}
}

// A line that does not match the format is still somebody's output.
func TestParseLogToleratesOddLines(t *testing.T) {
	got := parseLog("no-timestamp-here\n")
	if len(got) != 1 || got[0].Text != "no-timestamp-here" {
		t.Fatalf("parsed %#v, want the line kept verbatim", got)
	}
}

func TestParseLogEmpty(t *testing.T) {
	if got := parseLog(""); len(got) != 1 || got[0].Text != "" {
		t.Fatalf("parsed %#v, want a single empty line", got)
	}
}

package enroll

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestValidateSourceName(t *testing.T) {
	cases := []struct {
		name     string
		existing []string
		wantErr  bool
	}{
		{"midgame", nil, false},
		{"my-repo_2", nil, false},
		{"", nil, true},
		{"has spaces", nil, true},
		{"has:colon", nil, true},
		{"mock", []string{"mock", "other"}, true},
		{"fresh", []string{"mock", "other"}, false},
	}
	for _, tc := range cases {
		err := ValidateSourceName(tc.name, tc.existing)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateSourceName(%q, %v) error = %v, wantErr %v", tc.name, tc.existing, err, tc.wantErr)
		}
	}
}

// Ask prefers an -answer over the terminal: a fully scripted invocation with
// every question covered must read no stdin at all.
func TestAskPrefersAnswer(t *testing.T) {
	a := &Asker{Answers: map[string]string{"NAME": "value"}, Out: &bytes.Buffer{}}
	got, err := a.Ask("NAME", "prompt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "value" {
		t.Errorf("got %q, want %q", got, "value")
	}
	if _, ok := a.Answers["NAME"]; ok {
		t.Error("answer was not consumed")
	}
}

// With no answer and no terminal (In nil), Ask fails immediately rather than
// hanging — this is what makes -answer NAME=VALUE alone enough to run the
// whole flow with stdin closed.
func TestAskNoAnswerNoTerminal(t *testing.T) {
	a := &Asker{Out: &bytes.Buffer{}}
	_, err := a.Ask("NAME", "prompt", "", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "NAME") {
		t.Errorf("error %q should name the prompt", err)
	}
}

// An empty interactive answer takes the default when there is one.
func TestAskEmptyTakesDefault(t *testing.T) {
	a := &Asker{In: bufio.NewReader(strings.NewReader("\n")), Out: &bytes.Buffer{}}
	got, err := a.Ask("NAME", "prompt", "", "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if got != "fallback" {
		t.Errorf("got %q, want default %q", got, "fallback")
	}
}

// An empty interactive answer with no default re-asks; EOF at that point is
// an error naming the prompt.
func TestAskEmptyNoDefaultReAsksThenEOF(t *testing.T) {
	a := &Asker{In: bufio.NewReader(strings.NewReader("\n")), Out: &bytes.Buffer{}}
	_, err := a.Ask("NAME", "prompt", "", "")
	if err == nil {
		t.Fatal("expected an error on EOF with nothing answered")
	}
	if !strings.Contains(err.Error(), "NAME") {
		t.Errorf("error %q should name the prompt", err)
	}
}

func TestAskBool(t *testing.T) {
	cases := []struct {
		in   string
		def  bool
		want bool
	}{
		{"y\n", false, true},
		{"yes\n", false, true},
		{"n\n", true, false},
		{"no\n", true, false},
		{"\n", true, true},   // empty takes default
		{"\n", false, false}, // empty takes default
	}
	for _, tc := range cases {
		a := &Asker{In: bufio.NewReader(strings.NewReader(tc.in)), Out: &bytes.Buffer{}}
		got, err := a.AskBool("NAME", "prompt", tc.def)
		if err != nil {
			t.Fatalf("AskBool(%q, def=%v): %v", tc.in, tc.def, err)
		}
		if got != tc.want {
			t.Errorf("AskBool(%q, def=%v) = %v, want %v", tc.in, tc.def, got, tc.want)
		}
	}
}

// An -answer that is neither yes nor no is an error, not a re-ask: -answer
// gets one shot.
func TestAskBoolBadAnswerFromFlag(t *testing.T) {
	a := &Asker{Answers: map[string]string{"NAME": "maybe"}, Out: &bytes.Buffer{}}
	if _, err := a.AskBool("NAME", "prompt", false); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLeftover(t *testing.T) {
	a := &Asker{Answers: map[string]string{"B": "1", "A": "2"}, Out: &bytes.Buffer{}}
	got := a.Leftover()
	want := []string{"A", "B"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Leftover() = %v, want %v", got, want)
	}

	a2 := &Asker{Out: &bytes.Buffer{}}
	if got := a2.Leftover(); got != nil {
		t.Errorf("Leftover() = %v, want nil for no answers", got)
	}
}

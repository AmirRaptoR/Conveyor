package preflight

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func TestNormalizeStatus(t *testing.T) {
	cases := map[string]Status{
		"pass": StatusPass, "fail": StatusFail, "warn": StatusWarn, "skip": StatusSkip,
		"":        StatusUnknown,
		"PASS":    StatusUnknown, // case-sensitive: the vocabulary is exact words
		"success": StatusUnknown,
		"ok":      StatusUnknown,
	}
	for in, want := range cases {
		if got := NormalizeStatus(in); got != want {
			t.Errorf("NormalizeStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusFailed(t *testing.T) {
	failing := []Status{StatusFail, StatusUnknown}
	ok := []Status{StatusPass, StatusWarn, StatusSkip}
	for _, s := range failing {
		if !s.Failed() {
			t.Errorf("%q.Failed() = false, want true", s)
		}
	}
	for _, s := range ok {
		if s.Failed() {
			t.Errorf("%q.Failed() = true, want false", s)
		}
	}
}

func TestParseChecksWellFormed(t *testing.T) {
	data := []byte(`{"checks":[
		{"name":"gh on PATH","status":"pass"},
		{"name":"labels","status":"fail","detail":"missing conveyor:blocked","fix":"run onboard.sh"}
	]}`)
	checks, ok := ParseChecks(data)
	if !ok {
		t.Fatal("ParseChecks reported not ok for a well-formed envelope")
	}
	if len(checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(checks))
	}
	if checks[0].Name != "gh on PATH" || checks[0].Status != StatusPass {
		t.Errorf("checks[0] = %+v", checks[0])
	}
	if checks[1].Status != StatusFail || checks[1].Fix != "run onboard.sh" {
		t.Errorf("checks[1] = %+v", checks[1])
	}
}

func TestParseChecksMalformedEnvelope(t *testing.T) {
	cases := map[string][]byte{
		"not JSON":            []byte("not json at all"),
		"not an object":       []byte(`[1,2,3]`),
		"no checks key":       []byte(`{"foo":"bar"}`),
		"checks not an array": []byte(`{"checks":"nope"}`),
		"empty":               []byte(``),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			checks, ok := ParseChecks(data)
			if ok {
				t.Errorf("ParseChecks(%q) reported ok, want malformed", data)
			}
			if checks != nil {
				t.Errorf("ParseChecks(%q) returned checks %v, want nil", data, checks)
			}
		})
	}
}

// A malformed *entry* inside a well-formed array must not erase the checks
// that did come back — it becomes one unknown check named by its index.
func TestParseChecksMalformedEntry(t *testing.T) {
	data := []byte(`{"checks":[
		{"name":"first","status":"pass"},
		"not an object",
		{"name":"third","status":"fail"}
	]}`)
	checks, ok := ParseChecks(data)
	if !ok {
		t.Fatal("ParseChecks reported not ok")
	}
	if len(checks) != 3 {
		t.Fatalf("got %d checks, want 3 (one bad entry must not erase the others): %+v", len(checks), checks)
	}
	if checks[0].Status != StatusPass {
		t.Errorf("checks[0] = %+v", checks[0])
	}
	if checks[1].Status != StatusUnknown || checks[1].Name != "check 1" {
		t.Errorf("checks[1] = %+v, want unknown named by its index", checks[1])
	}
	if checks[2].Status != StatusFail {
		t.Errorf("checks[2] = %+v", checks[2])
	}
}

func TestParseChecksEntryWithNoName(t *testing.T) {
	data := []byte(`{"checks":[{"status":"pass"}]}`)
	checks, ok := ParseChecks(data)
	if !ok || len(checks) != 1 {
		t.Fatalf("ParseChecks = %v, %v", checks, ok)
	}
	if checks[0].Name != "check 0" {
		t.Errorf("Name = %q, want the index-derived name", checks[0].Name)
	}
}

func TestFromRunNonZeroExit(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeFailure, ExitCode: 3, Dir: "/data/runs/x"}
	checks := FromRun(run, nil)
	if len(checks) != 1 || checks[0].Status != StatusFail {
		t.Fatalf("FromRun(non-zero exit) = %+v", checks)
	}
	if !contains(checks[0].Detail, "/data/runs/x") {
		t.Errorf("Detail = %q, want it to name the run directory", checks[0].Detail)
	}
}

func TestFromRunTimeout(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeTimeout, TimedOut: true, Dir: "/data/runs/x"}
	checks := FromRun(run, nil)
	if len(checks) != 1 || checks[0].Status != StatusFail || !contains(checks[0].Detail, "timed out") {
		t.Fatalf("FromRun(timeout) = %+v", checks)
	}
}

func TestFromRunMalformedEnvelopeOnSuccess(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeSuccess, Dir: "/data/runs/x"}
	checks := FromRun(run, []byte("not json"))
	if len(checks) != 1 || checks[0].Status != StatusFail {
		t.Fatalf("FromRun(malformed on success) = %+v", checks)
	}
}

func TestFromRunEmptyResultOnSuccess(t *testing.T) {
	// Exit 0 with nothing written: "the checks ran" is a promise the script
	// broke, so this is a fail, not an empty pass.
	run := model.Run{Outcome: model.OutcomeSuccess, Dir: "/data/runs/x"}
	checks := FromRun(run, nil)
	if len(checks) != 1 || checks[0].Status != StatusFail {
		t.Fatalf("FromRun(empty result, exit 0) = %+v", checks)
	}
}

func TestNormalizeAgentState(t *testing.T) {
	cases := map[string]string{"ok": "ok", "limited": "limited", "": "unknown", "weird": "unknown"}
	for in, want := range cases {
		if got := NormalizeAgentState(in); got != want {
			t.Errorf("NormalizeAgentState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgentCheckOK(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeSuccess}
	data, _ := json.Marshal(map[string]string{"state": "ok", "summary": "all clear"})
	c := AgentCheck("claude", run, data, nil)
	if c.Status != StatusPass || c.Detail != "all clear" {
		t.Fatalf("AgentCheck(ok) = %+v", c)
	}
}

func TestAgentCheckLimited(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeSuccess}
	data, _ := json.Marshal(map[string]string{"state": "limited", "summary": "over weekly quota"})
	c := AgentCheck("claude", run, data, nil)
	if c.Status != StatusWarn || c.Detail != "over weekly quota" {
		t.Fatalf("AgentCheck(limited) = %+v", c)
	}
}

func TestAgentCheckUnrecognisedState(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeSuccess}
	data, _ := json.Marshal(map[string]string{"state": "confused"})
	c := AgentCheck("claude", run, data, nil)
	if c.Status != StatusUnknown {
		t.Fatalf("AgentCheck(unrecognised) = %+v", c)
	}
}

func TestAgentCheckRunError(t *testing.T) {
	c := AgentCheck("claude", model.Run{}, nil, errors.New("boom"))
	if c.Status != StatusUnknown || !contains(c.Detail, "boom") {
		t.Fatalf("AgentCheck(run error) = %+v", c)
	}
}

func TestAgentCheckNonZeroExit(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeFailure, ExitCode: 7}
	c := AgentCheck("claude", run, nil, nil)
	if c.Status != StatusUnknown {
		t.Fatalf("AgentCheck(non-zero exit) = %+v", c)
	}
}

func TestAgentCheckEmptyResult(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeSuccess}
	c := AgentCheck("claude", run, nil, nil)
	if c.Status != StatusUnknown || !contains(c.Detail, "nothing") {
		t.Fatalf("AgentCheck(empty result) = %+v", c)
	}
}

func TestAgentCheckNotAnObject(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeSuccess}
	c := AgentCheck("claude", run, []byte(`[1,2,3]`), nil)
	if c.Status != StatusUnknown {
		t.Fatalf("AgentCheck(not an object) = %+v", c)
	}
}

func TestCounts(t *testing.T) {
	checks := []Check{{Status: StatusPass}, {Status: StatusPass}, {Status: StatusFail}, {Status: StatusSkip}}
	counts := Counts(checks)
	if counts[StatusPass] != 2 || counts[StatusFail] != 1 || counts[StatusSkip] != 1 {
		t.Fatalf("Counts = %v", counts)
	}
}

func TestAnyFailed(t *testing.T) {
	if AnyFailed([]Check{{Status: StatusPass}, {Status: StatusWarn}, {Status: StatusSkip}}) {
		t.Error("AnyFailed = true for an all-clear set")
	}
	if !AnyFailed([]Check{{Status: StatusPass}, {Status: StatusUnknown}}) {
		t.Error("AnyFailed = false with an unknown present")
	}
}

func contains(s, substr string) bool { return strings.Contains(s, substr) }

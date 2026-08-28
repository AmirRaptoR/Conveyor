package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/agent-team/internal/model"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(t *testing.T, body string, timeout time.Duration) *Result {
	t.Helper()
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, body), Kind: "stage", Workdir: t.TempDir(),
		Timeout: timeout, Source: "test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func TestExitCodesMapToOutcomes(t *testing.T) {
	for _, tc := range []struct {
		body string
		want model.Outcome
	}{
		{"exit 0", model.OutcomeSuccess},
		{"exit 10", model.OutcomeNoop},
		{"exit 20", model.OutcomeBlocked},
		{"exit 1", model.OutcomeFailure},
		{"exit 7", model.OutcomeFailure},
	} {
		if got := run(t, tc.body, time.Minute).Run.Outcome; got != tc.want {
			t.Errorf("%q: got %s want %s", tc.body, got, tc.want)
		}
	}
}

// Logs must never be parsed as data: a script that prints JSON to stdout but
// writes nothing to $AGENT_TEAM_RESULT has produced no data at all.
func TestStdoutIsNeverData(t *testing.T) {
	res := run(t, `echo '{"pr": 999}'`, time.Minute)
	if res.Data != nil {
		t.Fatalf("stdout leaked into Data: %s", res.Data)
	}
	if len(res.Log) != 1 || !strings.Contains(res.Log[0].Text, "999") {
		t.Fatalf("expected the JSON as a log line, got %+v", res.Log)
	}
}

func TestResultFileIsData(t *testing.T) {
	res := run(t, `echo noise; echo '{"ok":true}' > "$AGENT_TEAM_RESULT"`, time.Minute)
	if res.Data == nil || !strings.Contains(string(res.Data), "true") {
		t.Fatalf("result file not captured: %s", res.Data)
	}
}

func TestMalformedResultIsIgnoredNotFatal(t *testing.T) {
	res := run(t, `echo 'not json' > "$AGENT_TEAM_RESULT"; exit 0`, time.Minute)
	if res.Run.Outcome != model.OutcomeSuccess {
		t.Errorf("outcome changed by bad result file: %s", res.Run.Outcome)
	}
	if res.Data != nil {
		t.Errorf("invalid JSON should not be surfaced as data")
	}
}

func TestStderrAndStdoutBothCaptured(t *testing.T) {
	res := run(t, `echo out; echo err >&2`, time.Minute)
	var streams []string
	for _, l := range res.Log {
		streams = append(streams, l.Stream+":"+l.Text)
	}
	joined := strings.Join(streams, ",")
	if !strings.Contains(joined, "stdout:out") || !strings.Contains(joined, "stderr:err") {
		t.Fatalf("missing a stream: %s", joined)
	}
}

// A timeout must kill the whole process group, not just the parent: an AI stage
// script spawns children that would otherwise hold the source's lock.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	res := run(t, `( while true; do sleep 1; done ) & sleep 60`, 500*time.Millisecond)
	if res.Run.Outcome != model.OutcomeTimeout {
		t.Fatalf("got %s want timeout", res.Run.Outcome)
	}
	if !res.Run.TimedOut {
		t.Error("TimedOut not set")
	}
}

// A run must be self-contained: everything needed to debug it, on disk.
func TestRunDirectoryIsSelfContained(t *testing.T) {
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `echo hi; echo '{"a":1}' > "$AGENT_TEAM_RESULT"`),
		Kind:   "stage", Workdir: t.TempDir(), Timeout: time.Minute, Source: "test",
		Item:  &model.Item{ID: "x:1", Ref: "1"},
		Stdin: map[string]string{"hello": "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"meta.json", "stdin.json", "log.txt", "result.json"} {
		if _, err := os.Stat(filepath.Join(res.Run.Dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(res.Run.Dir, "stdin.json"))
	if !strings.Contains(string(b), "world") {
		t.Error("stdin.json did not record the input")
	}
}

func TestMissingScriptIsAnErrorNotAnOutcome(t *testing.T) {
	r := New(t.TempDir())
	_, err := r.Run(context.Background(), Spec{
		Script: "/nonexistent/nope.sh", Kind: "stage", Workdir: t.TempDir(), Source: "test",
	})
	if err == nil {
		t.Fatal("expected an error when the script cannot be run at all")
	}
}

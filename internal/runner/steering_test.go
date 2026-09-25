package runner

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestOnSteeringTailsLiveAndFinalState(t *testing.T) {
	old := tickInterval
	tickInterval = 10 * time.Millisecond
	t.Cleanup(func() { tickInterval = old })
	r := New(t.TempDir())
	var mu sync.Mutex
	var updates []SteeringUpdate
	r.OnSteering = func(_, _ string, u SteeringUpdate) {
		mu.Lock()
		updates = append(updates, u)
		mu.Unlock()
	}
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `
printf '%s\n' '{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":["instruction"],"session":"s1"}' >> "$CONVEYOR_CONTROL_ACK"
sleep 0.05
`),
		Kind: "stage", To: "working", Workdir: t.TempDir(), Source: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(updates) < 2 {
		t.Fatalf("updates = %+v", updates)
	}
	foundLive := false
	for _, u := range updates {
		if !u.Final && u.Resolution.Hello && u.Kind == "stage" && u.Stage == "working" {
			foundLive = true
		}
	}
	if !foundLive || !updates[len(updates)-1].Final || res.Run.Outcome != "success" {
		t.Fatalf("updates=%+v outcome=%s", updates, res.Run.Outcome)
	}
}

func TestMalformedAckDoesNotChangeRunOutcome(t *testing.T) {
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `printf '%s\n' 'not json' >> "$CONVEYOR_CONTROL_ACK"`),
		Kind:   "stage", To: "working", Workdir: t.TempDir(), Source: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Run.Outcome != "success" || res.Run.ExitCode != 0 {
		t.Fatalf("outcome=%s exit=%d", res.Run.Outcome, res.Run.ExitCode)
	}
}

func TestAckRemovalStopsTailingWithoutLosingAcceptedState(t *testing.T) {
	old := tickInterval
	tickInterval = 10 * time.Millisecond
	t.Cleanup(func() { tickInterval = old })
	r := New(t.TempDir())
	var mu sync.Mutex
	var last SteeringUpdate
	r.OnSteering = func(_, _ string, u SteeringUpdate) {
		mu.Lock()
		last = u
		mu.Unlock()
	}
	_, err := r.Run(context.Background(), Spec{
		Script: script(t, `
printf '%s\n' '{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":["instruction"],"session":"kept"}' >> "$CONVEYOR_CONTROL_ACK"
sleep 0.05
rm "$CONVEYOR_CONTROL_ACK"
sleep 0.03
`),
		Kind: "stage", To: "working", Workdir: t.TempDir(), Source: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !last.Final || !last.Resolution.Hello || last.Resolution.Session != "kept" {
		t.Fatalf("final update lost accepted state: %+v", last)
	}
}

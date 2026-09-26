package runner

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func TestTransientProbeLeavesNoRunOrCallbacks(t *testing.T) {
	root := t.TempDir()
	r := New(root)
	var callbacks atomic.Int64
	r.OnStart = func(model.Run) { callbacks.Add(1) }
	r.OnProcessExit = func(model.Run) { callbacks.Add(1) }
	r.OnLog = func(string, LogLine) { callbacks.Add(1) }
	r.OnResult = func(*Result) { callbacks.Add(1) }
	r.OnPlan = func(string, string, PlanUpdate) { callbacks.Add(1) }
	r.OnSteering = func(string, string, SteeringUpdate) { callbacks.Add(1) }

	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `printf '{"changed":false}' > "$CONVEYOR_RESULT"; echo private-probe-log`),
		Kind:   "probe", Workdir: t.TempDir(), Source: "s1", Transient: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatalf("probe result: %v", err)
	}
	if got.Changed {
		t.Fatal("probe result changed during transport")
	}
	if callbacks.Load() != 0 {
		t.Fatalf("transient probe fired %d public callback(s)", callbacks.Load())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("transient probe left run-store entries: %v", entries)
	}
}

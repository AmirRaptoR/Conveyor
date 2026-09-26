package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Every run gets control.jsonl and control-ack.jsonl, pre-created 0600,
// named to the script by CONVEYOR_CONTROL / CONVEYOR_CONTROL_ACK — the
// mirror image of plan.jsonl's own uniform creation across every Kind.
func TestControlChannelCreatedForEveryKind(t *testing.T) {
	for _, kind := range []string{"list", "move", "stage", "status", "preflight"} {
		t.Run(kind, func(t *testing.T) {
			r := New(t.TempDir())
			res, err := r.Run(context.Background(), Spec{
				Script: script(t, `test -n "$CONVEYOR_CONTROL" && test -n "$CONVEYOR_CONTROL_ACK" && echo ok > "$CONVEYOR_RESULT"`),
				Kind:   kind, Workdir: t.TempDir(), Source: "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			ctlPath := filepath.Join(res.Run.Dir, "control.jsonl")
			ackPath := filepath.Join(res.Run.Dir, "control-ack.jsonl")
			for _, p := range []string{ctlPath, ackPath} {
				info, err := os.Stat(p)
				if err != nil {
					t.Fatalf("%s missing: %v", p, err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("%s mode = %v, want 0600", p, info.Mode().Perm())
				}
			}
			if res.Run.Env["CONVEYOR_CONTROL"] != ctlPath {
				t.Errorf("CONVEYOR_CONTROL = %q, want %q", res.Run.Env["CONVEYOR_CONTROL"], ctlPath)
			}
			if res.Run.Env["CONVEYOR_CONTROL_ACK"] != ackPath {
				t.Errorf("CONVEYOR_CONTROL_ACK = %q, want %q", res.Run.Env["CONVEYOR_CONTROL_ACK"], ackPath)
			}
		})
	}
}

func TestControlChannelCreatedForInlineScript(t *testing.T) {
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Inline: "#!/usr/bin/env bash\necho \"$CONVEYOR_CONTROL:$CONVEYOR_CONTROL_ACK\" > \"$CONVEYOR_RESULT\"\n",
		Kind:   "stage", Workdir: t.TempDir(), Source: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(res.Run.Dir, "control.jsonl")); err != nil {
		t.Fatalf("control.jsonl missing for inline script: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Run.Dir, "control-ack.jsonl")); err != nil {
		t.Fatalf("control-ack.jsonl missing for inline script: %v", err)
	}
}

// Neither variable can be overridden by a source's env: or a script's
// params: (both arrive through Spec.Env), the same treatment CONVEYOR_PLAN
// already gets.
func TestControlChannelCannotBeOverridden(t *testing.T) {
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, `echo "$CONVEYOR_CONTROL:$CONVEYOR_CONTROL_ACK" > "$CONVEYOR_RESULT"`),
		Kind:   "stage", Workdir: t.TempDir(), Source: "test",
		Env: map[string]string{
			"CONVEYOR_CONTROL":     "/tmp/evil-control.jsonl",
			"CONVEYOR_CONTROL_ACK": "/tmp/evil-control-ack.jsonl",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCtl := filepath.Join(res.Run.Dir, "control.jsonl")
	wantAck := filepath.Join(res.Run.Dir, "control-ack.jsonl")
	if got := res.Run.Env["CONVEYOR_CONTROL"]; got != wantCtl {
		t.Errorf("CONVEYOR_CONTROL was overridden: got %q, want %q", got, wantCtl)
	}
	if got := res.Run.Env["CONVEYOR_CONTROL_ACK"]; got != wantAck {
		t.Errorf("CONVEYOR_CONTROL_ACK was overridden: got %q, want %q", got, wantAck)
	}
}

// A process that fails to start must not panic — the control channel is
// created before Start() is even attempted, the same as result.json and
// plan.jsonl.
func TestControlChannelNoPanicWhenProcessFailsToStart(t *testing.T) {
	r := New(t.TempDir())
	_, err := r.Run(context.Background(), Spec{
		Script: "/nonexistent/nope.sh", Kind: "stage", Workdir: t.TempDir(), Source: "test",
	})
	if err == nil {
		t.Fatal("expected an error when the script cannot be run at all")
	}
}

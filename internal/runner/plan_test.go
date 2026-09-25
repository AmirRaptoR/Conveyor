package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// Every run gets a plan.jsonl, pre-created 0600, named to the script by
// CONVEYOR_PLAN — list, move, stage, doctor, status, preflight and an inline
// run: body alike, since Run() handles every Kind uniformly.
func TestPlanChannelCreatedForEveryKind(t *testing.T) {
	for _, kind := range []string{"list", "move", "stage", "doctor", "status", "preflight"} {
		t.Run(kind, func(t *testing.T) {
			r := New(t.TempDir())
			res, err := r.Run(context.Background(), Spec{
				Script: script(t, `test -n "$CONVEYOR_PLAN" && echo "$CONVEYOR_PLAN" > "$CONVEYOR_RESULT"`),
				Kind:   kind, Workdir: t.TempDir(), Source: "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(res.Run.Dir, "plan.jsonl")
			info, err := os.Stat(planPath)
			if err != nil {
				t.Fatalf("plan.jsonl missing: %v", err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("plan.jsonl mode = %v, want 0600", info.Mode().Perm())
			}
			if res.Run.Env["CONVEYOR_PLAN"] != planPath {
				t.Errorf("CONVEYOR_PLAN = %q, want %q", res.Run.Env["CONVEYOR_PLAN"], planPath)
			}
		})
	}
}

// A process that fails to start must not leave the plan tailer goroutine
// dangling or panic — the plan channel is created before Start() is even
// attempted, the same as result.json.
func TestPlanChannelNoPanicWhenProcessFailsToStart(t *testing.T) {
	r := New(t.TempDir())
	_, err := r.Run(context.Background(), Spec{
		Script: "/nonexistent/nope.sh", Kind: "stage", Workdir: t.TempDir(), Source: "test",
	})
	if err == nil {
		t.Fatal("expected an error when the script cannot be run at all")
	}
}

func TestPlanChannelCreatedForInlineScript(t *testing.T) {
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Inline: "#!/usr/bin/env bash\necho \"$CONVEYOR_PLAN\" > \"$CONVEYOR_RESULT\"\n",
		Kind:   "stage", Workdir: t.TempDir(), Source: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(res.Run.Dir, "plan.jsonl")
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("plan.jsonl missing for inline script: %v", err)
	}
}

// CONVEYOR_PLAN is engine-owned: a source's env: or a script's params:
// (both arrive through Spec.Env) must not be able to redirect it elsewhere.
func TestPlanChannelCannotBeOverridden(t *testing.T) {
	r := New(t.TempDir())
	res, err := r.Run(context.Background(), Spec{
		Script:  script(t, `echo "$CONVEYOR_PLAN" > "$CONVEYOR_RESULT"`),
		Kind:    "stage", Workdir: t.TempDir(), Source: "test",
		Env: map[string]string{"CONVEYOR_PLAN": "/tmp/evil-plan.jsonl"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(res.Run.Dir, "plan.jsonl")
	if got := res.Run.Env["CONVEYOR_PLAN"]; got != want {
		t.Errorf("CONVEYOR_PLAN was overridden: got %q, want %q", got, want)
	}
}

// OnPlan fires live, for every accepted revision, while the process runs.
func TestOnPlanFiresLiveForAcceptedRevisions(t *testing.T) {
	r := New(t.TempDir())
	tickInterval = 20 * time.Millisecond
	defer func() { tickInterval = time.Second }()

	var updates []PlanUpdate
	var mu sync.Mutex
	r.OnPlan = func(runID, itemID string, u PlanUpdate) {
		mu.Lock()
		updates = append(updates, u)
		mu.Unlock()
	}

	body := `printf '{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"step one","status":"in_progress"}]}\n' > "$CONVEYOR_PLAN"
sleep 0.15
printf '{"v":1,"rev":2,"at":"2026-09-24T12:00:01Z","todos":[{"id":"a","text":"step one","status":"completed"}]}\n' >> "$CONVEYOR_PLAN"
sleep 0.15
`
	_, err := r.Run(context.Background(), Spec{
		Script: script(t, body), Kind: "stage", Workdir: t.TempDir(), Source: "test",
		Item: &model.Item{ID: "i1", Ref: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(updates) < 2 {
		t.Fatalf("expected at least 2 live plan updates, got %d: %+v", len(updates), updates)
	}
	sawLive := false
	for _, u := range updates[:len(updates)-1] {
		if u.HasRevision {
			sawLive = true
		}
	}
	if !sawLive {
		t.Errorf("expected at least one update to have arrived before the process exited")
	}
	last := updates[len(updates)-1]
	if !last.HasRevision || last.Revision.Rev != 2 {
		t.Fatalf("expected final update to carry revision 2, got %+v", last)
	}
}

// OnPlan carries the run's own Kind and target Stage, so a subscriber keying
// board state by item can tell a stage run targeting a real stage apart from
// a list, move, doctor or status run publishing against the same item — the
// only distinction that decides whether a revision may ever reach a card.
func TestOnPlanCarriesKindAndStage(t *testing.T) {
	r := New(t.TempDir())
	var got runner_PlanUpdateCapture
	r.OnPlan = func(runID, itemID string, u PlanUpdate) {
		got = runner_PlanUpdateCapture{kind: u.Kind, stage: u.Stage, has: true}
	}
	body := `printf '{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"step","status":"pending"}]}\n' > "$CONVEYOR_PLAN"`
	_, err := r.Run(context.Background(), Spec{
		Script: script(t, body), Kind: "stage", To: "implementing",
		Workdir: t.TempDir(), Source: "test", Item: &model.Item{ID: "i1", Ref: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.has {
		t.Fatal("OnPlan never fired")
	}
	if got.kind != "stage" || got.stage != "implementing" {
		t.Errorf("got Kind=%q Stage=%q, want Kind=%q Stage=%q", got.kind, got.stage, "stage", "implementing")
	}
}

type runner_PlanUpdateCapture struct {
	kind, stage string
	has         bool
}

// A rejection is diagnosed into the run's own log, capped at 10, with a
// suppressed-count summary line after the final read.
func TestPlanRejectionsAreLoggedAsEngineLines(t *testing.T) {
	r := New(t.TempDir())
	body := `printf 'not json\n' > "$CONVEYOR_PLAN"`
	res, err := r.Run(context.Background(), Spec{
		Script: script(t, body), Kind: "stage", Workdir: t.TempDir(), Source: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range res.Log {
		if l.Stream == "engine" && strings.Contains(l.Text, "plan.jsonl") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an engine log line naming the plan.jsonl rejection, got %+v", res.Log)
	}
}

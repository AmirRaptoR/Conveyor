package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/registry"
	"github.com/AmirRaptoR/Conveyor/internal/steering"
)

// steerBoard sets up a Server with one item whose stage has a live run
// registered, and that run's control-ack.jsonl already carrying a hello
// naming the given accepts — everything handleSteer needs without
// running an actual pipeline dispatch.
func steerBoard(t *testing.T, accepts ...string) (*Server, string) {
	t.Helper()
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working"}}

	runDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runDir, "control.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var acceptsJSON string
	for i, a := range accepts {
		if i > 0 {
			acceptsJSON += ","
		}
		acceptsJSON += `"` + a + `"`
	}
	ack := fmt.Sprintf(`{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":[%s]}`+"\n", acceptsJSON)
	if err := os.WriteFile(filepath.Join(runDir, "control-ack.jsonl"), []byte(ack), 0o600); err != nil {
		t.Fatal(err)
	}
	s.liveRuns.Open("s1:1", registry.Entry{RunID: "run1", Dir: runDir, Stage: "working"})
	return s, runDir
}

func postSteer(s *Server, id, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/items/"+id+"/steer", strings.NewReader(body))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.handleSteer(w, r)
	return w
}

func TestSteerInstructionIsAccepted(t *testing.T) {
	s, dir := steerBoard(t, "instruction", "pause")
	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"do the thing","runId":"run1"}`)
	if w.Code != 202 {
		t.Fatalf("status = %d (%s), want 202", w.Code, w.Body.String())
	}
	res := steering.Load(dir, false)
	if len(res.Commands) != 1 || res.Commands[0].Command.Text != "do the thing" || res.Commands[0].Command.Kind != "instruction" {
		t.Fatalf("unexpected commands: %+v", res.Commands)
	}
	if res.Commands[0].Command.ItemID != "s1:1" || res.Commands[0].Command.RunID != "run1" {
		t.Fatalf("command not bound correctly: %+v", res.Commands[0])
	}
}

func TestSteerPauseIgnoresText(t *testing.T) {
	s, dir := steerBoard(t, "pause")
	w := postSteer(s, "s1:1", `{"kind":"pause","text":"ignored","runId":"run1"}`)
	if w.Code != 202 {
		t.Fatalf("status = %d (%s), want 202", w.Code, w.Body.String())
	}
	res := steering.Load(dir, false)
	if res.Commands[0].Command.Text != "" {
		t.Fatalf("expected pause's text stored empty, got %q", res.Commands[0].Command.Text)
	}
}

func TestSteerNoItemIs404(t *testing.T) {
	s, _ := steerBoard(t, "instruction")
	w := postSteer(s, "nope", `{"kind":"instruction","text":"x","runId":"run1"}`)
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestSteerNoLiveRunIs409(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working"}}
	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"x","runId":"run1"}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestSteerMissingRunIDIs400(t *testing.T) {
	s, _ := steerBoard(t, "instruction")
	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"x"}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSteerWrongRunIDIs409(t *testing.T) {
	s, _ := steerBoard(t, "instruction")
	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"x","runId":"run-old"}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409 (that run has already finished)", w.Code)
	}
}

func TestSteerUnadvertisedKindIs409(t *testing.T) {
	s, _ := steerBoard(t, "instruction") // pause not advertised
	w := postSteer(s, "s1:1", `{"kind":"pause","runId":"run1"}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestSteerNoHelloAtAllIs409(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working"}}
	runDir := t.TempDir()
	os.WriteFile(filepath.Join(runDir, "control.jsonl"), nil, 0o600)
	os.WriteFile(filepath.Join(runDir, "control-ack.jsonl"), nil, 0o600)
	s.liveRuns.Open("s1:1", registry.Entry{RunID: "run1", Dir: runDir, Stage: "working"})

	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"x","runId":"run1"}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestSteerStaleSessionIs409(t *testing.T) {
	s, dir := steerBoard(t, "instruction")
	ack := `{"v":1,"type":"session","seq":2,"at":"2026-09-24T12:00:01Z","session":"current"}` + "\n"
	f, err := os.OpenFile(filepath.Join(dir, "control-ack.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(ack)
	f.Close()

	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"x","runId":"run1","session":"stale"}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestSteerUnknownKindIs400(t *testing.T) {
	s, _ := steerBoard(t, "instruction")
	w := postSteer(s, "s1:1", `{"kind":"teleport","runId":"run1"}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSteerEmptyInstructionTextIs400(t *testing.T) {
	s, _ := steerBoard(t, "instruction")
	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"   ","runId":"run1"}`)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSteerTextOverCapIs400(t *testing.T) {
	s, _ := steerBoard(t, "instruction")
	long := strings.Repeat("a", steering.MaxTextLen+1)
	body, _ := json.Marshal(map[string]string{"kind": "instruction", "text": long, "runId": "run1"})
	w := postSteer(s, "s1:1", string(body))
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSteerObserveModeIs403(t *testing.T) {
	s, _ := steerBoard(t, "instruction")
	s.mode = ModeObserve
	guarded := s.mutationGuard(s.handleSteer)
	r := httptest.NewRequest("POST", "/api/items/s1:1/steer", strings.NewReader(`{"kind":"instruction","text":"x","runId":"run1"}`))
	r.SetPathValue("id", "s1:1")
	w := httptest.NewRecorder()
	guarded(w, r)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestSteerCommandCeilingIs409(t *testing.T) {
	s, dir := steerBoard(t, "instruction")
	for i := 0; i < steering.MaxCommands; i++ {
		if _, err := steering.AppendCommand(filepath.Join(dir, "control.jsonl"), steering.Command{
			ID: fmt.Sprintf("c%d", i), Kind: "instruction", Text: "x", ItemID: "s1:1", RunID: "run1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"one more","runId":"run1"}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409 once the command ceiling is reached", w.Code)
	}
}

// Two concurrent POSTs for the same item both land, with distinct strictly
// increasing seq and distinct ids, and neither line is lost nor interleaved.
func TestSteerConcurrentPostsBothLand(t *testing.T) {
	s, dir := steerBoard(t, "instruction")
	var wg sync.WaitGroup
	n := 10
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"kind":"instruction","text":"cmd %d","runId":"run1"}`, i)
			if w := postSteer(s, "s1:1", body); w.Code != 202 {
				t.Errorf("post %d: status = %d (%s)", i, w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()

	res := steering.Load(dir, false)
	if len(res.Commands) != n {
		t.Fatalf("expected %d commands, got %d", n, len(res.Commands))
	}
	seen := map[string]bool{}
	for _, c := range res.Commands {
		if seen[c.Command.ID] {
			t.Fatalf("duplicate command id %q", c.Command.ID)
		}
		seen[c.Command.ID] = true
	}
}

func TestSteerByIsRequestedByUnauthenticated(t *testing.T) {
	s, dir := steerBoard(t, "instruction")
	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"x","runId":"run1"}`)
	if w.Code != 202 {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	res := steering.Load(dir, false)
	if res.Commands[0].Command.By != "" {
		t.Errorf("expected empty By with no Basic Auth on the request, got %q", res.Commands[0].Command.By)
	}
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
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
	ack := fmt.Sprintf(`{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":[%s],"session":""}`+"\n", acceptsJSON)
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

func TestSteerMissingRunIDIs409(t *testing.T) {
	s, _ := steerBoard(t, "instruction")
	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"x"}`)
	if w.Code != 409 {
		t.Fatalf("status = %d, want 409", w.Code)
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

func TestSteerConcurrentPostsToTwoItemsStayIndependent(t *testing.T) {
	s, firstDir := steerBoard(t, "instruction")
	secondDir := t.TempDir()
	os.WriteFile(filepath.Join(secondDir, "control.jsonl"), nil, 0o600)
	os.WriteFile(filepath.Join(secondDir, "control-ack.jsonl"), []byte(hello("")+"\n"), 0o600)
	s.state.Items = append(s.state.Items, model.Item{ID: "s1:2", Ref: "2", Source: "s1", Stage: "working"})
	s.liveRuns.Open("s1:2", registry.Entry{RunID: "run2", Dir: secondDir, Stage: "working"})

	var wg sync.WaitGroup
	for _, tc := range []struct{ id, run string }{{"s1:1", "run1"}, {"s1:2", "run2"}} {
		tc := tc
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"kind":"instruction","text":"%s","runId":"%s"}`, tc.id, tc.run)
			if w := postSteer(s, tc.id, body); w.Code != 202 {
				t.Errorf("%s: %d %s", tc.id, w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if got := steering.Load(firstDir, false); len(got.Commands) != 1 || got.Commands[0].Command.ItemID != "s1:1" {
		t.Fatalf("first run = %+v", got.Commands)
	}
	if got := steering.Load(secondDir, false); len(got.Commands) != 1 || got.Commands[0].Command.ItemID != "s1:2" {
		t.Fatalf("second run = %+v", got.Commands)
	}
}

func TestSteerRacingCloseNeverAppendsAfterClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		s, dir := steerBoard(t, "instruction")
		start := make(chan struct{})
		var w *httptest.ResponseRecorder
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			w = postSteer(s, "s1:1", `{"kind":"instruction","text":"x","runId":"run1"}`)
		}()
		go func() {
			defer wg.Done()
			<-start
			s.liveRuns.Close("s1:1", "run1")
		}()
		close(start)
		wg.Wait()
		before := len(steering.Load(dir, false).Commands)
		if w.Code != 202 && w.Code != 409 {
			t.Fatalf("race status = %d: %s", w.Code, w.Body.String())
		}
		if (w.Code == 202 && before != 1) || (w.Code == 409 && before != 0) {
			t.Fatalf("status=%d commands=%d", w.Code, before)
		}
		after := postSteer(s, "s1:1", `{"kind":"instruction","text":"late","runId":"run1"}`)
		if after.Code != 409 || len(steering.Load(dir, false).Commands) != before {
			t.Fatalf("post-close status=%d commands before=%d after=%d", after.Code, before, len(steering.Load(dir, false).Commands))
		}
	}
}

func TestSteerPublishesFullSSEEvent(t *testing.T) {
	s, _ := steerBoard(t, "instruction")
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	w := postSteer(s, "s1:1", `{"kind":"instruction","text":"x","runId":"run1"}`)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	select {
	case e := <-ch:
		if e.Kind != "steering" || e.Steering == nil || len(e.Steering.Commands) != 1 || e.Steering.Commands[0].Text != "x" {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no steering event")
	}
}

func TestSteerRequestedByThroughAuthenticatedAndSocketHandlers(t *testing.T) {
	s, dir := steerBoard(t, "instruction")
	s.cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "secret")}}
	s.verify = newAuthVerifier(s.cfg.Auth.Check)
	tcp, socket, err := s.handler()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/items/s1:1/steer", bytes.NewBufferString(`{"kind":"instruction","text":"tcp","runId":"run1"}`))
	req.Host = "localhost"
	req.SetBasicAuth("amir", "secret")
	w := httptest.NewRecorder()
	tcp.ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatalf("authenticated request = %d: %s", w.Code, w.Body.String())
	}
	res := steering.Load(dir, false)
	if res.Commands[0].Command.By != "amir" {
		t.Fatalf("authenticated by = %q", res.Commands[0].Command.By)
	}

	// The socket intentionally has no auth wall. A Basic header there is only
	// caller-supplied audit text, exactly as requestedBy already documents.
	req = httptest.NewRequest("POST", "/api/items/s1:1/steer", bytes.NewBufferString(`{"kind":"instruction","text":"socket","runId":"run1"}`))
	req.Host = "localhost"
	req.SetBasicAuth("claimed", "anything")
	w = httptest.NewRecorder()
	socket.ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatalf("socket-chain request = %d: %s", w.Code, w.Body.String())
	}
	res = steering.Load(dir, false)
	if res.Commands[1].Command.By != "claimed" {
		t.Fatalf("socket by = %q", res.Commands[1].Command.By)
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

func TestCancelRunIDCannotCancelReplacement(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	called := make(chan struct{}, 1)
	s.cancelFns.Store("s1:1", context.CancelFunc(func() { called <- struct{}{} }))
	s.liveRuns.Open("s1:1", registry.Entry{RunID: "new-run"})

	request := func(runID string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"reason":"stop","runId":%q}`, runID)
		req := httptest.NewRequest("POST", "/api/items/s1:1/cancel", strings.NewReader(body))
		req.SetPathValue("id", "s1:1")
		w := httptest.NewRecorder()
		s.handleCancel(w, req)
		return w
	}
	if w := request("old-run"); w.Code != 409 {
		t.Fatalf("stale run = %d", w.Code)
	}
	select {
	case <-called:
		t.Fatal("stale run id cancelled the replacement")
	default:
	}
	if w := request("new-run"); w.Code != 202 {
		t.Fatalf("live run = %d: %s", w.Code, w.Body.String())
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("matching run id did not cancel")
	}
}

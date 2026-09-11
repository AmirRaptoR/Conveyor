package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// modePipeline lays down two items: s1:1 sits in "backlog", unblocked and
// ready to move into "working" — the shape a tick, a drag-to-start or the
// scheduler would advance. s1:2 sits in "working" already marked, the shape
// Unblock and Unblock-all act on. Every listing, stage run and move appends a
// line to its own counter file, so a test can prove a script never ran rather
// than merely that a handler answered a particular status code.
func modePipeline(t *testing.T) (cfg *config.Config, r *runner.Runner, stageRuns, moveRuns, lists, dir string) {
	t.Helper()
	dir = t.TempDir()
	stageRuns = filepath.Join(dir, "stageruns")
	moveRuns = filepath.Join(dir, "moveruns")
	lists = filepath.Join(dir, "lists")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
echo x >> `+lists+`
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"backlog","title":"ready to move"},
 {"id":"s1:2","ref":"2","source":"s1","stage":"working","title":"marked","blocked":true}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\necho x >> "+moveRuns+"\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\necho x >> "+stageRuns+"\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 24h
stages:
  - name: backlog
  - name: working
    script: work
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo
    scripts:
      work:
        script: ./work.sh
`), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, runner.New(filepath.Join(dir, "runs")), stageRuns, moveRuns, lists, dir
}

// respRecorder is a minimal httptest.ResponseRecorder, spelled out to avoid
// importing net/http/httptest just for this shape.
type respRecorder struct {
	code   int
	header http.Header
	buf    bytes.Buffer
}

func (r *respRecorder) Header() http.Header { return r.header }
func (r *respRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.buf.Write(b)
}
func (r *respRecorder) WriteHeader(code int) { r.code = code }

// doReq drives a request through h and returns the status code and body.
func doReq(t *testing.T, h http.Handler, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://localhost"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost" // passes hostCheck; these tests are not about it
	w := &respRecorder{header: http.Header{}}
	h.ServeHTTP(w, req)
	return w.code, w.buf.Bytes()
}

// newModeServer builds a Server in the given mode with routes wired exactly
// as Run would wire them, without binding a real listener — the httptest
// pattern every other handler test in this package uses.
func newModeServer(t *testing.T, cfg *config.Config, r *runner.Runner, mode Mode) (*Server, http.Handler) {
	t.Helper()
	s := New(cfg, r)
	s.ctx = context.Background()
	s.mode = mode
	s.mu.Lock()
	s.state.Mode = string(mode)
	s.mu.Unlock()
	s.cop = http.NewCrossOriginProtection()
	h, _, err := s.handler()
	if err != nil {
		t.Fatal(err)
	}
	return s, h
}

// Every mutation route observe refuses, and none of them so much as started a
// stage, a move or a doctor script.
func TestObserveModeRefusesEveryMutationRoute(t *testing.T) {
	cfg, r, stageRuns, moveRuns, _, _ := modePipeline(t)
	s, h := newModeServer(t, cfg, r, ModeObserve)
	s.refresh(s.ctx)

	routes := []struct {
		method, path string
		body         []byte
	}{
		{"POST", "/api/tick", nil},
		{"POST", "/api/items/s1:1/start", nil},
		{"POST", "/api/items/s1:2/unblock", []byte(`{}`)},
		{"POST", "/api/unblock", nil},
		{"POST", "/api/doctor", nil},
	}
	for _, rt := range routes {
		code, _ := doReq(t, h, rt.method, rt.path, rt.body)
		if code != http.StatusForbidden {
			t.Errorf("%s %s in observe = %d, want 403", rt.method, rt.path, code)
		}
	}

	// Direct on s.tick too — the route is not the only guarantee, and a
	// button press must not reach the scheduler by some other path.
	ctx, cancel := context.WithCancel(context.Background())
	go s.button(ctx, ModeObserve)
	s.tick <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	cancel()

	if n := countLines(stageRuns); n != 0 {
		t.Errorf("%d stage run(s) happened in observe mode, want 0", n)
	}
	if n := countLines(moveRuns); n != 0 {
		t.Errorf("%d move(s) happened in observe mode, want 0", n)
	}
}

// Observation itself keeps working: listing, live state, refresh and order.
func TestObserveModeStillServesReadsAndOrder(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	s, h := newModeServer(t, cfg, r, ModeObserve)
	s.refresh(s.ctx)

	for _, path := range []string{"/api/state", "/api/runs"} {
		code, _ := doReq(t, h, "GET", path, nil)
		if code != http.StatusOK {
			t.Errorf("GET %s in observe = %d, want 200", path, code)
		}
	}
	if code, _ := doReq(t, h, "POST", "/api/refresh", nil); code != http.StatusAccepted {
		t.Errorf("POST /api/refresh in observe = %d, want 202", code)
	}
	// handleRefresh starts s.refresh in the background; wait for it so the
	// test's TempDir cleanup does not race a goroutine still writing into it.
	time.Sleep(20 * time.Millisecond) // let the goroutine actually start
	waitFor(t, "the background refresh to finish", func() bool { return !s.polling.Load() })

	order, _ := json.Marshal([]string{"s1:2", "s1:1"})
	if code, _ := doReq(t, h, "PUT", "/api/order", order); code != http.StatusNoContent {
		t.Errorf("PUT /api/order in observe = %d, want 204", code)
	}
	if got := s.order.IDs(); len(got) != 2 || got[0] != "s1:2" {
		t.Errorf("order = %v, want [s1:2 s1:1] persisted", got)
	}
}

// retryStalled never runs under observe: every item stays marked across
// several of its intervals and nothing moves. Mirrors exactly what Run does
// — stalled only ever starts when mode.Runs() — rather than a real listener.
func TestObserveModeNeverRunsRetryStalled(t *testing.T) {
	cfg, r, _, moveRuns, _, _ := modePipeline(t)
	cfg.RetryStalled = config.Duration(30 * time.Millisecond)
	s := New(cfg, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctx = ctx
	s.refresh(ctx)
	s.mu.Lock()
	for i := range s.state.Items {
		s.state.Items[i].Blocked = true // the guard stalled itself requires
	}
	s.mu.Unlock()

	if ModeObserve.Runs() {
		t.Fatal("ModeObserve.Runs() = true; Run would launch retryStalled under it")
	}
	// What Run actually does: `if mode.Runs() { go s.stalled(...) }`. Since it
	// does not here, nothing below should be able to move.
	time.Sleep(150 * time.Millisecond) // several retryStalled intervals

	s.mu.RLock()
	stillMarked := true
	for _, it := range s.state.Items {
		stillMarked = stillMarked && it.Blocked
	}
	s.mu.RUnlock()
	if !stillMarked {
		t.Error("an item was unmarked; retryStalled must not run under observe")
	}
	if n := countLines(moveRuns); n != 0 {
		t.Errorf("%d move(s) happened; retryStalled must not run under observe", n)
	}
}

// manual runs no scheduler on its own: several poll-interval-equivalents pass
// with nothing moving, and one tick then performs exactly one transition.
func TestManualModeRunsNoSchedulerButTickAdvancesOnce(t *testing.T) {
	cfg, r, stageRuns, _, _, _ := modePipeline(t)
	s, _ := newModeServer(t, cfg, r, ModeManual)
	s.refresh(s.ctx)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.button(ctx, ModeManual)
	// No s.schedule goroutine started, matching what Run does for manual.

	time.Sleep(120 * time.Millisecond)
	if n := countLines(stageRuns); n != 0 {
		t.Errorf("%d stage run(s) happened with no scheduler running, want 0", n)
	}

	s.tick <- struct{}{}
	waitFor(t, "the tick to perform one transition", func() bool { return countLines(stageRuns) == 1 })
	time.Sleep(80 * time.Millisecond)
	if n := countLines(stageRuns); n != 1 {
		t.Errorf("%d stage run(s) after one tick, want exactly 1", n)
	}
}

// GET /api/state carries the mode a server was started in.
func TestStateCarriesMode(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	for _, m := range []Mode{ModeAuto, ModeManual, ModeObserve} {
		_, h := newModeServer(t, cfg, r, m)
		_, body := doReq(t, h, "GET", "/api/state", nil)
		var st State
		if err := json.Unmarshal(body, &st); err != nil {
			t.Fatal(err)
		}
		if st.Mode != string(m) {
			t.Errorf("state.Mode = %q, want %q", st.Mode, m)
		}
	}
}

// Startup order: a listen address already in use makes Run fail before it
// lists anything.
func TestStartupBindsBeforeAnyGoroutineStarts(t *testing.T) {
	cfg, r, _, _, lists, _ := modePipeline(t)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	s := New(cfg, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Run(ctx, occupied.Addr().String(), ModeAuto); err == nil {
		t.Fatal("Run on an address already in use returned nil, want an error")
	}
	time.Sleep(50 * time.Millisecond) // any stray goroutine a bug launched
	if countLines(lists) != 0 {
		t.Error("a listing happened before Run could bind its listener")
	}
}

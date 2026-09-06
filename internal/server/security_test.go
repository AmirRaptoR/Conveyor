package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
)

func basic(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// sseRecorder is a minimal http.ResponseWriter + http.Flusher whose Context
// can be cancelled from the test, standing in for the request context a real
// connection closing would cancel.
type sseRecorder struct {
	mu     sync.Mutex
	code   int
	header http.Header
	buf    bytes.Buffer
	ctx    context.Context
	cancel context.CancelFunc
}

func newSSERecorder() *sseRecorder {
	ctx, cancel := context.WithCancel(context.Background())
	return &sseRecorder{header: http.Header{}, ctx: ctx, cancel: cancel}
}
func (r *sseRecorder) Header() http.Header { return r.header }
func (r *sseRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.buf.Write(b)
}
func (r *sseRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}
func (r *sseRecorder) Flush() {}
func (r *sseRecorder) Code() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code
}
func (r *sseRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// doReqHost is doReq with control over the Host and extra headers, for
// exercising the cross-origin and Host checks directly.
func doReqHost(t *testing.T, h http.Handler, method, path, host string, hdr map[string]string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(method, "http://x"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := &respRecorder{header: http.Header{}}
	h.ServeHTTP(w, req)
	return w.code
}

func mutationRoutes() []struct{ method, path string } {
	return []struct{ method, path string }{
		{"POST", "/api/tick"},
		{"POST", "/api/items/s1:1/start"},
		{"POST", "/api/items/s1:2/unblock"},
		{"POST", "/api/unblock"},
		{"POST", "/api/doctor"},
		{"POST", "/api/refresh"},
		{"PUT", "/api/order"},
	}
}

// A cross-site request — declared either by Sec-Fetch-Site or by an Origin
// naming a different host — is refused before it can do anything, whether or
// not it carries valid credentials.
func TestCrossSiteMutationIsRefused(t *testing.T) {
	cfg, r, _, moveRuns, _, _ := modePipeline(t)
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "s")}}
	s, h := newModeServer(t, cfg, r, ModeAuto)
	s.refresh(s.ctx)

	auth := map[string]string{"Authorization": "Basic " + basic("amir", "s")}
	for _, rt := range mutationRoutes() {
		hdr := merge(auth, map[string]string{"Sec-Fetch-Site": "cross-site"})
		code := doReqHost(t, h, rt.method, rt.path, "localhost", hdr, []byte("[]"))
		if code != http.StatusForbidden {
			t.Errorf("%s %s with Sec-Fetch-Site: cross-site = %d, want 403", rt.method, rt.path, code)
		}
	}
	for _, rt := range mutationRoutes() {
		hdr := merge(auth, map[string]string{"Origin": "https://evil.example.com"})
		code := doReqHost(t, h, rt.method, rt.path, "localhost", hdr, []byte("[]"))
		if code != http.StatusForbidden {
			t.Errorf("%s %s with a foreign Origin = %d, want 403", rt.method, rt.path, code)
		}
	}
	if n := countLines(moveRuns); n != 0 {
		t.Errorf("%d move(s) happened; a refused cross-origin request must have no side effects", n)
	}
}

// The host and cross-origin checks are the cheap ones, and they run before
// Basic Auth ever derives a key: a cross-site or bad-Host request carrying
// valid credentials is refused without spending a single PBKDF2 derivation,
// so a flood of either cannot buy CPU by attaching a real password.
func TestCheapChecksRunBeforePasswordVerification(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "s")}}
	s, h := newModeServer(t, cfg, r, ModeAuto)
	s.listenHost = "127.0.0.1"
	s.refresh(s.ctx)

	auth := map[string]string{"Authorization": "Basic " + basic("amir", "s")}
	code := doReqHost(t, h, "PUT", "/api/order", "localhost",
		merge(auth, map[string]string{"Sec-Fetch-Site": "cross-site"}), []byte(`["s1:1"]`))
	if code != http.StatusForbidden {
		t.Fatalf("cross-site request with valid credentials = %d, want 403", code)
	}
	if n := s.verify.derivations; n != 0 {
		t.Errorf("a refused cross-site request caused %d derivation(s), want 0", n)
	}

	code = doReqHost(t, h, "PUT", "/api/order", "evil.example.com", auth, []byte(`["s1:1"]`))
	if code != http.StatusForbidden {
		t.Fatalf("bad-Host request with valid credentials = %d, want 403", code)
	}
	if n := s.verify.derivations; n != 0 {
		t.Errorf("a refused bad-Host request caused %d derivation(s), want 0", n)
	}
}

// Same-origin requests, and requests with neither header at all (every
// curl/CLI caller), are not refused by the cross-origin check — they reach
// their handler and get whatever status that handler decides.
func TestSameOriginAndHeaderlessRequestsPassTheCrossOriginCheck(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	s, h := newModeServer(t, cfg, r, ModeAuto)
	s.refresh(s.ctx)

	for _, hdr := range []map[string]string{
		{"Sec-Fetch-Site": "same-origin"},
		{}, // no Sec-Fetch-Site, no Origin at all
	} {
		code := doReqHost(t, h, "PUT", "/api/order", "localhost", hdr, []byte(`["s1:1","s1:2"]`))
		if code == http.StatusForbidden {
			t.Errorf("PUT /api/order with headers %v = 403, want it to reach the handler", hdr)
		}
	}
}

// auth.origins entries are trusted: a request whose Origin names one is not
// cross-origin-refused, and its host is accepted as a Host header value too.
func TestAuthOriginsAreTrusted(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	cfg.Auth.Origins = []string{"https://board.example.com"}
	s, h := newModeServer(t, cfg, r, ModeAuto)
	s.refresh(s.ctx)

	code := doReqHost(t, h, "PUT", "/api/order", "localhost",
		map[string]string{"Origin": "https://board.example.com"}, []byte(`["s1:1","s1:2"]`))
	if code == http.StatusForbidden {
		t.Error("a request from a trusted auth.origins entry was refused")
	}

	code = doReqHost(t, h, "GET", "/api/state", "board.example.com", nil, nil)
	if code == http.StatusForbidden {
		t.Error("Host: board.example.com was refused though it is in auth.origins")
	}
}

// GET /api/events is untouched by the cross-origin check even when declared
// cross-site, and a read/write-unbounded http.Server never cuts the stream.
func TestEventsStreamSurvivesCrossSiteAndStaysOpen(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	s, h := newModeServer(t, cfg, r, ModeAuto)
	s.refresh(s.ctx)

	req, _ := http.NewRequest("GET", "http://x/api/events", nil)
	req.Host = "localhost"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := newSSERecorder()
	req = req.WithContext(rec.ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	if rec.Code() == http.StatusForbidden {
		t.Fatal("GET /api/events with Sec-Fetch-Site: cross-site was refused")
	}
	s.hub.publish(event{Kind: "state"})
	waitFor(t, "the event to reach the stream", func() bool {
		return strings.Contains(rec.String(), `"kind":"state"`)
	})

	rec.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not return after its context was cancelled")
	}
}

// The handler-chain check above proves the cross-origin rule; this proves
// the other half of the same criterion against a real listening
// http.Server — the only thing that can actually enforce a Read/WriteTimeout
// — held open past 30s and still receiving an event at the end of it. Those
// timeouts are deliberately never set (server.go's Run), because either one
// would cut exactly this stream.
func TestEventsStreamOverRealListenerStaysOpenPast30Seconds(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	s := New(cfg, r)
	s.listening = make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, "127.0.0.1:0", ModeAuto)
	addr := <-s.listening

	client := &http.Client{}
	req, _ := http.NewRequest("GET", "http://"+addr+"/api/events", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("GET /api/events with Sec-Fetch-Site: cross-site was refused")
	}

	// Publish once now and once past the 30s mark: read must succeed both
	// times off the same, still-open connection, proving nothing cut it in
	// between.
	s.hub.publish(event{Kind: "state"})
	if !readUntilContains(t, resp.Body, `"kind":"state"`, 5*time.Second) {
		t.Fatal("never received the first event")
	}

	start := time.Now()
	for time.Since(start) < 31*time.Second {
		time.Sleep(time.Second)
	}
	s.hub.publish(event{Kind: "polling"})
	if !readUntilContains(t, resp.Body, `"kind":"polling"`, 5*time.Second) {
		t.Fatal("the stream had closed by 31s — a timeout cut it")
	}
}

// readUntilContains reads from body until want appears or timeout passes,
// reporting whether it was seen.
func readUntilContains(t *testing.T, body io.Reader, want string, timeout time.Duration) bool {
	t.Helper()
	type result struct {
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			n, err := body.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), want) {
					done <- result{true}
					return
				}
			}
			if err != nil {
				done <- result{false}
				return
			}
		}
		done <- result{false}
	}()
	select {
	case r := <-done:
		return r.ok
	case <-time.After(timeout + time.Second):
		return false
	}
}

// A Host this board does not recognise is refused, naming auth.origins; the
// loopback names it always accepts pass. This holds with auth disabled on a
// loopback listen address — no credential is required to reach this check.
func TestHostValidation(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	s, h := newModeServer(t, cfg, r, ModeAuto)
	s.listenHost = "127.0.0.1"
	s.refresh(s.ctx)

	for _, host := range []string{"localhost", "localhost:8090", "127.0.0.1", "127.0.0.1:8090", "[::1]:8090"} {
		code := doReqHost(t, h, "GET", "/api/state", host, nil, nil)
		if code == http.StatusForbidden {
			t.Errorf("Host: %s was refused, want accepted", host)
		}
	}
	code, body := doReqHostBody(t, h, "GET", "/api/state", "evil.example.com")
	if code != http.StatusForbidden {
		t.Errorf("Host: evil.example.com = %d, want 403", code)
	}
	if !strings.Contains(string(body), "auth.origins") {
		t.Errorf("refusal body %q does not name the fix", body)
	}
}

func doReqHostBody(t *testing.T, h http.Handler, method, path, host string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://x"+path, nil)
	req.Host = host
	w := &respRecorder{header: http.Header{}}
	h.ServeHTTP(w, req)
	return w.code, w.buf.Bytes()
}

// PUT /api/order and POST .../start refuse an oversized body with 413 and do
// not apply it; a normal body still works.
func TestOversizedBodyIsRejected(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	s, h := newModeServer(t, cfg, r, ModeAuto)
	s.refresh(s.ctx)

	// Valid JSON, just too much of it: one huge element, so the decoder has to
	// keep reading real content until it runs past the limit rather than
	// failing on the first byte for looking malformed.
	huge := []byte(`["` + strings.Repeat("a", orderBodyLimit+1) + `"]`)
	if code, _ := doReq(t, h, "PUT", "/api/order", huge); code != http.StatusRequestEntityTooLarge {
		t.Errorf("PUT /api/order with an oversized body = %d, want 413", code)
	}
	if got := s.order.IDs(); len(got) != 0 {
		t.Errorf("an oversized order body was still applied: %v", got)
	}

	good := []byte(`["s1:1","s1:2"]`)
	if code, _ := doReq(t, h, "PUT", "/api/order", good); code != http.StatusNoContent {
		t.Errorf("a normal order body = %d, want 204", code)
	}

	hugeStart := []byte(`{"stage":"` + strings.Repeat("a", startBodyLimit+1) + `"}`)
	req, _ := http.NewRequest("POST", "http://x/api/items/s1:1/start", bytes.NewReader(hugeStart))
	req.Host = "localhost"
	req.ContentLength = int64(len(hugeStart))
	w := &respRecorder{header: http.Header{}}
	h.ServeHTTP(w, req)
	if w.code != http.StatusRequestEntityTooLarge {
		t.Errorf("POST .../start with an oversized body = %d, want 413", w.code)
	}
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

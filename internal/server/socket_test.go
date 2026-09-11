package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// unixClient is an http.Client that dials sockPath no matter what host a
// request names — the same trick `curl --unix-socket` plays, and the one a
// real local caller (a script, a Claude session) would use.
func unixClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

// startRealServer runs s.Run against a real loopback TCP listener and a real
// socket, handing back both addresses once startup has actually finished
// with each — the listening/socketListening seams, never a sleep — and
// registers cleanup that cancels and waits for Run to return.
func startRealServer(t *testing.T, cfg *config.Config, r *runner.Runner, mode Mode) (s *Server, tcpAddr, sockPath string) {
	t.Helper()
	s = New(cfg, r)
	s.listening = make(chan string, 1)
	s.socketListening = make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, "127.0.0.1:0", mode) }()
	tcpAddr = <-s.listening
	sockPath = <-s.socketListening
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return within 5s of cancellation during cleanup")
		}
	})
	return s, tcpAddr, sockPath
}

func topLevelKeys(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, body)
	}
	keys := map[string]bool{}
	for k := range m {
		keys[k] = true
	}
	return keys
}

// --- Serving ---

// The socket exists, is a Unix socket, and its permission bits are exactly
// 0600 — filesystem access control, not a password.
func TestSocketExistsWithCorrectPermissions(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	_, _, sockPath := startRealServer(t, cfg, r, ModeAuto)

	fi, err := os.Lstat(sockPath)
	if err != nil {
		t.Fatalf("stat %s: %v", sockPath, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket: mode=%v", sockPath, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("permission bits = %o, want 0600", perm)
	}
}

// GET /api/state over the socket, no Authorization header, returns 200 with
// the same top-level JSON keys an authenticated GET over TCP returns.
func TestSocketServesStateWithoutAuth(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "s")}}
	// Observe mode: nothing advances between the two requests below, so the
	// board's own state cannot drift between them and make the comparison
	// flaky for a reason that has nothing to do with auth.
	s, tcpAddr, sockPath := startRealServer(t, cfg, r, ModeObserve)
	// Run's own poll loop already calls refresh once at startup (discovery.go);
	// calling it again here can lose the single-flight race against that
	// goroutine and return immediately having done nothing, leaving a window
	// where the first request below reads state the second one, moments
	// later, no longer does. Waiting for the listing to actually land is
	// what the "no drift between the two requests" comment above depends on.
	waitFor(t, "the listing to populate blocked item s1:2", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		_, ok := s.blocks["s1:2"]
		return ok
	})

	req, _ := http.NewRequest("GET", "http://localhost/api/state", nil)
	resp, err := unixClient(sockPath).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/state over the socket with no credentials = %d, want 200", resp.StatusCode)
	}
	sockBody, _ := io.ReadAll(resp.Body)

	tcpReq, _ := http.NewRequest("GET", "http://"+tcpAddr+"/api/state", nil)
	tcpReq.SetBasicAuth("amir", "s")
	tcpResp, err := http.DefaultClient.Do(tcpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpResp.Body.Close()
	if tcpResp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /api/state over TCP = %d, want 200", tcpResp.StatusCode)
	}
	tcpBody, _ := io.ReadAll(tcpResp.Body)

	sockKeys, tcpKeys := topLevelKeys(t, sockBody), topLevelKeys(t, tcpBody)
	if len(sockKeys) == 0 {
		t.Fatal("socket response had no top-level keys")
	}
	for k := range tcpKeys {
		if !sockKeys[k] {
			t.Errorf("socket response is missing top-level key %q present over TCP", k)
		}
	}
	for k := range sockKeys {
		if !tcpKeys[k] {
			t.Errorf("socket response has extra top-level key %q not present over TCP", k)
		}
	}
}

// The socket never evaluates credentials: a wrong password and a malformed
// Authorization header both still reach the handler and get 200.
func TestSocketIgnoresWrongAndMalformedAuth(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "s")}}
	s, _, sockPath := startRealServer(t, cfg, r, ModeAuto)
	s.refresh(s.ctx)

	for name, hdr := range map[string]string{
		"wrong password":     "Basic " + basic("amir", "not-the-password"),
		"malformed":          "Basic not-even-base64!!!",
		"unrecognized-realm": "Bearer sometoken",
	} {
		req, _ := http.NewRequest("GET", "http://localhost/api/state", nil)
		req.Header.Set("Authorization", hdr)
		resp, err := unixClient(sockPath).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: GET /api/state over the socket = %d, want 200", name, resp.StatusCode)
		}
	}
}

// GET / over the socket with no credentials returns the board's HTML — the
// whole mux is served on purpose, the same authority as the API.
func TestSocketServesRootHTML(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "s")}}
	_, _, sockPath := startRealServer(t, cfg, r, ModeAuto)

	resp, err := unixClient(sockPath).Get("http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / over the socket = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(strings.ToLower(string(body)), "<html") {
		t.Errorf("GET / over the socket did not return HTML: %s", truncate(body, 200))
	}
}

// POST /api/refresh over the socket with no credentials, in auto mode,
// returns the same status code the TCP route returns with valid credentials.
func TestSocketRefreshStatusMatchesAuthenticatedTCP(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "s")}}
	_, tcpAddr, sockPath := startRealServer(t, cfg, r, ModeAuto)

	tcpReq, _ := http.NewRequest("POST", "http://"+tcpAddr+"/api/refresh", nil)
	tcpReq.SetBasicAuth("amir", "s")
	tcpResp, err := http.DefaultClient.Do(tcpReq)
	if err != nil {
		t.Fatal(err)
	}
	tcpResp.Body.Close()

	sockResp, err := unixClient(sockPath).Post("http://localhost/api/refresh", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sockResp.Body.Close()

	if sockResp.StatusCode != tcpResp.StatusCode {
		t.Errorf("POST /api/refresh over the socket = %d, authenticated TCP = %d, want equal",
			sockResp.StatusCode, tcpResp.StatusCode)
	}
}

// GET /api/events over the socket with no credentials delivers at least one
// SSE event.
func TestSocketEventsDeliversAnEvent(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "s")}}
	s, _, sockPath := startRealServer(t, cfg, r, ModeAuto)

	req, _ := http.NewRequest("GET", "http://localhost/api/events", nil)
	resp, err := unixClient(sockPath).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/events over the socket = %d, want 200", resp.StatusCode)
	}

	s.hub.publish(event{Kind: "state"})
	if !readUntilContains(t, resp.Body, `"kind":"state"`, 5*time.Second) {
		t.Fatal("never received the event over the socket")
	}
}

// With no auth.users configured (loopback bind, allowed today), the socket
// is still created and serves the same way, and TCP behaviour is unchanged
// (open, no credentials required).
func TestSocketWithNoAuthUsersConfigured(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	_, tcpAddr, sockPath := startRealServer(t, cfg, r, ModeAuto)

	resp, err := unixClient(sockPath).Get("http://localhost/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/state over the socket = %d, want 200", resp.StatusCode)
	}

	tcpResp, err := http.Get("http://" + tcpAddr + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	tcpResp.Body.Close()
	if tcpResp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/state over TCP with no auth.users = %d, want 200", tcpResp.StatusCode)
	}
}

// The startup banner names the socket path and says it needs no password;
// when no socket was created (something else at the path), that line is
// absent.
func TestStartupBannerNamesSocketPathWhenCreated(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	stdout := captureStdout(t)
	_, _, sockPath := startRealServer(t, cfg, r, ModeAuto)
	waitFor(t, "the banner to be printed", func() bool { return stdout() != "" })
	out := stdout()

	if !strings.Contains(out, sockPath) {
		t.Errorf("banner = %q, want it to name the socket path %q", out, sockPath)
	}
	if !strings.Contains(out, "needs no password") {
		t.Errorf("banner = %q, want it to say the socket needs no password", out)
	}
}

func TestStartupBannerOmitsSocketLineWhenNoneWasCreated(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	sp := socketPath(cfg)
	if err := os.MkdirAll(filepath.Dir(sp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sp, 0o700); err != nil { // a directory: never a socket
		t.Fatal(err)
	}

	stdout := captureStdout(t)
	s := New(cfg, r)
	s.listening = make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, "127.0.0.1:0", ModeAuto) }()
	<-s.listening
	// No socketListening seam set: give the socket-preparation step, which
	// runs synchronously right after the TCP bind, a moment to finish and the
	// banner to print before asserting on it.
	waitFor(t, "the banner to be printed", func() bool { return strings.Contains(stdout(), "conveyor: http://") })
	cancel()
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	out := stdout()
	if strings.Contains(out, "needs no password") {
		t.Errorf("banner = %q, want no socket line: nothing was ever created at %s", out, sp)
	}
}

// --- Every other layer still applies over the socket ---

// In observe mode, POST /api/tick over the socket returns 403, as it does
// over TCP, and nothing ran.
func TestSocketObserveModeRefusesMutation(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, stageRuns, moveRuns, _, _ := modePipeline(t)
	_, _, sockPath := startRealServer(t, cfg, r, ModeObserve)

	resp, err := unixClient(sockPath).Post("http://localhost/api/tick", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /api/tick over the socket in observe mode = %d, want 403", resp.StatusCode)
	}
	if n := countLines(stageRuns); n != 0 {
		t.Errorf("%d stage run(s) happened, want 0", n)
	}
	if n := countLines(moveRuns); n != 0 {
		t.Errorf("%d move(s) happened, want 0", n)
	}
}

// A socket request with an unrecognised Host, or a cross-site POST
// /api/tick, is refused before it reaches a handler — same as TCP.
func TestSocketHostAndCrossSiteChecksApply(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	_, _, sockPath := startRealServer(t, cfg, r, ModeAuto)

	req, _ := http.NewRequest("GET", "http://localhost/api/state", nil)
	req.Host = "evil.example"
	resp, err := unixClient(sockPath).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /api/state over the socket with Host: evil.example = %d, want 403", resp.StatusCode)
	}

	req2, _ := http.NewRequest("POST", "http://localhost/api/tick", nil)
	req2.Header.Set("Sec-Fetch-Site", "cross-site")
	resp2, err := unixClient(sockPath).Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("POST /api/tick over the socket with Sec-Fetch-Site: cross-site = %d, want 403", resp2.StatusCode)
	}
}

// --- TCP is not loosened ---

// From a loopback TCP peer, GET /api/state with no credentials still 401s —
// with the default Host, with an explicit loopback Host, and with a
// configured auth.origins Host. This is the regression guard for the proxy
// case: to a loopback-peer check every request from Caddy or cloudflared
// looks local too, so nothing about the peer being loopback may exempt it.
func TestTCPStillRequiresAuthRegardlessOfHost(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	cfg.Auth = config.Auth{
		Users:   map[string]string{"amir": hashed(t, "s")},
		Origins: []string{"https://board.example"},
	}
	_, tcpAddr, _ := startRealServer(t, cfg, r, ModeAuto)

	for _, host := range []string{"", tcpAddr, "board.example"} {
		req, _ := http.NewRequest("GET", "http://"+tcpAddr+"/api/state", nil)
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET /api/state over TCP, Host=%q, no credentials = %d, want 401", host, resp.StatusCode)
		}
	}
}

// serve with a non-loopback -addr and no auth.users is still refused, and no
// socket is touched: not created, and an existing file at the path is left
// exactly as it was.
func TestNonLoopbackNoAuthIsRefusedAndSocketPathUntouched(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	sp := socketPath(cfg)
	if err := os.MkdirAll(filepath.Dir(sp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp, []byte("pre-existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(cfg, r)
	err := s.Run(context.Background(), "0.0.0.0:0", ModeManual)
	if err == nil || !strings.Contains(err.Error(), "refusing to serve") {
		t.Fatalf("Run on a non-loopback address with no auth = %v, want a refusal", err)
	}

	got, err := os.ReadFile(sp)
	if err != nil {
		t.Fatalf("the pre-existing file at the socket path is gone: %v", err)
	}
	if string(got) != "pre-existing" {
		t.Errorf("the pre-existing file at the socket path was modified: %q", got)
	}
}

// --- Startup edge cases ---

// The TCP listener binds before the socket is touched: a TCP bind failure
// returns that error and never creates or touches api.sock.
func TestTCPBindsBeforeSocketIsTouched(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	sp := socketPath(cfg)
	if err := os.MkdirAll(filepath.Dir(sp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp, []byte("pre-existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	s := New(cfg, r)
	if err := s.Run(context.Background(), taken.Addr().String(), ModeObserve); err == nil {
		t.Fatal("Run on an address already in use returned nil, want an error")
	}

	got, err := os.ReadFile(sp)
	if err != nil {
		t.Fatalf("the pre-existing file at the socket path is gone: %v", err)
	}
	if string(got) != "pre-existing" {
		t.Errorf("the pre-existing file at the socket path was modified: %q", got)
	}
}

// A Unix socket file that refuses connections, as a process killed with
// SIGKILL leaves one, is removed and replaced by a freshly bound socket.
func TestPrepareSocketReplacesAStaleSocket(t *testing.T) {
	skipWithoutUnix(t)
	dir := shortTempDir(t)
	sp := filepath.Join(dir, "api.sock")

	addr, err := net.ResolveUnixAddr("unix", sp)
	if err != nil {
		t.Fatal(err)
	}
	dead, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	dead.SetUnlinkOnClose(false)
	dead.Close() // the file is left behind, nothing is listening: stale

	ln, err := prepareSocket(sp)
	if err != nil {
		t.Fatalf("prepareSocket on a stale socket: %v", err)
	}
	defer ln.Close()

	fi, err := os.Lstat(sp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket after replacing the stale one", sp)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("permission bits = %o, want 0600", fi.Mode().Perm())
	}
}

// A Unix socket that accepts a connection is not removed: prepareSocket
// declines with an error naming why, and leaves the live socket in place.
func TestPrepareSocketLeavesALiveSocketAlone(t *testing.T) {
	skipWithoutUnix(t)
	dir := shortTempDir(t)
	sp := filepath.Join(dir, "api.sock")

	addr, err := net.ResolveUnixAddr("unix", sp)
	if err != nil {
		t.Fatal(err)
	}
	live, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	go func() {
		for {
			c, err := live.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	_, err = prepareSocket(sp)
	if err == nil {
		t.Fatal("prepareSocket on a live socket returned no error, want a decline")
	}

	fi, statErr := os.Lstat(sp)
	if statErr != nil {
		t.Fatalf("the live socket file is gone: %v", statErr)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Error("the live socket was replaced by something that is not a socket")
	}
}

// A regular file, a directory, and a symlink at the socket path are each
// left exactly as they are: not removed, not followed, not modified.
func TestPrepareSocketLeavesNonSocketsAlone(t *testing.T) {
	skipWithoutUnix(t)
	dir := shortTempDir(t)

	regular := filepath.Join(dir, "regular.sock")
	if err := os.WriteFile(regular, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSocket(regular); err == nil {
		t.Error("prepareSocket on a regular file returned no error, want a decline")
	}
	if got, err := os.ReadFile(regular); err != nil || string(got) != "hi" {
		t.Errorf("the regular file was touched: content=%q err=%v", got, err)
	}

	dirPath := filepath.Join(dir, "adir.sock")
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSocket(dirPath); err == nil {
		t.Error("prepareSocket on a directory returned no error, want a decline")
	}
	if fi, err := os.Stat(dirPath); err != nil || !fi.IsDir() {
		t.Errorf("the directory was touched: %v %v", fi, err)
	}

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link.sock")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSocket(symlink); err == nil {
		t.Error("prepareSocket on a symlink returned no error, want a decline")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "elsewhere" {
		t.Errorf("the symlink's target was touched: content=%q err=%v", got, err)
	}
	if lfi, err := os.Lstat(symlink); err != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink itself was replaced: %v %v", lfi, err)
	}
}

// A path too long for AF_UNIX's sun_path is a "cannot create" failure like
// any other: prepareSocket declines with an error, nothing panics.
func TestPrepareSocketFailsOnAnOverlongPath(t *testing.T) {
	skipWithoutUnix(t)
	dir := shortTempDir(t)
	over := filepath.Join(dir, strings.Repeat("x", 200)+".sock")
	if _, err := prepareSocket(over); err == nil {
		t.Error("prepareSocket on an overlong path returned no error, want a decline")
	}
}

// A chmod failure after bind closes the listener without serving any
// request, removes the socket file, and reports the failure — nothing is
// ever reachable over a socket whose mode is not 0600.
func TestPrepareSocketChmodFailureCleansUp(t *testing.T) {
	skipWithoutUnix(t)
	dir := shortTempDir(t)
	sp := filepath.Join(dir, "api.sock")

	boom := errors.New("boom")
	old := chmodSocket
	chmodSocket = func(string, os.FileMode) error { return boom }
	defer func() { chmodSocket = old }()

	ln, err := prepareSocket(sp)
	if err == nil {
		ln.Close()
		t.Fatal("prepareSocket with a failing chmod returned no error")
	}
	if _, statErr := os.Lstat(sp); !os.IsNotExist(statErr) {
		t.Errorf("the socket file was left behind after a chmod failure: err=%v", statErr)
	}
}

// serveSocket's other half: a Serve failure that is not http.ErrServerClosed
// (something closed the raw listener out from under it, standing in for any
// runtime failure) prints a warning and removes the socket file.
func TestServeSocketRuntimeFailureWarnsAndRemovesFile(t *testing.T) {
	skipWithoutUnix(t)
	dir := shortTempDir(t)
	sp := filepath.Join(dir, "api.sock")

	ln, err := prepareSocket(sp)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler()}

	stderr := captureStderr(t)
	done := make(chan struct{})
	go func() {
		s := &Server{}
		s.serveSocket(srv, ln, sp)
		close(done)
	}()
	// Close the raw listener directly, not through srv.Close()/Shutdown(): the
	// server never learns it is shutting down, so Serve returns its own
	// "use of closed network connection" rather than http.ErrServerClosed.
	// Correct regardless of whether Serve's Accept loop has started yet —
	// Accept on an already-closed listener fails immediately too.
	time.Sleep(20 * time.Millisecond)
	ln.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveSocket did not return after its listener was closed")
	}

	waitFor(t, "the warning to reach stderr", func() bool { return strings.Contains(stderr(), sp) })
	if _, statErr := os.Lstat(sp); !os.IsNotExist(statErr) {
		t.Errorf("the socket file was left behind after a runtime failure: err=%v", statErr)
	}
}

// The TCP side's half of the same guarantee, exercised through Run rather
// than serveSocket directly: a runtime failure of the TCP http.Server's
// Serve (its listener closed out from under it, never through srv.Close())
// makes Run return that error, and drain — which Run's defers still run on
// an error return — waits for the socket's own goroutine to close and remove
// its file before Run hands the error back.
func TestTCPServeFailureClosesSocketAndRemovesFile(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	s := New(cfg, r)
	s.listening = make(chan string, 1)
	s.socketListening = make(chan string, 1)
	s.drainGrace = 5 * time.Second

	lnCh := make(chan net.Listener, 1)
	old := netListen
	netListen = func(network, addr string) (net.Listener, error) {
		ln, err := old(network, addr)
		if err == nil {
			lnCh <- ln
		}
		return ln, err
	}
	defer func() { netListen = old }()

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(context.Background(), "127.0.0.1:0", ModeAuto) }()

	<-s.listening
	sockPath := <-s.socketListening
	if sockPath == "" {
		t.Fatal("no socket was created; nothing to prove was cleaned up")
	}
	if _, err := os.Lstat(sockPath); err != nil {
		t.Fatalf("socket does not exist right after startup: %v", err)
	}

	tcpLn := <-lnCh
	// Closing the raw listener directly, not through srv.Close(): the
	// http.Server never learns it is shutting down, so its Serve returns its
	// own error rather than http.ErrServerClosed — a genuine runtime failure.
	tcpLn.Close()

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("Run returned nil after its TCP listener failed at runtime, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its TCP listener failed at runtime")
	}

	if _, err := os.Lstat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after a TCP Serve failure: err=%v", err)
	}
}

// The socket side's half: a runtime failure of the socket's own Serve (its
// listener closed out from under it) prints a warning and removes the socket
// file, exactly like TestServeSocketRuntimeFailureWarnsAndRemovesFile proves
// in isolation — but TCP, run through the same Run call, is untouched and
// keeps serving.
func TestSocketServeFailureLeavesTCPServing(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	s := New(cfg, r)
	s.listening = make(chan string, 1)
	s.socketListening = make(chan string, 1)
	s.drainGrace = 5 * time.Second

	sockLnCh := make(chan *net.UnixListener, 1)
	old := listenUnix
	listenUnix = func(network string, addr *net.UnixAddr) (*net.UnixListener, error) {
		ln, err := old(network, addr)
		if err == nil {
			sockLnCh <- ln
		}
		return ln, err
	}
	defer func() { listenUnix = old }()

	// ModeManual, not ModeAuto: mode.Runs() is what spawns the retention
	// sweep, which writes to os.Stderr in the background on its own timer —
	// racing this test's own captureStderr redirection. Mutation is
	// irrelevant to what this test checks (TCP still answering), so the mode
	// that avoids the race is the right one, not an accommodation of it.
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, "127.0.0.1:0", ModeManual) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return within 5s of cleanup's cancellation")
		}
	})

	tcpAddr := <-s.listening
	sockPath := <-s.socketListening
	if sockPath == "" {
		t.Fatal("no socket was created; nothing to fail")
	}
	sockLn := <-sockLnCh

	stderr := captureStderr(t)
	sockLn.Close() // out from under sockSrv.Serve, not through its own Close()

	waitFor(t, "the socket failure warning to reach stderr", func() bool {
		return strings.Contains(stderr(), sockPath)
	})
	waitFor(t, "the socket file to be removed", func() bool {
		_, err := os.Lstat(sockPath)
		return os.IsNotExist(err)
	})

	select {
	case err := <-runErr:
		t.Fatalf("Run returned (err=%v) after only the socket failed; TCP should still be serving", err)
	case <-time.After(200 * time.Millisecond):
	}

	resp, err := http.Get("http://" + tcpAddr + "/api/state")
	if err != nil {
		t.Fatalf("TCP no longer serving after the socket's runtime failure: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/state over TCP after the socket failed = %d, want 200 (no auth.users configured)", resp.StatusCode)
	}
}

// A request over the socket that is still in its handler at cancellation is
// counted in shutdownWork exactly like a TCP request, so drain waits for it —
// run against the real socket chain handler() returns and the real tracked
// wrapper Run wires it through, not a stand-in handler.
func TestSocketRequestInFlightIsCountedForDrain(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	s := New(cfg, r)
	s.cop = http.NewCrossOriginProtection()
	s.drainGrace = 5 * time.Second
	_, sockHandler, err := s.handler()
	if err != nil {
		t.Fatal(err)
	}
	wrapped := s.tracked(sockHandler)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "http://localhost/api/events", nil).WithContext(reqCtx)
	done := make(chan struct{})
	go func() {
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	waitFor(t, "the in-flight socket request to be counted", func() bool { return s.shutdownWork.Load() != 0 })

	drained := make(chan struct{})
	go func() { s.drain(); close(drained) }()

	select {
	case <-drained:
		t.Fatal("drain returned while the socket's in-flight request was still running")
	case <-time.After(200 * time.Millisecond):
	}

	cancelReq()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not return once its request context was cancelled")
	}
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not return once the socket's in-flight request finished")
	}
}

// On every return from Run after the socket was created, the socket file no
// longer exists — checked here against the ordinary shutdown path (context
// cancellation), the common case.
func TestSocketFileRemovedOnEveryReturnFromRun(t *testing.T) {
	skipWithoutUnix(t)
	cfg, r, _, _, _, _ := modePipeline(t)
	s := New(cfg, r)
	s.listening = make(chan string, 1)
	s.socketListening = make(chan string, 1)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, "127.0.0.1:0", ModeAuto) }()
	<-s.listening
	sockPath := <-s.socketListening
	if sockPath == "" {
		t.Fatal("no socket was created; nothing to prove was cleaned up")
	}
	if _, err := os.Lstat(sockPath); err != nil {
		t.Fatalf("socket does not exist right after startup: %v", err)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	if _, err := os.Lstat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Run returned: err=%v", err)
	}
}

// --- helpers ---

func skipWithoutUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("AF_UNIX is not exercised on this platform")
	}
}

// shortTempDir is t.TempDir() rebased under os.TempDir() with a short,
// content-free name: a deep t.TempDir() path (which embeds the test name)
// can exceed AF_UNIX's ~108-byte sun_path limit on its own.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cvsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// syncBuf is an io.Writer safe for one goroutine to append to while another
// reads a snapshot — captureStream copies into one continuously, in the
// background, so reading the snapshot never itself consumes the pipe.
type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureStream redirects *stream (os.Stdout or os.Stderr) for the duration
// of the test and returns a function reading everything written so far.
func captureStream(t *testing.T, stream **os.File) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := *stream
	*stream = w
	sb := &syncBuf{}
	copyDone := make(chan struct{})
	go func() {
		io.Copy(sb, r)
		close(copyDone)
	}()
	t.Cleanup(func() {
		*stream = old
		w.Close()
		<-copyDone
	})
	return sb.String
}

func captureStdout(t *testing.T) func() string { return captureStream(t, &os.Stdout) }
func captureStderr(t *testing.T) func() string { return captureStream(t, &os.Stderr) }

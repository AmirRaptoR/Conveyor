package server

import (
	"context"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Run serves until ctx is done. auto drives the pipeline; without it the server
// only ever reads.
//
// The guarantee it makes: when Run returns, nothing it started is still
// writing under the data directory. cmdServe releases the owner lock in a
// defer the instant Run returns, so that guarantee is the whole reason a
// restarted engine never takes the lock out from under a run its predecessor
// left going — a run directory half-created, a label half-written, an
// order.json mid-write. It holds on every return path, including a refusal
// that never started anything and a listen error that leaves the caller with
// no reason of its own to cancel ctx: Run derives its own cancellable context
// here and cancels it before it returns, rather than only reacting to the
// caller's.
func (s *Server) Run(ctx context.Context, addr string, auto bool) error {
	// The board is a control plane: it starts agent runs, reorders work and
	// hands marked items back. Reaching it is enough to drive every repository
	// the config enrols, so an open one on a public interface is not a
	// read-only inconvenience. This refuses rather than warns, because the
	// mistake it prevents is silent and the fix is one command.
	//
	// Checked before anything below starts: the loops read and write through
	// ctx, not through this call's error return, so launching them ahead of a
	// refusal left every one of them running regardless — on
	// context.Background() if the caller never cancelled it, forever.
	if !s.cfg.Auth.Enabled() && !loopback(addr) {
		return fmt.Errorf("refusing to serve %s with no auth: configure auth.users "+
			"(run `conveyor passwd <name>` for a line to paste) or bind a loopback address", addr)
	}

	// Run's own lifetime, derived rather than reused: cancelling it is what
	// stops every loop and handler-spawned goroutine on every return path
	// below, whether or not the caller's ctx was ever cancelled. drain() runs
	// after cancel(), never before — defer unwinds last-registered-first, so
	// the loops are already unwinding by the time drain starts waiting on
	// them.
	runCtx, cancel := context.WithCancel(ctx)
	defer s.drain()
	defer cancel()
	s.ctx = runCtx

	// Three loops, and they are separate on purpose. Discovery must keep its
	// interval while a 90-minute stage runs, so nothing that waits for work to
	// finish may share a goroutine with it. Each goes through spawn so drain
	// waits for it to actually exit, not just for ctx to be cancelled.
	s.spawn(func() { s.poll(runCtx) })
	s.spawn(func() { s.button(runCtx, auto) })
	if auto {
		s.spawn(func() { s.schedule(runCtx) })
		if d := s.cfg.RetryStalled.D(); d > 0 {
			s.spawn(func() { s.stalled(runCtx, d) })
		}
		s.spawn(func() { s.sweep(runCtx) })
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleRun)
	mux.HandleFunc("GET /api/items/{id}/report", s.handleReport)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/tick", s.handleTick)
	mux.HandleFunc("PUT /api/order", s.handleOrder)
	mux.HandleFunc("POST /api/items/{id}/start", s.handleStart)
	mux.HandleFunc("POST /api/items/{id}/unblock", s.handleUnblock)
	mux.HandleFunc("POST /api/unblock", s.handleUnblockAll)
	mux.HandleFunc("POST /api/doctor", s.handleDoctorStart)
	mux.HandleFunc("GET /api/doctor", s.handleDoctorGet)
	mux.HandleFunc("GET /api/push/key", s.handlePushKey)
	mux.HandleFunc("POST /api/push/subscribe", s.handlePushSubscribe)
	mux.HandleFunc("POST /api/push/unsubscribe", s.handlePushUnsubscribe)
	mux.HandleFunc("POST /api/push/test", s.handlePushTest)

	static, err := webHandler()
	if err != nil {
		return err
	}
	mux.Handle("/", static)

	srv := &http.Server{Addr: addr, Handler: s.tracked(s.authed(mux))}
	go func() { <-runCtx.Done(); _ = srv.Close() }()
	mode := "running: items advance on their own"
	if !auto {
		mode = "watching only: -watch is set, nothing will advance"
	}
	if s.cfg.Auth.Enabled() {
		mode += "\n  basic auth on, " + strconv.Itoa(len(s.cfg.Auth.Users)) + " user(s)"
	}
	// ":8080" means every interface, so name a host you can actually open;
	// "127.0.0.1:8090" already names one and must not have a second glued on.
	shown := addr
	if strings.HasPrefix(addr, ":") {
		shown = "localhost" + addr
	}
	fmt.Printf("conveyor: http://%s\n  %s\n", shown, mode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		// The deferred cancel() and drain() above still run on this path: a
		// bind failure must stop the loops just launched, not leave them on
		// the caller's ctx with nothing telling it to cancel.
		return err
	}
	// The HTTP listener is down the moment runCtx is cancelled, but a
	// transition already launched keeps running — a stage script, an agent, a
	// git push — and so does discovery, a doctor sweep, a push send, any
	// request still writing order.json or answers.json. The deferred cancel()
	// stops every loop above; the deferred drain() is what actually waits for
	// all of that, transitions and everything else, before Run hands back
	// control to cmdServe's defer that releases the data-directory lock.
	return nil
}

// webHandler serves the embedded web/ directory: the board's markup, the
// stylesheets and the ES modules it links, and the app shell beside them. Its
// own function rather than two lines inside Run so a test can drive exactly
// what Run mounts instead of a second copy of it.
func webHandler() (http.Handler, error) {
	// Go's table has no entry for the manifest extension and would serve it
	// as text; Chrome wants the manifest type before it offers to install.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}
	return http.FileServer(http.FS(sub)), nil
}

// drainGrace bounds how long shutdown waits for in-flight transitions before
// giving up and returning anyway. It must exceed the runner's own grace
// period (30s): a script whose child ignores TERM is not reaped until the
// runner's own SIGKILL lands, and draining any less than that would time out
// on exactly the case it exists for.
const drainGrace = 45 * time.Second

// drain waits for every transition already claimed — schedule's launches,
// handleStart, and the tick button's own advance — and every other goroutine
// Run started that still has work outstanding — the loops themselves, a
// background refresh or doctor sweep, a push send, a request still writing to
// the data directory — to finish, or gives up after drainGrace and says what
// is still running rather than hanging forever on a run that will not.
//
// This is what makes "Run has returned" mean "the owner lock is safe to
// release": cmdServe's defer does that the instant Run returns, so nothing
// counted here may still be writing under the data directory once drain does.
func (s *Server) drain() {
	deadline := time.Now().Add(s.drainGrace)
	for (s.inFlight.Load() != 0 || s.shutdownWork.Load() != 0) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	n, other := s.inFlight.Load(), s.shutdownWork.Load()
	switch {
	case n == 0 && other == 0:
		return
	case n == 0:
		// Nothing this counter can name — a loop still unwinding, a push send,
		// a request mid-write. "still running: " followed by an empty list
		// would claim a transition that does not exist; say only the count.
		fmt.Fprintf(os.Stderr, "conveyor: shutting down with %d task(s) still running after %s\n",
			other, s.drainGrace)
	case other == 0:
		var running []string
		for _, a := range s.activeList() {
			running = append(running, a.ItemID)
		}
		fmt.Fprintf(os.Stderr, "conveyor: shutting down with %d transition(s) still running after %s: %s\n",
			n, s.drainGrace, strings.Join(running, ", "))
	default:
		// Named, because inFlight is exactly the transitions activeList can
		// name. other has no item to name, so it is folded into the count
		// rather than pretended into the list.
		var running []string
		for _, a := range s.activeList() {
			running = append(running, a.ItemID)
		}
		fmt.Fprintf(os.Stderr, "conveyor: shutting down with %d transition(s) and %d other task(s) still running after %s: %s\n",
			n, other, s.drainGrace, strings.Join(running, ", "))
	}
}

// spawn runs f in its own goroutine, counted in shutdownWork until it
// returns. Every goroutine Run starts directly (a loop) or a handler spawns
// that runs a script or writes under the data directory (a background
// refresh, a doctor sweep, unblockAll, a push send) goes through this, so
// drain actually waits for it instead of only for a transition.
func (s *Server) spawn(f func()) {
	s.shutdownWork.Add(1)
	go func() {
		defer s.shutdownWork.Add(-1)
		f()
	}()
}

// tracked counts a request as outstanding shutdown work for as long as its
// handler is running — the same idiom as webHandler and authed, so a test can
// drive it directly. A request that decides to write, order.json,
// answers.json, push.json, does so synchronously inside its handler; counting
// the handler's own lifetime is what closes the gap between that write
// starting and drain knowing to wait for it, and closes the same gap in
// handleStart between its ctx.Err() check and claiming a slot: the request is
// already counted before either runs.
func (s *Server) tracked(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.shutdownWork.Add(1)
		defer s.shutdownWork.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// Addr normalises a listen address so `-addr 8080` works like `-addr :8080`.
// authed puts basic auth in front of everything, or nothing in front of
// anything. There is no per-route exemption on purpose: every route either
// reads the state of the repositories or changes it, and a health endpoint
// nobody asked for would be the first hole in a wall one line high.
//
// It replaces a reverse proxy that did the same job in a second process with a
// second config file and a second password store. What the proxy added beyond
// this — terminating the connection somewhere else — is not something a board
// bound to one machine needed.
func (s *Server) authed(next http.Handler) http.Handler {
	if !s.cfg.Auth.Enabled() {
		return next
	}
	realm := s.cfg.Auth.Realm
	if realm == "" {
		realm = "conveyor"
	}
	// A realm is quoted into a header, so a quote or newline in one would let a
	// config file write the rest of the header. Not a threat here — the config
	// is the operator's own — but a cheap thing to be right about.
	realm = strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(realm)
	deny := func(w http.ResponseWriter) {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The app's own shell is public: a browser fetches the manifest with
		// no credentials and will not offer to install behind a 401, and the
		// icons and worker script are static code that reveals nothing. The
		// board, its state and every action stay behind the password.
		if r.Method == http.MethodGet && publicAsset[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || !s.cfg.Auth.Check(user, pass) {
			deny(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var publicAsset = map[string]bool{
	"/manifest.webmanifest": true, "/sw.js": true,
	"/icon.svg": true, "/icon-192.png": true, "/icon-512.png": true,
}

// loopback reports whether this listen address reaches only this machine.
//
// A bare port (":8080") does not: it is every interface, which is the case
// worth being strict about, because it is also the default.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func Addr(a string) string {
	if a == "" {
		return ":8080"
	}
	if !strings.Contains(a, ":") {
		return ":" + a
	}
	return a
}

package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
)

// socketDialTimeout bounds how long startup spends deciding whether an
// existing socket file is live before removing it as stale — short, because
// it only has to distinguish "nothing is listening" from "something is."
const socketDialTimeout = 200 * time.Millisecond

// chmodSocket is os.Chmod, indirected so a test can make the chmod-after-bind
// step fail without needing a filesystem that actually refuses it — the
// "chmod fails after bind" criterion has no other way to reproduce.
var chmodSocket = os.Chmod

// listenUnix is net.ListenUnix, indirected the same way so a test can force
// the socket's Serve loop into a genuine runtime failure — closing the
// listener it returns directly, never through the socket's own http.Server —
// the only way to reproduce a runtime Serve failure for it, same reasoning as
// netListen (serve.go) on the TCP side.
var listenUnix = net.ListenUnix

// socketPath is where the local, no-auth Unix socket lives: beside
// order.json and owner.lock in the data directory, so its access control —
// the file mode — inherits that directory's own 0700 for the bind-to-chmod
// gap and gets its own 0600 on top.
func socketPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir(), "api.sock")
}

// prepareSocket binds a Unix socket at path with permission bits exactly
// 0600, replacing a stale socket a killed process left behind but never
// touching a live one or anything at path that is not a socket at all. A
// non-nil error names exactly why no socket was provided; the caller prints
// one warning and runs without one — nothing becomes reachable without auth.
func prepareSocket(path string) (*net.UnixListener, error) {
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, err
	}
	ln, err := listenUnix("unix", addr)
	if err != nil {
		return nil, err
	}
	// net.ListenUnix unlinks its path on Close by default. Cleanup here is
	// explicit (removeSocketFile, called only while the path still resolves
	// to a socket), so a file someone else puts at path after startup is
	// never removed out from under them.
	ln.SetUnlinkOnClose(false)
	if err := chmodSocket(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return nil, fmt.Errorf("chmod: %w", err)
	}
	return ln, nil
}

// clearStaleSocket decides what, if anything, already sits at path: nothing
// (fine to bind fresh), a stale socket a dead process left behind (removed),
// a live one another process is still serving (left alone; returns an error
// so the caller declines to bind), or something else — a regular file, a
// directory, a symlink — which is never removed, followed, or modified.
func clearStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Lstat, not Stat: a symlink at path must be judged on what it is, not
	// followed to see what it points at.
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; leaving it alone", path)
	}
	conn, err := net.DialTimeout("unix", path, socketDialTimeout)
	if err == nil {
		conn.Close()
		return fmt.Errorf("%s is already in use by a running process", path)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		if rmErr := os.Remove(path); rmErr != nil {
			return fmt.Errorf("removing stale socket: %w", rmErr)
		}
		return nil
	}
	return fmt.Errorf("checking existing socket: %w", err)
}

// removeSocketFile removes path only while it still resolves to a socket —
// the same positive-allowlist rule the rest of this codebase applies to
// touching a path (CLAUDE.md, worktree reclaim): something a person or
// another process put at path after startup — a regular file, a symlink — is
// left exactly as it is.
func removeSocketFile(path string) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return
	}
	_ = os.Remove(path)
}

// serveSocket runs the socket's http.Server until it returns for any reason,
// then removes the socket file. A return other than http.ErrServerClosed is
// a runtime failure — Serve stopped without anyone asking it to — and gets a
// warning; a clean Close (shutdown, or the TCP side failing and closing this
// one down with it) does not. Cleanup happens either way, so every return
// path leaves no socket file behind.
func (s *Server) serveSocket(srv *http.Server, ln *net.UnixListener, path string) {
	err := srv.Serve(ln)
	if err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "conveyor: %s: %v\n", path, err)
	}
	removeSocketFile(path)
}

// signalSocket reports to a test seam that startup's socket step has
// finished — path if a socket was bound, "" if not — so a test never races
// Run's own goroutine ordering. Nil in production; Run skips the send.
func (s *Server) signalSocket(path string) {
	if s.socketListening != nil {
		select {
		case s.socketListening <- path:
		default:
		}
	}
}

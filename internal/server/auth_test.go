package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
)

func hashed(t *testing.T, pass string) string {
	t.Helper()
	h, err := config.Hash(pass)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The wall, and that it has no door in it. Every route either reads the state
// of the repositories or changes it, so there is no route that may answer
// without credentials — a health endpoint nobody asked for would be the first
// hole in a wall one line high.
func TestEveryRouteIsBehindTheSameWall(t *testing.T) {
	cfg, r, _ := pipelineFor(t)
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "correct horse")}}
	s := New(cfg, r)
	s.ctx = context.Background()

	reached := false
	h := s.authed(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/", "/api/state", "/api/events", "/api/runs", "/icon.svg"} {
		reached = false
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without credentials = %d, want 401", path, w.Code)
		}
		if reached {
			t.Errorf("GET %s reached the handler unauthenticated", path)
		}
		// And the browser is told how to ask, or it cannot prompt.
		if ch := w.Header().Get("WWW-Authenticate"); !strings.HasPrefix(ch, "Basic ") {
			t.Errorf("GET %s challenge = %q, want a Basic challenge", path, ch)
		}
	}

	// The right password gets through; a wrong one does not.
	for _, tc := range []struct {
		user, pass string
		want       int
	}{
		{"amir", "correct horse", http.StatusOK},
		{"amir", "correct horse ", http.StatusUnauthorized},
		{"amir", "", http.StatusUnauthorized},
		{"amir", "CORRECT HORSE", http.StatusUnauthorized},
		{"nobody", "correct horse", http.StatusUnauthorized},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.SetBasicAuth(tc.user, tc.pass)
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s/%q = %d, want %d", tc.user, tc.pass, w.Code, tc.want)
		}
	}
}

// With no users configured the board is open, which is correct for a loopback
// address and is what a fresh checkout does.
func TestNoUsersMeansNoWall(t *testing.T) {
	cfg, r, _ := pipelineFor(t)
	s := New(cfg, r)
	h := s.authed(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/state", nil))
	if w.Code != http.StatusTeapot {
		t.Errorf("an unconfigured board answered %d; it should be open on loopback", w.Code)
	}
}

// The mistake worth refusing rather than warning about. The board starts agent
// runs, reorders work and hands marked items back, so an open one on a public
// interface is not a read-only inconvenience — and ":7788" is both the shape
// that reaches the world and the shape people type.
func TestServingOpenOffLoopbackIsRefused(t *testing.T) {
	cfg, r, _ := pipelineFor(t)
	s := New(cfg, r)

	for _, addr := range []string{":7788", "0.0.0.0:7788", "[::]:7788", "192.168.1.10:7788"} {
		err := s.Run(context.Background(), addr, false)
		if err == nil || !strings.Contains(err.Error(), "refusing to serve") {
			t.Errorf("Run(%q) with no auth = %v, want a refusal", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090"} {
		if !loopback(addr) {
			t.Errorf("loopback(%q) = false; it reaches only this machine", addr)
		}
	}
	// Configured, and the same address is allowed. Run blocks once it serves,
	// so this only asserts it got past the guard.
	cfg.Auth = config.Auth{Users: map[string]string{"amir": hashed(t, "s")}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx, ":0", false); err != nil && strings.Contains(err.Error(), "refusing to serve") {
		t.Errorf("a configured board was still refused: %v", err)
	}
}

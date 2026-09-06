package server

import (
	"sync"
	"time"
)

// authVerifier bounds the cost of password verification.
//
// Auth.Check runs 600,000 PBKDF2 iterations — real CPU, paid on every
// unauthenticated request and every SSE reconnect. Two things keep a flood of
// requests from turning into a flood of derivations: concurrent checks of the
// exact same (user, password) pair join the one already in flight instead of
// each deriving their own key, and a pair that verified correctly is
// remembered for cacheTTL so the same browser's next request (or reconnect)
// skips derivation entirely.
//
// cacheTTL is the "documented lifetime" a password change is honoured within:
// an operator who edits auth.users and hands out a new password invalidates
// the old one across every process restart, and within cacheTTL of the last
// process that had it cached. Wrong passwords are never cached — only a
// verified pair is, so this never turns into an offline dictionary check.
type authVerifier struct {
	check func(user, pass string) bool
	ttl   time.Duration

	mu     sync.Mutex
	good   map[string]time.Time // "user\x00pass" -> expiry, verified pairs only
	inHand map[string]*verifyCall
	// derivations counts calls into check — the thing an attacker is trying to
	// buy with a flood of requests. Tests read it directly; nothing else does.
	derivations int
}

type verifyCall struct {
	wg sync.WaitGroup
	ok bool
}

// defaultVerifyCacheTTL is cacheTTL's default: long enough that a browser
// reconnecting SSE every few seconds never re-derives, short enough that a
// changed password is honoured well within one operator's sitting.
const defaultVerifyCacheTTL = 30 * time.Second

func newAuthVerifier(check func(user, pass string) bool) *authVerifier {
	return &authVerifier{
		check:  check,
		ttl:    defaultVerifyCacheTTL,
		good:   map[string]time.Time{},
		inHand: map[string]*verifyCall{},
	}
}

// verify reports whether user/pass is correct, deriving at most once for any
// set of identical concurrent callers and skipping derivation altogether for
// cacheTTL after a pair last verified.
func (v *authVerifier) verify(user, pass string) bool {
	key := user + "\x00" + pass
	now := time.Now()

	v.mu.Lock()
	if exp, ok := v.good[key]; ok && now.Before(exp) {
		v.mu.Unlock()
		return true
	}
	if call, ok := v.inHand[key]; ok {
		v.mu.Unlock()
		call.wg.Wait()
		return call.ok
	}
	call := &verifyCall{}
	call.wg.Add(1)
	v.inHand[key] = call
	v.derivations++
	v.mu.Unlock()

	ok := v.check(user, pass)

	v.mu.Lock()
	delete(v.inHand, key)
	if ok {
		v.good[key] = time.Now().Add(v.ttl)
	}
	v.mu.Unlock()

	call.ok = ok
	call.wg.Done()
	return ok
}

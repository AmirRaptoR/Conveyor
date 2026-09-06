package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A flood of the same wrong password must not buy one derivation per request:
// concurrent callers checking the exact same pair join the one already
// running.
func TestAuthVerifierCoalescesAFlood(t *testing.T) {
	var real int64
	v := newAuthVerifier(func(user, pass string) bool {
		atomic.AddInt64(&real, 1)
		time.Sleep(20 * time.Millisecond) // stand-in for 600,000 PBKDF2 iterations
		return user == "amir" && pass == "correct horse"
	})

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v.verify("amir", "wrong") {
				t.Error("a wrong password verified")
			}
		}()
	}
	wg.Wait()

	if real > 5 {
		t.Errorf("200 concurrent wrong-password checks caused %d derivations, want a handful", real)
	}
}

// A valid request must not queue behind a flood of a different, wrong
// password: the flood coalesces to one derivation, leaving the valid check
// free to run.
func TestAuthVerifierValidRequestIsNotStarvedByAFlood(t *testing.T) {
	v := newAuthVerifier(func(user, pass string) bool {
		time.Sleep(20 * time.Millisecond)
		return user == "amir" && pass == "correct horse"
	})

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.verify("amir", "wrong")
		}()
	}

	done := make(chan bool, 1)
	go func() { done <- v.verify("amir", "correct horse") }()

	select {
	case ok := <-done:
		if !ok {
			t.Error("the valid request was refused")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the valid request did not complete within 2s")
	}
	wg.Wait()
}

// 50 requests with the same valid credentials must not each derive: the
// first caches it, and any still racing join the call already in flight.
func TestAuthVerifierCachesAVerifiedPair(t *testing.T) {
	var real int64
	v := newAuthVerifier(func(user, pass string) bool {
		atomic.AddInt64(&real, 1)
		time.Sleep(10 * time.Millisecond)
		return true
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !v.verify("amir", "s") {
				t.Error("a valid pair was refused")
			}
		}()
	}
	wg.Wait()

	if real > 2 {
		t.Errorf("50 concurrent identical valid checks caused %d derivations, want at most 2", real)
	}

	// Sequential, after the first batch settled: must still be served from
	// cache, not re-derived.
	before := atomic.LoadInt64(&real)
	v.verify("amir", "s")
	if atomic.LoadInt64(&real) != before {
		t.Error("a cached pair re-derived")
	}
}

// A wrong password is refused regardless of how many correct ones came
// before it, and a password changed underneath the cache is honoured once the
// cache's documented lifetime (ttl) has passed.
func TestAuthVerifierHonoursAChangedPasswordWithinItsCacheLifetime(t *testing.T) {
	current := "old"
	v := newAuthVerifier(func(user, pass string) bool { return pass == current })
	v.ttl = 30 * time.Millisecond // the lifetime this test asserts

	if !v.verify("amir", "old") {
		t.Fatal("the correct password was refused")
	}
	if v.verify("amir", "wrong") {
		t.Error("a wrong password verified after correct ones came before it")
	}

	current = "new" // the operator edits auth.users and hands out a new password
	if !v.verify("amir", "old") {
		t.Error("the cache did not still honour the old password immediately after the change")
	}

	time.Sleep(40 * time.Millisecond) // past ttl
	if v.verify("amir", "old") {
		t.Error("the old password still verified past the cache's documented lifetime")
	}
	if !v.verify("amir", "new") {
		t.Error("the new password was not honoured once the cache expired")
	}
}

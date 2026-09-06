package server

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

// F07's lock-order requirement, exercised the way the two real call sites use
// it: refresh takes s.mu and then reads order.IDs(); handleOrder calls
// order.Set() and then takes s.mu. Run both concurrently, many times, under
// -race — a lock order inverted against the store's own writer lock would
// deadlock or trip the race detector.
func TestOrderAndRefreshDoNotDeadlock(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()

	var wg sync.WaitGroup
	const n = 50
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			body := `["s1:` + strconv.Itoa(i) + `"]`
			req := httptest.NewRequest("POST", "/api/order", strings.NewReader(body))
			s.handleOrder(w, req)
		}()
		go func() {
			defer wg.Done()
			s.refresh(context.Background())
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent order changes and refreshes did not complete; suspect a deadlock")
	}
}

// Regression guard, per the issue: a failed answer persistence must leave the
// item still marked and return the error, and F07's rework of Answers.Set
// must not change that.
func TestUnblockLeavesTheMarkWhenAnswerPersistenceFails(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks = map[string]Block{"s1:1": {Kind: "decision", Reason: "which way?"}}

	// answers persistence is made to fail exactly as the store package's own
	// write-failure tests do it: point it at a path beneath a plain file, so
	// no directory can ever be created there.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.answers = store.OpenAnswers(filepath.Join(blocker, "sub", "answers.json"))

	err := s.answerThenUnblock(s.ctx, s.state.Items[0], "go with plan A")
	if err == nil {
		t.Fatal("answerThenUnblock returned nil despite the answer failing to persist")
	}
	if _, held := s.blocks["s1:1"]; !held {
		t.Error("the mark was cleared despite the answer failing to persist")
	}
}

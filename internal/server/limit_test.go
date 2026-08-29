package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// The night this exists for: every item stopped because the agent was out of
// quota, and the quota came back at 3am while the board stayed red until
// someone woke up and pressed a button.
//
// A `limit` mark is the outside world and may clear itself. A `decision` mark
// is a person's answer outstanding and must survive, or the one question the
// night actually raised is thrown away and re-run to be asked again.
func TestQuotaRecoveryReleasesOnlyLimitMarks(t *testing.T) {
	cfg, r, _ := pipelineFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()

	s.state.Items = []model.Item{
		{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:2", Ref: "2", Source: "s1", Stage: "working", Blocked: true},
	}
	s.blocks = map[string]Block{
		"s1:1": {Kind: "limit", Reason: "the agent was out of quota", At: time.Now()},
		"s1:2": {Kind: "decision", Reason: "which database?", At: time.Now()},
	}

	if n := s.releaseLimited(s.ctx); n != 1 {
		t.Fatalf("released %d item(s), want 1", n)
	}
	// Nothing left to release: a second pass must not keep handing back.
	if n := s.releaseLimited(s.ctx); n != 0 {
		t.Errorf("a second pass released %d more item(s); the release repeats", n)
	}
	if _, held := s.blocks["s1:1"]; held {
		t.Error("the limit mark survived the agent coming back")
	}
	if _, held := s.blocks["s1:2"]; !held {
		t.Error("the decision mark was cleared: a person's answer was thrown away")
	}
}

// Answering is one gesture with unblocking, which means the session has to be
// read out of the block before clearing the mark forgets it. Get that order
// wrong and every answer starts a fresh conversation that has to rediscover the
// repository — the exact cost resuming exists to avoid.
func TestAnsweringCapturesTheSessionBeforeTheMarkIsCleared(t *testing.T) {
	cfg, r, _ := pipelineFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()

	s.state.Items = []model.Item{{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks = map[string]Block{
		"s1:1": {Kind: "decision", Reason: "may I change the schema?", Session: "sess-abc"},
	}

	req := httptest.NewRequest("POST", "/api/items/s1:1/unblock",
		strings.NewReader(`{"answer":"no, keep backwards compatibility"}`))
	req.SetPathValue("id", "s1:1")
	w := httptest.NewRecorder()
	s.handleUnblock(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("unblock returned %d: %s", w.Code, w.Body.String())
	}
	if _, held := s.blocks["s1:1"]; held {
		t.Error("the mark survived being answered")
	}

	got := s.answers.Take("s1:1")
	if got.Answer != "no, keep backwards compatibility" {
		t.Errorf("answer = %q, want the reply that was typed", got.Answer)
	}
	if got.Session != "sess-abc" {
		t.Errorf("session = %q, want sess-abc: the conversation was lost with the mark", got.Session)
	}
	// Said once. A second run of the stage is not a second question.
	if again := s.answers.Take("s1:1"); again.Answer != "" {
		t.Errorf("the answer was handed over twice: %q", again.Answer)
	}
}

// The whole point, end to end: what a person typed has to arrive in the stage
// script's stdin. The path crosses the store, the engine and the runner, and
// each of those hands it on by a different name.
func TestTheAnswerReachesTheStageScript(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.json")

	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"backlog","title":"the only item"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\ncat > "+seen+"\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 100ms
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
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	s := New(cfg, runner.New(filepath.Join(dir, "runs")))
	s.ctx = context.Background()
	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog", Title: "the only item"}
	s.state.Items = []model.Item{item}
	if err := s.answers.Set("s1:1", model.Resume{Answer: "no, keep backwards compatibility", Session: "sess-abc"}); err != nil {
		t.Fatal(err)
	}

	s.runOne(s.ctx, item, "working")

	b, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the stage script recorded no stdin: %v", err)
	}
	var got model.StageInput
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("stdin was not the documented shape: %v", err)
	}
	if got.Answer != "no, keep backwards compatibility" {
		t.Errorf("the script saw answer %q; the reply never arrived", got.Answer)
	}
	if got.Session != "sess-abc" {
		t.Errorf("the script saw session %q; it cannot resume", got.Session)
	}
	// Spent on the way in, so a re-run is not a second reply to one question.
	if left := s.answers.Get("s1:1"); left.Answer != "" {
		t.Errorf("the answer outlived the run it was written for: %q", left.Answer)
	}
}

// A resume that does not work must not cost the answer. The session is the
// disposable half — a conversation the agent can no longer be given — and the
// paragraph a person typed is the half nobody can retype from the board.
func TestAFailedRunKeepsTheAnswerAndDropsTheSession(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\ncat >/dev/null\nexit 1\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
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
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	s := New(cfg, runner.New(filepath.Join(dir, "runs")))
	s.ctx = context.Background()
	item := model.Item{ID: "s1:1", Ref: "1", Source: "s1", Stage: "backlog"}
	s.state.Items = []model.Item{item}
	if err := s.answers.Set("s1:1", model.Resume{Answer: "keep backwards compatibility", Session: "sess-gone"}); err != nil {
		t.Fatal(err)
	}

	s.runOne(s.ctx, item, "working")

	left := s.answers.Get("s1:1")
	if left.Answer != "keep backwards compatibility" {
		t.Errorf("the answer was lost to a failed run: %q", left.Answer)
	}
	if left.Session != "" {
		t.Errorf("session %q was kept; the next run would resume the same dead conversation", left.Session)
	}
}

// The rule the whole distinction exists for: a question is never cleared in
// bulk. Not by the button, not by a stall timer, not by a quota coming back.
// Waiting produces conditions; it does not produce answers.
func TestBulkClearingLeavesQuestionsStanding(t *testing.T) {
	cfg, r, _ := pipelineFor(t)
	s := New(cfg, r)
	s.ctx = context.Background()

	s.state.Items = []model.Item{
		{ID: "s1:1", Ref: "1", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:2", Ref: "2", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:3", Ref: "3", Source: "s1", Stage: "working", Blocked: true},
	}
	s.blocks = map[string]Block{
		"s1:1": {Kind: "limit", Reason: "out of quota"},
		"s1:2": {Kind: "worktree", Reason: "the checkout is dirty"},
		"s1:3": {Kind: "decision", Reason: "may I change the schema?", Asked: true},
	}

	req := httptest.NewRequest("POST", "/api/unblock", nil)
	w := httptest.NewRecorder()
	s.handleUnblockAll(w, req)

	var got struct {
		Unblocking   int `json:"unblocking"`
		WaitingOnYou int `json:"waitingOnYou"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Unblocking != 2 {
		t.Errorf("unblocking %d, want the two conditions", got.Unblocking)
	}
	if got.WaitingOnYou != 1 {
		t.Errorf("waitingOnYou %d, want the one question", got.WaitingOnYou)
	}

	waitFor(t, "the two conditions to be handed back", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return len(s.blocks) == 1
	})
	s.mu.RLock()
	_, question := s.blocks["s1:3"]
	s.mu.RUnlock()
	if !question {
		t.Error("the question was cleared in bulk; answering it is the only thing that should")
	}

	// And a quota coming back does not reach it either.
	if n := s.releaseLimited(s.ctx); n != 0 {
		t.Errorf("quota recovery released %d item(s); the question is not a quota problem", n)
	}
}

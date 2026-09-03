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

// doctorBoard is a one-source board whose source declares a doctor: script —
// via script: rather than agent:, since most tests care about the sweep, not
// agent resolution.
func doctorBoard(t *testing.T, doctorBody string) (*config.Config, *runner.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "doctor.sh"), doctorBody)
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
stages:
  - name: backlog
    onSuccess: working
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
      doctor:
        script: ./doctor.sh
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, runner.New(filepath.Join(dir, "runs")), dir
}

func doctorPost(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest("POST", "/api/doctor", nil)
	} else {
		req = httptest.NewRequest("POST", "/api/doctor", strings.NewReader(body))
	}
	s.handleDoctorStart(w, req)
	return w
}

func doctorGet(t *testing.T, s *Server) Sweep {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleDoctorGet(w, httptest.NewRequest("GET", "/api/doctor", nil))
	var sw Sweep
	if err := json.Unmarshal(w.Body.Bytes(), &sw); err != nil {
		t.Fatalf("GET /api/doctor: %v (%s)", err, w.Body.String())
	}
	return sw
}

func waitSweepDone(t *testing.T, s *Server) Sweep {
	t.Helper()
	var sw Sweep
	waitFor(t, "the sweep to finish", func() bool {
		sw = doctorGet(t, s)
		return !sw.Running
	})
	return sw
}

func TestDoctorSweepWithNoMarkedItemsRunsNothing(t *testing.T) {
	cfg, r, _ := doctorBoard(t, "#!/bin/sh\nexit 1\n") // must never run
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working"}}

	w := doctorPost(t, s, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	sw := waitSweepDone(t, s)
	if len(sw.Results) != 0 {
		t.Errorf("results = %v, want none — nothing is marked", sw.Results)
	}
}

func TestDoctorSweepReturns202BeforeFinishing(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	cfg, r, _ := doctorBoard(t, `#!/bin/sh
while [ ! -f `+release+` ]; do sleep 0.01; done
exit 10
`)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Kind: "error", Reason: "boom", Stage: "working", RunID: "r0"}

	w := doctorPost(t, s, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	sw := doctorGet(t, s)
	if !sw.Running || len(sw.Results) != 1 || sw.Results[0].Status != "pending" {
		t.Fatalf("sweep = %+v, want one pending row while running", sw)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sw = waitSweepDone(t, s)
	if sw.Results[0].Status != "left" {
		t.Errorf("row settled to %+v, want left (exit 10)", sw.Results[0])
	}
}

func TestDoctorSweepRefusesConcurrentStart(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	cfg, r, _ := doctorBoard(t, `#!/bin/sh
while [ ! -f `+release+` ]; do sleep 0.01; done
exit 10
`)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Kind: "error", Reason: "boom", Stage: "working"}

	if w := doctorPost(t, s, ""); w.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202", w.Code)
	}
	waitFor(t, "the sweep to start", func() bool { return doctorGet(t, s).Running })
	w := doctorPost(t, s, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("second POST status = %d, want 409", w.Code)
	}
	os.WriteFile(release, nil, 0o644)
	waitSweepDone(t, s)
}

// A missing body, {}, {"apply": null}, {"apply": "yes"}, null and unparseable
// JSON must all read as a dry run, never a 400 and never an apply.
func TestDoctorApplyDefaultsToDryRun(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"no body", ""},
		{"empty object", "{}"},
		{"null apply", `{"apply": null}`},
		{"wrong type", `{"apply": "yes"}`},
		{"null body", "null"},
		{"unparseable", "{not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, r, _ := doctorBoard(t, "#!/bin/sh\nexit 10\n")
			s := New(cfg, r)
			s.ctx = t.Context()
			s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
			s.blocks["s1:1"] = Block{Kind: "error", Reason: "boom", Stage: "working"}

			w := doctorPost(t, s, tc.body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d (%s), want 202", w.Code, w.Body.String())
			}
			sw := waitSweepDone(t, s)
			if sw.Apply {
				t.Errorf("apply = true, want the safe default")
			}
		})
	}
}

// Under a dry run the script sees CONVEYOR_DRY_RUN=1; with apply: true the
// variable is unset. Confirmed by having the doctor script report which it saw.
func TestDoctorDryRunSetsTheEnvironmentVariable(t *testing.T) {
	cfg, r, _ := doctorBoard(t, `#!/bin/sh
if [ -n "$CONVEYOR_DRY_RUN" ]; then v=1; else v=0; fi
cat > "$CONVEYOR_RESULT" <<JSON
{"summary": "dry=$v"}
JSON
exit 10
`)
	s := New(cfg, r)
	s.ctx = t.Context()

	for _, tc := range []struct {
		apply string
		want  string
	}{
		{"", "dry=1"},
		{`{"apply": true}`, "dry=0"},
	} {
		s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
		s.blocks = map[string]Block{"s1:1": {Kind: "error", Reason: "boom", Stage: "working"}}
		doctorPost(t, s, tc.apply)
		sw := waitSweepDone(t, s)
		if sw.Results[0].Why != tc.want {
			t.Errorf("apply=%q: why = %q, want %q", tc.apply, sw.Results[0].Why, tc.want)
		}
	}
}

// Three marked items, a script that records its own start and end: the
// intervals a sweep produces must never overlap.
func TestDoctorInvocationsNeverOverlap(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "intervals")
	cfg, r, _ := doctorBoard(t, `#!/bin/sh
echo "start $(date +%s.%N)" >> `+log+`
sleep 0.05
echo "end $(date +%s.%N)" >> `+log+`
exit 10
`)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:2", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:3", Source: "s1", Stage: "working", Blocked: true},
	}
	for _, id := range []string{"s1:1", "s1:2", "s1:3"} {
		s.blocks[id] = Block{Kind: "error", Reason: "boom", Stage: "working"}
	}
	doctorPost(t, s, "")
	sw := waitSweepDone(t, s)
	if len(sw.Results) != 3 {
		t.Fatalf("results = %v, want 3", sw.Results)
	}

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 6 {
		t.Fatalf("intervals log = %q, want 6 lines (start/end x3)", lines)
	}
	// start, end, start, end, start, end — never start, start.
	for i := 0; i < len(lines); i += 2 {
		if !strings.HasPrefix(lines[i], "start") || !strings.HasPrefix(lines[i+1], "end") {
			t.Fatalf("intervals overlapped: %v", lines)
		}
	}
}

// A block with asked: true is a question, not a condition — skipped before the
// script runs, and no run directory is created for it.
func TestDoctorSkipsAskedItems(t *testing.T) {
	cfg, r, dir := doctorBoard(t, "#!/bin/sh\nexit 1\n") // must never run
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Kind: "decision", Reason: "which approach?", Stage: "working", Asked: true}

	doctorPost(t, s, "")
	sw := waitSweepDone(t, s)
	if sw.Results[0].Status != "skipped" || sw.Results[0].RunID != "" {
		t.Errorf("row = %+v, want skipped with no run", sw.Results[0])
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "runs")); len(entries) != 0 {
		t.Errorf("a run directory was created for a question the doctor must not touch")
	}
}

// A source whose doctor: names an agent the server has paused is skipped; a
// doctor declared with script: is never skipped for that reason, because no
// agent's quota is spent running it.
func TestDoctorSkipsPausedAgentButNotAPlainScript(t *testing.T) {
	cfg, r, _ := doctorBoard(t, "#!/bin/sh\nexit 10\n")
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Kind: "error", Reason: "boom", Stage: "working"}

	// script: doctor.sh names no agent, so pausing an unrelated agent must not
	// touch it.
	s.pauseFor("claude", time.Time{}, "over quota", true)
	doctorPost(t, s, "")
	sw := waitSweepDone(t, s)
	if sw.Results[0].Status == "skipped" {
		t.Errorf("a script: doctor was skipped for an agent pause it cannot spend: %+v", sw.Results[0])
	}
}

// Exit 0 with no result file: the mark is cleared, no answer recorded.
func TestDoctorApplyExit0ClearsTheMark(t *testing.T) {
	cfg, r, _ := doctorBoard(t, "#!/bin/sh\nexit 0\n")
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Kind: "limit", Reason: "quota", Stage: "working", RunID: "r0"}

	doctorPost(t, s, `{"apply": true}`)
	sw := waitSweepDone(t, s)
	if sw.Results[0].Status != "cleared" {
		t.Fatalf("row = %+v, want cleared", sw.Results[0])
	}
	s.mu.RLock()
	it, hasBlock := s.state.Items[0], s.blocks["s1:1"]
	_, stillBlocked := s.blocks["s1:1"]
	s.mu.RUnlock()
	if it.Blocked {
		t.Error("item is still marked")
	}
	if stillBlocked {
		t.Errorf("blocks entry survived: %+v", hasBlock)
	}
	if s.answers.Get("s1:1") != (model.Resume{}) {
		t.Error("no answer should have been recorded")
	}
}

// Exit 0 with an answer: the answer is recorded with the block's session
// before the mark clears, and reaches the item's next run.
func TestDoctorApplyExit0WithAnswerRecordsIt(t *testing.T) {
	cfg, r, _ := doctorBoard(t, `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
{"answer": "resumed with a fresh budget"}
JSON
exit 0
`)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Kind: "turns", Reason: "ran out of turns", Stage: "working", RunID: "r0", Session: "sess-1"}

	doctorPost(t, s, `{"apply": true}`)
	sw := waitSweepDone(t, s)
	if sw.Results[0].Status != "cleared" || !sw.Results[0].Answered {
		t.Fatalf("row = %+v, want cleared and answered", sw.Results[0])
	}
	got := s.answers.Get("s1:1")
	if got.Answer != "resumed with a fresh budget" || got.Session != "sess-1" {
		t.Errorf("answer = %+v, want the text and the block's session", got)
	}
}

// A result file that is absent, unparseable, or has answer empty,
// whitespace-only or not a string, still clears on exit 0 and records nothing.
func TestDoctorNoUsableAnswerStillClears(t *testing.T) {
	for _, tc := range []struct {
		name, script string
	}{
		{"no result file", "#!/bin/sh\nexit 0\n"},
		{"malformed json", "#!/bin/sh\necho 'not json' > \"$CONVEYOR_RESULT\"\nexit 0\n"},
		{"whitespace answer", "#!/bin/sh\necho '{\"answer\": \"   \"}' > \"$CONVEYOR_RESULT\"\nexit 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, r, _ := doctorBoard(t, tc.script)
			s := New(cfg, r)
			s.ctx = t.Context()
			s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
			s.blocks["s1:1"] = Block{Kind: "limit", Reason: "quota", Stage: "working", RunID: "r0"}

			doctorPost(t, s, `{"apply": true}`)
			sw := waitSweepDone(t, s)
			if sw.Results[0].Status != "cleared" || sw.Results[0].Answered {
				t.Fatalf("row = %+v, want cleared with no answer", sw.Results[0])
			}
			if s.answers.Get("s1:1") != (model.Resume{}) {
				t.Error("an answer was recorded from an unusable result")
			}
		})
	}
}

// Exit 10, 20, a failure and a timeout each leave the item marked and record no
// answer, whatever the script wrote.
func TestDoctorNonZeroExitsLeaveTheItemMarked(t *testing.T) {
	for _, tc := range []struct {
		name, script, want string
	}{
		{"noop", "#!/bin/sh\necho '{\"answer\":\"x\"}' > \"$CONVEYOR_RESULT\"\nexit 10\n", "left"},
		{"blocked", "#!/bin/sh\necho '{\"answer\":\"x\"}' > \"$CONVEYOR_RESULT\"\nexit 20\n", "left"},
		{"failure", "#!/bin/sh\necho '{\"answer\":\"x\"}' > \"$CONVEYOR_RESULT\"\nexit 3\n", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, r, _ := doctorBoard(t, tc.script)
			s := New(cfg, r)
			s.ctx = t.Context()
			s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
			s.blocks["s1:1"] = Block{Kind: "error", Reason: "boom", Stage: "working", RunID: "r0"}

			doctorPost(t, s, `{"apply": true}`)
			sw := waitSweepDone(t, s)
			if sw.Results[0].Status != tc.want {
				t.Fatalf("row = %+v, want status %q", sw.Results[0], tc.want)
			}
			s.mu.RLock()
			it, still := s.state.Items[0], s.blocks["s1:1"]
			s.mu.RUnlock()
			if !it.Blocked || still.RunID != "r0" {
				t.Errorf("item = %+v, block = %+v, want left marked with its original block untouched", it, still)
			}
			if s.answers.Get("s1:1") != (model.Resume{}) {
				t.Error("an answer must not survive a non-clearing exit")
			}
		})
	}
}

// An item unblocked by a person (or Unblock all, or a quota release) after the
// sweep started must not be cleared again — the re-check happens against the
// live board, not the snapshot the sweep began with.
func TestDoctorSkipsAtApplyIfAlreadyUnblocked(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	cfg, r, _ := doctorBoard(t, `#!/bin/sh
while [ ! -f `+release+` ]; do sleep 0.01; done
exit 0
`)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Kind: "limit", Reason: "quota", Stage: "working", RunID: "r0"}

	doctorPost(t, s, `{"apply": true}`)
	waitFor(t, "the doctor to start", func() bool {
		return len(doctorGet(t, s).Results) == 1
	})
	// A person clears it by hand while the doctor is mid-run.
	s.mu.Lock()
	s.state.Items[0].Blocked = false
	delete(s.blocks, "s1:1")
	s.mu.Unlock()
	os.WriteFile(release, nil, 0o644)

	sw := waitSweepDone(t, s)
	if sw.Results[0].Status != "skipped" {
		t.Errorf("row = %+v, want skipped once a person already cleared it", sw.Results[0])
	}
}

// A doctor that fails on one item does not stop the sweep.
func TestDoctorContinuesAfterOneItemFails(t *testing.T) {
	cfg, r, dir := doctorBoard(t, "#!/bin/sh\nexit 0\n")
	// s1:1's doctor script is deleted after load, so it fails to start.
	broken := filepath.Join(dir, "doctor.sh")
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:2", Source: "s1", Stage: "working", Blocked: true},
	}
	s.blocks["s1:1"] = Block{Kind: "limit", Reason: "quota", Stage: "working", RunID: "r0"}
	s.blocks["s1:2"] = Block{Kind: "limit", Reason: "quota", Stage: "working", RunID: "r1"}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}

	doctorPost(t, s, `{"apply": true}`)
	sw := waitSweepDone(t, s)
	if len(sw.Results) != 2 {
		t.Fatalf("results = %v, want 2", sw.Results)
	}
	for _, row := range sw.Results {
		if row.Status != "failed" {
			t.Errorf("row %+v, want failed — the script cannot even start", row)
		}
	}
}

// Every invocation is recorded with kind "doctor", and recallBlocks — which
// reads only kind "stage" runs — must never adopt one, even when it exits 20.
func TestDoctorRunsAreNeverAdoptedByRecallBlocks(t *testing.T) {
	cfg, r, _ := doctorBoard(t, "#!/bin/sh\nexit 20\n")
	s := New(cfg, r)
	s.ctx = t.Context()

	// The stage run that actually marked the item, the way recallBlocks expects
	// to find one — an earlier day, so it sorts before the doctor's own run.
	day := filepath.Join(r.Root, "2026-08-28", "090000.000-aaaa")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "meta.json"), []byte(
		`{"id":"090000.000-aaaa","source":"s1","itemId":"s1:1","kind":"stage",
		  "to":"working","outcome":"blocked","exitCode":20}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "result.json"), []byte(
		`{"blocked":true,"reason":"needs a person","kind":"decision"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Kind: "decision", Reason: "needs a person", Stage: "working", RunID: "090000.000-aaaa"}

	doctorPost(t, s, `{"apply": true}`)
	sw := waitSweepDone(t, s)
	if sw.Results[0].Status != "left" {
		t.Fatalf("row = %+v, want left", sw.Results[0])
	}
	if sw.Results[0].RunID == "" || sw.Results[0].RunID == "090000.000-aaaa" {
		t.Fatalf("row = %+v, want its own run id, distinct from the stage run that marked the item", sw.Results[0])
	}

	var found *RunMeta
	s.walkRuns(func(m RunMeta) bool {
		if m.ID == sw.Results[0].RunID {
			cp := m
			found = &cp
			return false
		}
		return true
	})
	if found == nil || found.Kind != "doctor" {
		t.Fatalf("the doctor's own run = %+v, want kind doctor", found)
	}

	// After a restart, recallBlocks must recover the original stage run's
	// reason — it reads only kind "stage" and must skip the doctor run that
	// exited 20 sitting right beside it.
	s.blocks = map[string]Block{}
	s.recallBlocks(s.state.Items)
	if got := s.blocks["s1:1"]; got.Kind != "decision" || got.Reason != "needs a person" {
		t.Errorf("recallBlocks recovered %+v, want the original stage run's decision", got)
	}
}

// The stdin a doctor receives: item, stage, blocked: true, the block (without
// session) and this item's own trimmed run history.
func TestDoctorStdinShape(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen.json")
	cfg, r, _ := doctorBoard(t, `#!/bin/sh
cp /dev/stdin `+seen+`
exit 10
`)
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true, Title: "a title"}}
	s.blocks["s1:1"] = Block{Kind: "turns", Reason: "ran out of turns", Stage: "working",
		RunID: "r0", Asked: false, Session: "should-not-leak"}

	doctorPost(t, s, "")
	waitSweepDone(t, s)

	b, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	var in model.DoctorInput
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("stdin was not a DoctorInput: %v (%s)", err, b)
	}
	if in.Item == nil || in.Item.ID != "s1:1" || !in.Blocked || in.Stage != "working" {
		t.Errorf("stdin = %+v", in)
	}
	if in.Block.Kind != "turns" || in.Block.RunID != "r0" {
		t.Errorf("block = %+v", in.Block)
	}
	if strings.Contains(string(b), "should-not-leak") {
		t.Error("the block's session leaked onto the doctor's stdin")
	}
}

// A doctor executable that loses its execute bit after load still produces a
// failed row naming the error and leaves the item marked, exactly as a
// deleted one does.
func TestDoctorNotExecutableProducesAFailedRow(t *testing.T) {
	cfg, r, dir := doctorBoard(t, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(dir, "doctor.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, r)
	s.ctx = t.Context()
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}}
	s.blocks["s1:1"] = Block{Kind: "limit", Reason: "quota", Stage: "working", RunID: "r0"}

	doctorPost(t, s, `{"apply": true}`)
	sw := waitSweepDone(t, s)
	if sw.Results[0].Status != "failed" || sw.Results[0].Why == "" {
		t.Fatalf("row = %+v, want a failed row naming the error", sw.Results[0])
	}
	s.mu.RLock()
	it := s.state.Items[0]
	s.mu.RUnlock()
	if !it.Blocked {
		t.Error("the item was touched even though its doctor could not even start")
	}
}

// Cancelling the server's context mid-sweep leaves the rows it never reached
// pending, still marks the sweep finished, and releases the guard.
func TestDoctorCancellationLeavesRemainingRowsPending(t *testing.T) {
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	cfg, r, _ := doctorBoard(t, `#!/bin/sh
touch `+started+`
while [ ! -f `+release+` ]; do sleep 0.01; done
exit 10
`)
	s := New(cfg, r)
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true},
		{ID: "s1:2", Source: "s1", Stage: "working", Blocked: true},
	}
	s.blocks["s1:1"] = Block{Kind: "error", Reason: "boom", Stage: "working"}
	s.blocks["s1:2"] = Block{Kind: "error", Reason: "boom", Stage: "working"}

	doctorPost(t, s, "")
	waitFor(t, "the first item's doctor to start", func() bool { _, err := os.Stat(started); return err == nil })
	cancel()
	os.WriteFile(release, nil, 0o644) // let the in-flight invocation actually finish

	sw := waitSweepDone(t, s)
	if sw.Results[1].Status != "pending" {
		t.Errorf("second row = %+v, want pending — the sweep was cancelled before reaching it", sw.Results[1])
	}
	if sw.FinishedAt == nil {
		t.Error("a cancelled sweep must still be marked finished")
	}

	// The guard was released: a fresh context lets a new sweep start.
	s.ctx = t.Context()
	w := doctorPost(t, s, "")
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 — the guard must be released after cancellation", w.Code)
	}
	waitSweepDone(t, s)
}

// The scheduler is a separate goroutine from the sweep and takes no lock the
// sweep holds, so an unmarked item keeps advancing while a sweep is running.
func TestDoctorSweepDoesNotBlockTheScheduler(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	cfg, r, _ := doctorBoard(t, `#!/bin/sh
while [ ! -f `+release+` ]; do sleep 0.01; done
exit 10
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(cfg, r)
	s.ctx = ctx
	s.state.Items = []model.Item{
		{ID: "s1:1", Source: "s1", Stage: "working", Blocked: true}, // holds the sweep busy
		{ID: "s1:2", Source: "s1", Stage: "backlog"},                // free to advance
	}
	s.blocks["s1:1"] = Block{Kind: "error", Reason: "boom", Stage: "working"}
	go s.schedule(ctx)

	doctorPost(t, s, "")
	waitFor(t, "s1:2 to advance while the sweep is running", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, it := range s.state.Items {
			if it.ID == "s1:2" {
				return it.Stage == "done"
			}
		}
		return false
	})
	if !doctorGet(t, s).Running {
		t.Error("the scheduler outran the sweep instead of running beside it — this proves nothing")
	}
	os.WriteFile(release, nil, 0o644)
	waitSweepDone(t, s)
}

// The runs handed to a doctor script are built from meta.json alone — never
// log.txt, result.json or stdin.json — so a run whose other files retention
// already swept still appears rather than breaking the invocation.
func TestDoctorRunsSurviveASweptDirectory(t *testing.T) {
	cfg, r, _ := doctorBoard(t, "#!/bin/sh\nexit 10\n")
	s := New(cfg, r)
	s.ctx = t.Context()

	day := filepath.Join(r.Root, "2026-08-20", "090000.000-swpt")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "meta.json"), []byte(
		`{"id":"090000.000-swpt","source":"s1","itemId":"s1:1","kind":"stage",
		  "to":"working","outcome":"blocked","exitCode":20,
		  "startedAt":"2026-08-20T09:00:00Z","finishedAt":"2026-08-20T09:00:01Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// log.txt, result.json and stdin.json are deliberately never written —
	// this is what a run looks like after retention has swept everything but
	// the directory listing itself needs.

	runs := s.doctorRuns("s1:1")
	if len(runs) != 1 || runs[0].ID != "090000.000-swpt" || runs[0].Dir == "" {
		t.Fatalf("runs = %+v, want the swept run still present with a readable dir", runs)
	}
}

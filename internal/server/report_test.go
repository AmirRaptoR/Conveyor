package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// reportStages is the four-stage pipeline docs/CONTRACTS.md's example report
// walks through: a queue (ready) sitting between two stages that run scripts.
func reportStages() []config.Stage {
	return []config.Stage{
		{Name: "refining", Script: "refine", OnSuccess: "ready"},
		{Name: "ready", OnSuccess: "in-progress"}, // a queue: no script
		{Name: "in-progress", Script: "implement", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}
}

func mv(from, to string, outcome model.Outcome, at time.Time) runFact {
	return runFact{Run: model.Run{Kind: "move", From: from, To: to, Outcome: outcome, StartedAt: at, FinishedAt: at}}
}

func stageRun(id, to string, outcome model.Outcome, start, finish time.Time) runFact {
	return runFact{Run: model.Run{ID: id, Kind: "stage", To: to, From: to, Outcome: outcome,
		StartedAt: start, FinishedAt: finish, ExitCode: exitFor(outcome)}}
}

func exitFor(o model.Outcome) int {
	if o == model.OutcomeSuccess {
		return 0
	}
	return 1
}

func TestReport_HappyPath(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	item := model.Item{ID: "s1:1", Title: "Ship the thing", URL: "https://example.com/issues/1",
		Stage: "done", CreatedAt: "2026-08-24T09:12:00Z"}
	runs := []runFact{
		mv("", "refining", model.OutcomeSuccess, t0),
		stageRun("r1", "refining", model.OutcomeSuccess, t0.Add(time.Minute), t0.Add(11*time.Minute)),
		mv("refining", "ready", model.OutcomeSuccess, t0.Add(12*time.Minute)),
		mv("ready", "in-progress", model.OutcomeSuccess, t0.Add(12*time.Minute+25*time.Hour)),
		{Run: model.Run{ID: "r-fail", Kind: "stage", To: "in-progress", From: "in-progress",
			Outcome: model.OutcomeFailure, ExitCode: 1,
			StartedAt: t0.Add(13 * time.Hour), FinishedAt: t0.Add(13*time.Hour + 20*time.Minute)}},
		stageRun("r-ok", "in-progress", model.OutcomeSuccess,
			t0.Add(13*time.Hour+21*time.Minute), t0.Add(14*time.Hour+42*time.Minute)),
		mv("in-progress", "done", model.OutcomeSuccess, t0.Add(12*time.Minute+25*time.Hour+1*time.Hour+42*time.Minute)),
	}
	// The review stage isn't in this config, so its note is attached to
	// in-progress instead — exercised with a summary on the last run.
	runs[len(runs)-2].Result = json.RawMessage(`{"summary":"implemented on the second attempt"}`)

	now := t0.Add(48 * time.Hour)
	md := formatReport(item, runs, reportStages(), now)

	for _, want := range []string{
		"# Final report — Ship the thing",
		"`s1:1` · [https://example.com/issues/1](https://example.com/issues/1)",
		"| refining | 2026-08-25 08:00Z | 2026-08-25 08:12Z | 12m | 1 | success |",
		"| ready | 2026-08-25 08:12Z |",
		"| 1 | queued |",
		"| in-progress |",
		"| 1 | failure, success |",
		"| done | ",
		" | — | — | 1 | queued |",
		"4 stages, 3 stage runs, 1 stop",
		"**error** in `in-progress`",
		"— run `r-fail` — retried",
		"## Notes",
		"### in-progress",
		"> implemented on the second attempt",
		"> — run `r-ok`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report missing %q\n--- report ---\n%s", want, md)
		}
	}
	if strings.Contains(md, "No stops.") {
		t.Error("report says no stops, but one failure should be listed")
	}
}

// A stage entered twice is one row, not two, and its time is the sum of both
// stays.
func TestReport_StageVisitedTwiceIsOneRowSummed(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	item := model.Item{ID: "s1:1", Title: "t", Stage: "done"}
	runs := []runFact{
		mv("", "in-progress", model.OutcomeSuccess, t0),
		mv("in-progress", "ready", model.OutcomeSuccess, t0.Add(10*time.Minute)), // 10m stay
		mv("ready", "in-progress", model.OutcomeSuccess, t0.Add(20*time.Minute)),
		mv("in-progress", "done", model.OutcomeSuccess, t0.Add(35*time.Minute)), // 15m stay
	}
	tl := buildTimeline(item, runs, reportStages(), t0.Add(time.Hour))
	var row *stageRow
	for i := range tl.rows {
		if tl.rows[i].name == "in-progress" {
			row = &tl.rows[i]
		}
	}
	if row == nil {
		t.Fatal("no row for in-progress")
	}
	if row.visits != 2 {
		t.Errorf("visits = %d, want 2", row.visits)
	}
	if row.dur != 25*time.Minute {
		t.Errorf("dur = %s, want 25m (10m + 15m)", row.dur)
	}
}

// A move that did not exit 0 never happened: no row, no visit.
func TestReport_FailedMoveProducesNoRow(t *testing.T) {
	item := model.Item{ID: "s1:1", Title: "t", Stage: "refining"}
	runs := []runFact{
		{Run: model.Run{Kind: "move", From: "refining", To: "ready", Outcome: model.OutcomeFailure,
			StartedAt: time.Now(), FinishedAt: time.Now()}},
	}
	tl := buildTimeline(item, runs, reportStages(), time.Now())
	if len(tl.rows) != 0 {
		t.Errorf("rows = %v, want none — the move never succeeded", tl.rows)
	}
}

// A move with from == to is the engine confirming where an item already is,
// not a transition — it produces no visit, even when it happens twice.
func TestReport_SameStageMoveIsNotAVisit(t *testing.T) {
	t0 := time.Now()
	item := model.Item{ID: "s1:1", Title: "t", Stage: "refining"}
	runs := []runFact{
		mv("", "refining", model.OutcomeSuccess, t0),
		mv("refining", "refining", model.OutcomeSuccess, t0.Add(time.Minute)),
		mv("refining", "refining", model.OutcomeSuccess, t0.Add(2*time.Minute)),
	}
	tl := buildTimeline(item, runs, reportStages(), t0.Add(time.Hour))
	if len(tl.rows) != 1 || tl.rows[0].visits != 1 {
		t.Fatalf("rows = %+v, want exactly one row with 1 visit", tl.rows)
	}
}

// A failure not followed by another stage run in the same stage is marked,
// not retried.
func TestReport_UnretriedFailureEndsMarked(t *testing.T) {
	t0 := time.Now()
	item := model.Item{ID: "s1:1", Title: "t", Stage: "in-progress"}
	runs := []runFact{
		mv("", "in-progress", model.OutcomeSuccess, t0),
		{Run: model.Run{ID: "only-run", Kind: "stage", To: "in-progress", From: "in-progress",
			Outcome: model.OutcomeFailure, ExitCode: 1, StartedAt: t0.Add(time.Minute), FinishedAt: t0.Add(2 * time.Minute)}},
	}
	md := formatReport(item, runs, reportStages(), t0.Add(time.Hour))
	if !strings.Contains(md, "run `only-run` — marked") {
		t.Errorf("report = %s, want the lone failure to end \"marked\"", md)
	}
	if strings.Contains(md, "retried") {
		t.Error("nothing followed the failure; it must not read as retried")
	}
}

// A stage where Stage.Runs() is false and no stage run was recorded shows
// "queued" — using the config's own notion of a queue, not a guess.
func TestReport_QueueStageShowsQueued(t *testing.T) {
	t0 := time.Now()
	item := model.Item{ID: "s1:1", Title: "t", Stage: "ready"}
	runs := []runFact{
		mv("", "refining", model.OutcomeSuccess, t0),
		mv("refining", "ready", model.OutcomeSuccess, t0.Add(time.Minute)),
	}
	md := formatReport(item, runs, reportStages(), t0.Add(time.Hour))
	if !strings.Contains(md, "| ready |") || !strings.Contains(md, "queued") {
		t.Errorf("report = %s, want a queued row for ready", md)
	}
}

// An absent createdAt omits Opened and Lead time and changes nothing else.
func TestReport_MissingCreatedAtOmitsOpenedAndLeadTime(t *testing.T) {
	t0 := time.Now()
	item := model.Item{ID: "s1:1", Title: "t", Stage: "done"} // no CreatedAt
	runs := []runFact{
		mv("", "done", model.OutcomeSuccess, t0),
	}
	md := formatReport(item, runs, reportStages(), t0.Add(time.Hour))
	if strings.Contains(md, "Opened") || strings.Contains(md, "Lead time") {
		t.Errorf("report = %s, want no Opened/Lead time rows without a createdAt", md)
	}
	if !strings.Contains(md, "Finished") {
		t.Error("Finished should still appear; only Opened/Lead time depend on createdAt")
	}
}

// A title containing a pipe must not break the stage table it sits above.
func TestReport_TitleWithPipeDoesNotBreakTable(t *testing.T) {
	t0 := time.Now()
	item := model.Item{ID: "s1:1", Title: "fix a | b", Stage: "done"}
	runs := []runFact{mv("", "done", model.OutcomeSuccess, t0)}
	md := formatReport(item, runs, reportStages(), t0.Add(time.Hour))
	if !strings.Contains(md, "# Final report — fix a | b") {
		t.Errorf("the heading is not a table row; the raw title should appear there verbatim: %s", md)
	}
	// The stage table itself must still have exactly six columns per row.
	for _, ln := range strings.Split(md, "\n") {
		if strings.HasPrefix(ln, "| done ") {
			if got := strings.Count(ln, "|"); got != 7 { // 6 columns => 7 pipes
				t.Errorf("stage row = %q, want 7 pipes (6 columns)", ln)
			}
		}
	}
}

// An item with no runs at all still answers with something readable, not an
// empty body.
func TestReport_NoRuns(t *testing.T) {
	item := model.Item{ID: "s1:1", Title: "t", Stage: "backlog"}
	md := formatReport(item, nil, reportStages(), time.Now())
	if !strings.Contains(md, "# Final report — t") {
		t.Errorf("report = %q, want the heading even with no runs", md)
	}
	if !strings.Contains(md, "No runs") {
		t.Errorf("report = %q, want a line saying no runs were found", md)
	}
	if strings.Contains(md, "| Stage |") {
		t.Errorf("report = %q, want no stage table with nothing to fill it", md)
	}
}

// An unfinished item opens with "In <stage> for <time>" and shows its
// current stage's time so far, suffixed, with Left left blank.
func TestReport_UnfinishedItemShowsTimeSoFar(t *testing.T) {
	t0 := time.Now().Add(-90 * time.Minute)
	item := model.Item{ID: "s1:1", Title: "t", Stage: "in-progress"}
	runs := []runFact{
		mv("", "refining", model.OutcomeSuccess, t0),
		mv("refining", "in-progress", model.OutcomeSuccess, t0.Add(10*time.Minute)),
	}
	now := t0.Add(90 * time.Minute)
	md := formatReport(item, runs, reportStages(), now)
	if !strings.Contains(md, "In in-progress for") {
		t.Errorf("report = %s, want the unfinished-item sentence", md)
	}
	if !strings.Contains(md, "(so far)") {
		t.Errorf("report = %s, want the current stage's time marked (so far)", md)
	}
	if strings.Contains(md, "Finished") || strings.Contains(md, "Lead time") {
		t.Error("an unfinished item must not print Finished or Lead time")
	}
}

// When the item sits in a terminal stage but no successful entry into it
// survives, the report says so instead of printing a time it cannot back up.
func TestReport_IncompleteHistorySaysSo(t *testing.T) {
	t0 := time.Now()
	item := model.Item{ID: "s1:1", Title: "t", Stage: "done"}
	runs := []runFact{
		// Only a stage run survives retention; the move into "done" is gone.
		stageRun("r1", "in-progress", model.OutcomeSuccess, t0, t0.Add(time.Minute)),
	}
	md := formatReport(item, runs, reportStages(), t0.Add(time.Hour))
	if !strings.Contains(md, "incomplete") {
		t.Errorf("report = %s, want it to say the history is incomplete", md)
	}
	if strings.Contains(md, "Finished") || strings.Contains(md, "Lead time") || strings.Contains(md, "Working time") {
		t.Errorf("report = %s, want no Finished/Lead time/Working time computed from a partial record", md)
	}
}

// A stage run still recording "running" is not a stop.
func TestReport_RunningIsNotAStop(t *testing.T) {
	t0 := time.Now()
	item := model.Item{ID: "s1:1", Title: "t", Stage: "in-progress"}
	runs := []runFact{
		mv("", "in-progress", model.OutcomeSuccess, t0),
		{Run: model.Run{ID: "live", Kind: "stage", To: "in-progress", From: "in-progress",
			Outcome: model.OutcomeRunning, StartedAt: t0.Add(time.Minute)}},
	}
	md := formatReport(item, runs, reportStages(), t0.Add(time.Hour))
	if !strings.Contains(md, "No stops.") {
		t.Errorf("report = %s, want No stops. — a running run is not a stop", md)
	}
}

// A missing, non-string or unparseable summary adds no note and is not an
// error; with none anywhere the Notes section is absent entirely.
func TestReport_NoSummariesMeansNoNotesSection(t *testing.T) {
	t0 := time.Now()
	item := model.Item{ID: "s1:1", Title: "t", Stage: "in-progress"}
	runs := []runFact{
		mv("", "in-progress", model.OutcomeSuccess, t0),
		{Run: model.Run{ID: "a", Kind: "stage", To: "in-progress", From: "in-progress",
			Outcome: model.OutcomeSuccess, StartedAt: t0, FinishedAt: t0.Add(time.Minute)},
			Result: json.RawMessage(`{"summary": 5}`)}, // non-string
		{Run: model.Run{ID: "b", Kind: "stage", To: "in-progress", From: "in-progress",
			Outcome: model.OutcomeSuccess, StartedAt: t0, FinishedAt: t0.Add(time.Minute)},
			Result: json.RawMessage(`not json`)}, // unparseable
	}
	md := formatReport(item, runs, reportStages(), t0.Add(time.Hour))
	if strings.Contains(md, "## Notes") {
		t.Errorf("report = %s, want no Notes section", md)
	}
}

// --- endpoint ------------------------------------------------------------

func TestHandleReport_OKAndNotFound(t *testing.T) {
	cfg, r := boardFor(t)
	s := New(cfg, r)
	s.state.Items = []model.Item{{ID: "s1:1", Source: "s1", Stage: "done", Title: "shipped"}}

	day := filepath.Join(r.Root, "2026-08-28", "120000.000-aaaa")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"120000.000-aaaa","source":"s1","itemId":"s1:1","kind":"move",
	          "from":"working","to":"done","outcome":"success",
	          "startedAt":"2026-08-28T12:00:00Z","finishedAt":"2026-08-28T12:00:00Z"}`
	if err := os.WriteFile(filepath.Join(day, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/items/s1:1/report", nil)
	req.SetPathValue("id", "s1:1")
	s.handleReport(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "# Final report — shipped") {
		t.Errorf("body = %s", w.Body.String())
	}

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/items/nope/report", nil)
	req2.SetPathValue("id", "nope")
	s.handleReport(w2, req2)
	if w2.Code != 404 {
		t.Fatalf("status = %d, want 404 for an item not on the board", w2.Code)
	}
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// SweepRow is one marked item's outcome in a sweep, settled in place as the
// sweep examines it — `pending` for every row until it is, so the board can
// watch a sweep of thirty items move rather than waiting for a count at the
// end.
type SweepRow struct {
	Item string `json:"item"`
	// Status is pending | skipped | cleared | left | failed.
	Status string `json:"status"`
	// Why is the skip reason, the script's own one-line summary, or an error —
	// never the log. Truncated to a bounded length before it reaches the board.
	Why      string `json:"why,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	// RunID is the doctor's own run, absent when the row was skipped before one
	// was started.
	RunID    string `json:"runId,omitempty"`
	Answered bool   `json:"answered,omitempty"`
}

// whyLimit bounds how much of a script's summary, or an error, reaches the
// board — the point of one line per row is a sweep that reads across, not one
// that turns into a second log.
const whyLimit = 240

func truncateWhy(s string) string {
	if len(s) <= whyLimit {
		return s
	}
	return s[:whyLimit] + "…"
}

// Sweep is one run of the doctor across every item marked on the board when it
// started. Unmarked items are not in it, and items marked afterwards are not
// either — a sweep diagnoses what was stalled the moment it began.
type Sweep struct {
	ID         string     `json:"id"`
	Apply      bool       `json:"apply"`
	Running    bool       `json:"running"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Results    []SweepRow `json:"results"`
}

// handleDoctorStart begins a sweep in the background and returns at once — a
// sweep is one agent run per marked item and can take hours, so like refresh
// and unblockAll it is started and then watched, never awaited.
//
// A missing, empty, malformed or non-object body, and an apply that is absent,
// null or not a boolean, all mean apply: false — the safe reading. Dry run is
// the default invocation, given the blast radius: labels and comments across
// every onboarded repository at once.
func (s *Server) handleDoctorStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Apply *bool `json:"apply"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body)
	}
	apply := body.Apply != nil && *body.Apply

	s.mu.RLock()
	var marked []string
	for _, it := range pipeline.Order(s.cfg, s.state.Items, s.state.Order) {
		if it.Blocked {
			marked = append(marked, it.ID)
		}
	}
	s.mu.RUnlock()

	sw := &Sweep{ID: time.Now().UTC().Format(time.RFC3339Nano), Apply: apply, Running: true, StartedAt: time.Now()}
	sw.Results = make([]SweepRow, len(marked))
	for i, id := range marked {
		sw.Results[i] = SweepRow{Item: id, Status: "pending"}
	}

	s.doctorMu.Lock()
	if s.doctorSweep != nil && s.doctorSweep.Running {
		s.doctorMu.Unlock()
		http.Error(w, "a sweep is already running", http.StatusConflict)
		return
	}
	s.doctorSweep = sw
	s.doctorMu.Unlock()

	go s.runDoctorSweep(s.ctx, sw)

	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"sweep": sw.ID})
}

// handleDoctorGet returns the current or most recently finished sweep. Nil
// until the first POST /api/doctor of this process's life.
//
// A copy, taken under the lock — the sweep it points at is still being
// mutated row by row while it runs, and a caller reading the pointer's own
// fields (Running, Results) after the lock is released would be reading them
// out from under runDoctorSweep's next write.
func (s *Server) handleDoctorGet(w http.ResponseWriter, r *http.Request) {
	s.doctorMu.Lock()
	sw := s.doctorSweep
	var cp Sweep
	if sw != nil {
		cp = *sw
		cp.Results = append([]SweepRow(nil), sw.Results...)
	}
	s.doctorMu.Unlock()
	if sw == nil {
		writeJSON(w, map[string]any{"results": []SweepRow{}})
		return
	}
	writeJSON(w, cp)
}

// runDoctorSweep examines every row, one at a time — doctor invocations are
// serialised against each other for the same reason unblockAll's writes are: a
// burst of concurrent `gh` calls against one repository is its own outage. The
// scheduler is untouched throughout: a sweep takes no source or stage lock,
// because a marked item is never in flight and the doctor only ever reads.
//
// Cancellation leaves every unexamined row `pending` and still marks the sweep
// finished, so the guard is released and a later POST starts cleanly.
func (s *Server) runDoctorSweep(ctx context.Context, sw *Sweep) {
	defer func() {
		s.doctorMu.Lock()
		now := time.Now()
		sw.FinishedAt = &now
		sw.Running = false
		s.doctorMu.Unlock()
		s.hub.publish(event{Kind: "state"})
	}()

	for i := range sw.Results {
		select {
		case <-ctx.Done():
			return
		default:
		}
		row := s.doctorOne(ctx, sw.Apply, sw.Results[i].Item)
		s.doctorMu.Lock()
		sw.Results[i] = row
		s.doctorMu.Unlock()
		s.hub.publish(event{Kind: "state"})
	}
}

// doctorOne diagnoses, and — if apply — acts on, one marked item.
func (s *Server) doctorOne(ctx context.Context, apply bool, itemID string) SweepRow {
	row := SweepRow{Item: itemID, Status: "skipped"}

	s.mu.RLock()
	var item model.Item
	found := false
	for _, it := range s.state.Items {
		if it.ID == itemID {
			item, found = it, true
			break
		}
	}
	block := s.blocks[itemID]
	s.mu.RUnlock()

	switch {
	case !found || !item.Blocked:
		row.Why = "no longer marked"
		return row
	case block.Asked:
		row.Why = "waiting on a person to answer, not a retry"
		return row
	}

	src, ok := s.sourceNamed(item.Source)
	if !ok {
		row.Why = fmt.Sprintf("no source named %q", item.Source)
		return row
	}
	doctorPath, hasDoctor := src.Paths["doctor"]
	if !hasDoctor {
		row.Why = "source declares no doctor:"
		return row
	}
	spec := src.Scripts["doctor"]
	if s.agentPaused(spec.Agent) {
		row.Why = fmt.Sprintf("%s is paused for being out of quota", spec.Agent)
		return row
	}

	env := mergeEnvLocal(src.Env, spec.Params)
	if !apply {
		env = mergeEnvLocal(env, map[string]string{"CONVEYOR_DRY_RUN": "1"})
	}
	stdin := model.DoctorInput{
		Item: &item, Stage: item.Stage, Blocked: true,
		Block: model.DoctorBlock{Kind: block.Kind, Reason: block.Reason, Stage: block.Stage,
			RunID: block.RunID, At: block.At, Asked: block.Asked},
		Runs: s.doctorRuns(itemID),
	}

	res, err := s.run.Run(ctx, runner.Spec{
		Script: doctorPath, Kind: "doctor", Workdir: s.cfg.Workdir(src), Env: env,
		Source: item.Source, Item: &item, Timeout: doctorTimeoutFor(s.cfg, src), Stdin: stdin,
	})
	if err != nil {
		row.Status, row.Why = "failed", truncateWhy(err.Error())
		return row
	}
	row.RunID = res.Run.ID
	row.ExitCode = res.Run.ExitCode
	row.Outcome = string(res.Run.Outcome)

	var said struct {
		Answer  string `json:"answer"`
		Summary string `json:"summary"`
	}
	if len(res.Data) > 0 {
		_ = json.Unmarshal(res.Data, &said)
	}
	row.Why = truncateWhy(said.Summary)

	switch res.Run.Outcome {
	case model.OutcomeSuccess:
		row.Status = "cleared"
		answer := strings.TrimSpace(said.Answer)
		row.Answered = answer != ""
		if !apply {
			return row
		}
		if !s.stillMarkedAsDiagnosed(itemID, block.RunID) {
			row.Status, row.Why = "skipped", "unblocked or re-marked before the sweep could apply it"
			return row
		}
		if err := s.answerThenUnblock(ctx, item, answer); err != nil {
			row.Status, row.Why = "failed", truncateWhy(err.Error())
		}
		return row
	case model.OutcomeNoop:
		row.Status = "left"
		return row
	case model.OutcomeBlocked:
		row.Status = "left"
		return row
	default: // failure, timeout
		row.Status = "failed"
		return row
	}
}

// stillMarkedAsDiagnosed re-checks, under the lock and against the current
// board rather than the snapshot the sweep started from, that the item this
// verdict was diagnosed for is still exactly the item standing there. A sweep
// runs for hours beside Unblock all, a per-card unblock, a quota release and
// the poller; clearing a mark a person already cleared, or that a stage has
// since re-marked for a different reason, is the one thing a doctor must not
// do.
func (s *Server) stillMarkedAsDiagnosed(itemID, diagnosedRunID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.state.Items {
		if it.ID == itemID {
			return it.Blocked && s.blocks[itemID].RunID == diagnosedRunID
		}
	}
	return false
}

// doctorRuns is this item's own run history, newest first and capped at 20,
// trimmed to what a doctor script may see — see model.DoctorRun.
func (s *Server) doctorRuns(itemID string) []model.DoctorRun {
	metas, _ := s.listRuns(itemID, 20)
	out := make([]model.DoctorRun, len(metas))
	for i, m := range metas {
		out[i] = model.DoctorRun{
			ID: m.ID, Kind: m.Kind, Stage: m.To, Outcome: m.Outcome,
			ExitCode: m.ExitCode, TimedOut: m.TimedOut,
			StartedAt: m.StartedAt, FinishedAt: m.FinishedAt, Dir: m.Dir,
		}
	}
	return out
}

func (s *Server) sourceNamed(name string) (config.Source, bool) {
	for _, src := range s.cfg.Sources {
		if src.Name == name {
			return src, true
		}
	}
	return config.Source{}, false
}

// mergeEnvLocal layers b over a into a new map, leaving both untouched.
func mergeEnvLocal(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// doctorTimeoutFor is scripts.doctor.timeout:, else the config's top-level
// timeout: — doctor has no stage of its own to inherit one from.
func doctorTimeoutFor(cfg *config.Config, src config.Source) time.Duration {
	if t := src.Scripts["doctor"].Timeout.D(); t > 0 {
		return t
	}
	return cfg.Timeout.D()
}

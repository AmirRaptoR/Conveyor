package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/source"
)

// handleState hands out the board. Items go out in the order the scheduler
// will work them, so the page can draw them in the order they arrive instead of
// keeping a second copy of the ordering rules that is free to disagree.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	st := s.state
	deps := pipeline.NewDeps(s.cfg, s.state.Items)
	st.Items = pipeline.Order(s.cfg, s.state.Items, s.state.Order, deps)
	st.Held = heldOf(s.cfg, s.state.Items, deps)
	if len(deps.Cycles) > 0 {
		st.Warnings = append(append([]string(nil), s.state.Warnings...), deps.Cycles...)
	}
	bySrc, byStage, held, max, perSrc, perStage := s.eng.Locks().Snapshot()
	st.Slots = SlotsView{BySource: bySrc, ByStage: byStage, Global: held, GlobalMax: max,
		PerSource: perSrc, PerStage: perStage, Running: int(s.inFlight.Load()),
		Resources: s.eng.Locks().Resources()}
	st.Blocks = make(map[string]Block, len(s.blocks))
	for id, b := range s.blocks {
		st.Blocks[id] = b
	}
	if len(s.waiting) > 0 {
		st.Waiting = make(map[string]model.Waiting, len(s.waiting))
		for id, wt := range s.waiting {
			st.Waiting[id] = wt
		}
	}
	st.Times = make(map[string]ItemTime, len(s.times))
	for id, t := range s.times {
		st.Times[id] = t
	}
	st.TransitionErrors = make(map[string]TransitionError, len(s.transitionErrs))
	for id, e := range s.transitionErrs {
		st.TransitionErrors[id] = e
	}
	st.Answers = make(map[string]AnswerView, len(s.answerInfo))
	for id, a := range s.answerInfo {
		st.Answers[id] = a
	}
	if len(s.cancels) > 0 {
		st.Cancels = make(map[string]CancelView, len(s.cancels))
		for id, c := range s.cancels {
			st.Cancels[id] = c
		}
	}
	ids := make([]string, len(st.Items))
	for i, it := range st.Items {
		ids[i] = it.ID
	}
	s.mu.RUnlock()
	st.ManualPauses = s.manualPauseList()
	if budgets := s.budgetViews(ids); len(budgets) > 0 {
		st.Budgets = budgets
	}
	st.BudgetDayUsage = s.budgets.DayUsage(budgetDay(time.Now()))
	st.BudgetMaxRunsPerItem = s.cfg.Budgets.MaxRunsPerItem
	st.BudgetMaxRunsPerDay = s.cfg.Budgets.MaxRunsPerDay
	writeJSON(w, st)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.spawn(func() { s.refresh(s.ctx) })
	w.WriteHeader(http.StatusAccepted)
}

// handleTick advances one item. Non-blocking: a second press while one is
// running is dropped rather than queued, because two agents in one worktree is
// exactly what perSource exists to prevent.
func (s *Server) handleTick(w http.ResponseWriter, r *http.Request) {
	// Shutting down: button's own goroutine has already returned or is about
	// to, so a tick queued here would never be read. Say so rather than
	// accepting a gesture that does nothing.
	if s.ctx.Err() != nil {
		http.Error(w, "conveyor is shutting down", http.StatusServiceUnavailable)
		return
	}
	select {
	case s.tick <- struct{}{}:
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "a tick is already in flight", http.StatusConflict)
	}
}

// handleOrder replaces the manual input order. The whole list is sent, not a
// move: two browsers reordering at once should end with one of the two
// arrangements, not a merge of both.
// orderBodyLimit bounds PUT /api/order — a JSON array of item ids, which for
// any board this pipeline actually runs is a few kilobytes at most.
const orderBodyLimit = 1 << 20

func (s *Server) handleOrder(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, orderBodyLimit)
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		if bodyTooLarge(err) {
			http.Error(w, "order body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "expected a JSON array of item ids", http.StatusBadRequest)
		return
	}
	if err := s.order.Set(ids); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.state.Order = s.order.IDs()
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
	w.WriteHeader(http.StatusNoContent)
}

// handleStart runs one item's next transition now, rather than when the poller
// next comes round. It is what a drag out of the backlog does.
//
// It can only start the transition the scheduler would have started anyway. A
// drop is a decision about *when*, never about which stage: a card dropped
// straight onto a deploy stage would be a deploy nobody reviewed, and the
// pipeline being human-authored is the point.
//
// A busy slot is a refusal, not a queue, and the refusal says what is holding
// it. Dropping something and seeing nothing happen reads as a broken board;
// "midgame is busy with midgame:49" reads as a reason to wait. The persisted
// input order is still what decides who goes next when the slot frees.
// startBodyLimit bounds POST /api/items/{id}/start — a JSON object with one
// short string field.
const startBodyLimit = 4 << 10

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Stage string `json:"stage"`
	}
	if r.ContentLength > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, startBodyLimit)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			if bodyTooLarge(err) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, `expected {"stage": "..."}`, http.StatusBadRequest)
			return
		}
	}

	var item model.Item
	found := false
	s.mu.RLock()
	for _, it := range s.state.Items {
		if it.ID == id {
			item, found = it, true
			break
		}
	}
	// Built while the listing is still held: the full board, not this one
	// item, is what the gate needs to answer whether it is held.
	deps := pipeline.NewDeps(s.cfg, s.state.Items)
	s.mu.RUnlock()
	if !found {
		http.Error(w, "no item "+id+" on the board", http.StatusNotFound)
		return
	}

	target, ok := pipeline.Target(s.cfg, &item, deps)
	if !ok {
		http.Error(w, s.whyStuck(item, deps), http.StatusConflict)
		return
	}
	if body.Stage != "" && body.Stage != target {
		http.Error(w, fmt.Sprintf("%s goes to %s next, not %s — a drop starts the next stage, it cannot skip one",
			id, target, body.Stage), http.StatusConflict)
		return
	}

	// Shutting down: refuse rather than claim an item whose run would outlive
	// the engine that started it.
	if s.ctx.Err() != nil {
		http.Error(w, "conveyor is shutting down", http.StatusServiceUnavailable)
		return
	}
	// A manual start goes through the same atomic claim as every other
	// dispatch path (claim), and overrides exactly one guard:
	//   - overrides: resting — a deferral means "wait for the next listing",
	//     and dragging a card out of the backlog is a person asking for that
	//     listing right now, in spirit if not in fact.
	//   - does not override: the item claim (claimItemBusy below), the
	//     (source, stage) slot (claimSlotBusy), a paused agent
	//     (claimAgentPaused — unlike the tick button, a manual start is not
	//     the last resort under -watch and must meet the same wall every
	//     other launch does), or pipeline.Target refusing above (a marked
	//     item, or no next stage).
	switch s.claimAndLaunch(s.ctx, item, target, false) {
	case claimItemBusy:
		http.Error(w, fmt.Sprintf("%s is already running in %s", id, target), http.StatusConflict)
		return
	case claimSlotBusy:
		http.Error(w, s.whyBusy(item.Source, target), http.StatusConflict)
		return
	case claimAgentPaused:
		http.Error(w, s.whyPaused(s.cfg.AgentFor(item.Source, target)), http.StatusConflict)
		return
	case claimManuallyPaused:
		http.Error(w, s.whyManuallyPaused(item.Source), http.StatusConflict)
		return
	case claimBudgetExhausted:
		http.Error(w, s.whyBudgetExhausted(item.ID), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleUnblock clears one item's mark. It is the other half of the board: the
// engine can mark an item, and only a person can take it back.
//
// The mark comes off where the item stands, never by moving it — that is the
// whole point of a mark over a column. The scheduler is woken because the item
// becomes pickable the instant the label is gone.
func (s *Server) handleUnblock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Answering is unblocking, deliberately one gesture and one endpoint. A
	// separate "answer" verb would let an answer be recorded against an item
	// nobody handed back, which is a note in a drawer: the pipeline would never
	// run the stage that reads it.
	var said struct {
		Answer string `json:"answer"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&said)
	}

	s.mu.RLock()
	var item model.Item
	found := false
	for _, it := range s.state.Items {
		if it.ID == id {
			item, found = it, true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		http.Error(w, "no item "+id+" on the board", http.StatusNotFound)
		return
	}
	if !item.Blocked {
		w.WriteHeader(http.StatusNoContent) // already where the caller wants it
		return
	}
	if err := s.answerThenUnblock(s.ctx, item, said.Answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAction arms one of a stage's declared actions for the next run of that
// item's stage. It is the whole of the manual override.
//
// Arming, not running: the action is a word handed to the script, and the
// script decides what it means. Nothing here moves an item, chooses a stage or
// skips a check — a person pressing "merge now" is saying "stop waiting", not
// "merge whatever the state of it is", and the difference is the reason this
// is one string rather than a way to drive the pipeline by hand.
//
// It clears the item's deferral too, because a stage that exited 10 to wait is
// exactly the one this exists to hurry, and leaving it resting would mean the
// button did nothing until the next listing.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var said struct {
		Action string `json:"action"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&said)
	}

	s.mu.RLock()
	var item model.Item
	found := false
	for _, it := range s.state.Items {
		if it.ID == id {
			item, found = it, true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		http.Error(w, "no item "+id+" on the board", http.StatusNotFound)
		return
	}
	// Only what the stage the item is actually in declares. The board offers
	// nothing else, so anything else is a stale page or a hand-written
	// request, and either way the script would not know the word.
	if !s.cfg.HasAction(item.Stage, said.Action) {
		http.Error(w, fmt.Sprintf("%s declares no action named %q", item.Stage, said.Action),
			http.StatusBadRequest)
		return
	}
	// Layered onto whatever is already armed rather than replacing it: an
	// answer someone typed and an action they then pressed are two things a
	// person said about the same stop, and the next run should get both.
	armed := s.answers.Get(item.ID)
	armed.Manual = said.Action
	if err := s.answers.Set(item.ID, armed); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	delete(s.resting, item.ID)
	delete(s.restingAt, item.ID)
	delete(s.waiting, item.ID)
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
	s.wakeUp()
	w.WriteHeader(http.StatusNoContent)
}

// answerThenUnblock records an answer against a marked item, if there is one,
// and then clears its mark — the sequence handleUnblock performs for a person
// typing into a card, and the doctor sweep performs for a script's verdict.
//
// The session comes out of the block while it still exists: clearing the mark
// is what forgets why the item stopped, and the conversation that asked is
// part of why. If the move that clears the mark then fails, the answer is
// taken back — a failed unblock must not strand a reply nobody's run will ever
// see, waiting to be said into a conversation that was never re-asked.
func (s *Server) answerThenUnblock(ctx context.Context, item model.Item, answer string) error {
	answer = strings.TrimSpace(answer)
	var resume model.Resume
	if answer != "" {
		s.mu.RLock()
		sess := s.blocks[item.ID].Session
		s.mu.RUnlock()
		// Layered onto whatever is armed, never replacing it: an action a
		// person pressed and a reply they then typed are two things said
		// about the same stop, and the next run should be handed both.
		resume = s.answers.Get(item.ID)
		resume.Answer, resume.Session = answer, sess
		if err := s.answers.Set(item.ID, resume); err != nil {
			return err
		}
	}
	if err := s.unblock(ctx, item); err != nil {
		if answer != "" {
			// Nothing will consume it: the mark is still in place, so no run
			// is about to be handed this reply. Roll back exactly the value
			// just set — never an unconditional delete, in case a person
			// somehow answered again in the gap.
			_, _ = s.answers.Take(item.ID, resume)
		}
		return err
	}
	if answer != "" {
		s.mu.Lock()
		s.answerInfo[item.ID] = AnswerView{RecordedAt: time.Now()}
		s.mu.Unlock()
	}
	return nil
}

// handleUnblockAll hands the whole board back at once, which is what an
// operator wants after fixing the thing that stopped it — an expired credential,
// a dirty worktree, an agent that was over its limit all night.
//
// In the background: each item is a provider write, and thirty of them is not a
// request. The board is republished as they land.
func (s *Server) handleUnblockAll(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	deps := pipeline.NewDeps(s.cfg, s.state.Items)
	var toUnblock []model.Item
	waitingOnYou, heldByDependencies := 0, 0
	for _, it := range s.state.Items {
		if !it.Blocked {
			continue
		}
		// Questions are left standing. This button means "I fixed the thing
		// that stopped you, try again" — an expired credential, a dirty
		// checkout, an agent that was over its limit all night. None of that
		// answers a question, and handing one back unanswered spends an agent
		// run to be asked it a second time. A question is cleared by answering
		// it on its own card, which is the only thing that resolves it. This
		// skip comes first: an item that is both asked and held is counted
		// once, under waitingOnYou — the question is what needs a person.
		if s.blocks[it.ID].Asked {
			waitingOnYou++
			continue
		}
		// The sequencing rule is asked directly — "would this item be held
		// if it were not marked" — never through pipeline.Target, which
		// refuses a marked item on its own mark before the gate is ever
		// reached. Decided from DependsOn and the listing alone: nothing
		// here reads the mark's own kind or the word "dependency" to decide
		// it — Scripts own that vocabulary, the engine never interprets it.
		//
		// The residual: a dependency the listing cannot see (an
		// un-onboarded issue, another repository) is not a hold the engine
		// can compute, so the item is still cleared here. agents/_deps
		// re-marks it at implement time, before the worktree and before any
		// model run, which is the accepted cost of that gap.
		if next, ok := nextStage(s.cfg, it.Stage); ok {
			if _, held := deps.Held(&it, next); held {
				heldByDependencies++
				continue
			}
		}
		toUnblock = append(toUnblock, it)
	}
	s.mu.RUnlock()
	s.spawn(func() { s.unblockAll(s.ctx, toUnblock) })
	writeJSON(w, map[string]int{
		"unblocking":         len(toUnblock),
		"waitingOnYou":       waitingOnYou,
		"heldByDependencies": heldByDependencies,
	})
}

// nextStage is the stage an item would move into were it not marked — the
// same "OnSuccess, or its own name on a re-run" logic Target and heldOf both
// use, needed here because a marked item's own path through Target is
// refused before it ever reaches that computation.
func nextStage(cfg *config.Config, stageName string) (string, bool) {
	st, ok := cfg.Stage(stageName)
	if !ok || st.Terminal {
		return "", false
	}
	if st.Runs() {
		return st.Name, true
	}
	if st.OnSuccess == "" {
		return "", false
	}
	return st.OnSuccess, true
}

// unblockAll clears marks one at a time. Sequentially on purpose: perSource is
// 1 because a source is a worktree, and while these writes touch no worktree,
// thirty concurrent `gh` calls against one repository is its own outage.
func (s *Server) unblockAll(ctx context.Context, held []model.Item) int {
	n := 0
	for _, it := range held {
		if err := s.unblock(ctx, it); err != nil {
			fmt.Fprintf(os.Stderr, "conveyor: could not unblock %s: %v\n", it.ID, err)
			continue
		}
		n++
	}
	return n
}

// releaseLimited hands back the items an agent's usage limit stopped, once that
// agent reports itself well again.
//
// This is the only mark the engine clears on its own initiative, and it is a
// narrow exception on purpose. A `decision` mark is a person's answer
// outstanding, and no amount of waiting produces one — clearing it spends an
// agent run to be told the same thing. A `limit` is the outside world: nothing
// was ever wrong with the item, the agent said so itself in the mark, and the
// quota comes back on a schedule the agent's own status script reports. Leaving
// those for a human to clear in the morning is a whole night of the line
// standing still for a reason that expired at 3am.
//
// It is not the model deciding flow. The pipeline is unchanged; this only takes
// off a mark that a script put on, when the same script says the cause is gone.
//
// ponytail: any agent recovering releases every `limit` mark, because there is
// one agent in use. Match a mark to the agent whose stage set it if two agents
// with separate quotas ever run side by side.
func (s *Server) releaseLimited(ctx context.Context) int {
	s.mu.RLock()
	var held []model.Item
	for _, it := range s.state.Items {
		if it.Blocked && s.blocks[it.ID].Kind == limitKind && !s.blocks[it.ID].Asked {
			held = append(held, it)
		}
	}
	s.mu.RUnlock()
	if len(held) == 0 {
		return 0
	}
	fmt.Fprintf(os.Stderr, "conveyor: an agent's quota is back; handing back %d item(s) it stopped\n", len(held))
	n := s.unblockAll(ctx, held)
	s.wakeUp()
	return n
}

// saidAt reads the two things a stopping script may say about its own stop,
// beyond the reason the mark already carries.
//
// The engine defines neither: an adapter that can be resumed writes
// {"session": "..."}, one that stopped on a question writes {"asked": true},
// and a script that says nothing is a condition with no way back — all three
// are correct. This is the same file the reason comes from, so a run pinned
// against retention carries the whole stop, not half of it.
func saidAt(dir string) (asked bool, session string, questions json.RawMessage) {
	if dir == "" {
		return false, "", nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return false, "", nil
	}
	var v struct {
		Asked     bool            `json:"asked"`
		Session   string          `json:"session"`
		Questions json.RawMessage `json:"questions"`
	}
	if json.Unmarshal(b, &v) != nil {
		return false, "", nil
	}
	// Only an array is a list of questions; anything else is not passed on to
	// a page that would try to walk it.
	if len(v.Questions) == 0 || v.Questions[0] != '[' {
		v.Questions = nil
	}
	return v.Asked, v.Session, v.Questions
}

// waitingAt reads what a run said it is waiting for out of its own result
// file. Absent, unreadable or malformed is simply "it did not say", which is
// the ordinary case: most stages that exit 10 have nothing to add.
func waitingAt(dir string) (model.Waiting, bool) {
	if dir == "" {
		return model.Waiting{}, false
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return model.Waiting{}, false
	}
	var v struct {
		Waiting *model.Waiting `json:"waiting"`
	}
	if json.Unmarshal(b, &v) != nil || v.Waiting == nil {
		return model.Waiting{}, false
	}
	// Neither half said anything: not a wait, just an empty object.
	if v.Waiting.Until.IsZero() && strings.TrimSpace(v.Waiting.Why) == "" {
		return model.Waiting{}, false
	}
	return *v.Waiting, true
}

// applyAgentStates turns what the agents said into what the scheduler does.
//
// Split out of askAgents so it can be driven with reports rather than scripts:
// the decision is the part worth testing, and running a status script to get at
// it would test the script instead.
//
// The probe is the authority on whether an agent is taking work. `limited`
// pauses it, until the reset it named if it named one. Anything else — `ok`,
// `unknown`, a probe that could not run at all — lifts a pause, including one a
// refused run set between probes. That direction is deliberate: a status script
// that breaks must not wedge the line shut, and letting work through is exactly
// the behaviour there was before any of this.
func (s *Server) applyAgentStates(out []AgentView, known []config.Agent) {
	for _, a := range out {
		if a.State == "limited" {
			until, _ := time.Parse(time.RFC3339, a.ResetsAt)
			reason := a.Summary
			if reason == "" {
				reason = a.Name + " reports it is out of quota"
			}
			if s.pauseFor(a.Name, until, reason, false) {
				if until.IsZero() {
					fmt.Fprintf(os.Stderr, "conveyor: %s is out of quota; holding work back until it says otherwise\n", a.Name)
				} else {
					fmt.Fprintf(os.Stderr, "conveyor: %s is out of quota; holding work back until %s\n",
						a.Name, until.Format(time.RFC3339))
				}
			}
			continue
		}
		if s.resumeAgent(a.Name) {
			fmt.Fprintf(os.Stderr, "conveyor: %s is taking work again\n", a.Name)
		}
	}

	// An agent with no status script can still be paused by a refused run, and
	// nothing above would ever lift that — it reports nothing, so there is no
	// `ok` to see. Give it one poll and then let work through again: a bounded
	// pause on no evidence, rather than an open-ended one nobody can end.
	reporting := map[string]bool{}
	for _, a := range out {
		reporting[a.Name] = true
	}
	for _, a := range known {
		if !reporting[a.Name] && s.resumeAgent(a.Name) {
			fmt.Fprintf(os.Stderr, "conveyor: %s has no status script to confirm the limit; taking work again\n", a.Name)
		}
	}
}

// pauseFor records that an agent is out of quota and the scheduler should stop
// handing it work. Returns true when this is news, so the caller can say so
// once rather than on every poll.
//
// A pause a probe found carries the reset the agent reported; one a refused run
// caused carries no deadline, because the mark does not know one. Neither
// outranks the other on purpose — whichever arrives with a deadline keeps it,
// so learning `resetsAt` a poll later does not lose the pause that beat it.
func (s *Server) pauseFor(agent string, until time.Time, reason string, fromMark bool) bool {
	if agent == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	was, had := s.paused[agent]
	p := PauseView{Agent: agent, Until: until, Since: time.Now(), Reason: reason, FromMark: fromMark}
	if had {
		p.Since = was.Since // one pause, however many times it is re-reported
		if until.IsZero() && !was.Until.IsZero() {
			p.Until = was.Until // a mark must not erase a deadline a probe knew
		}
		if reason == "" {
			p.Reason = was.Reason
		}
	}
	s.paused[agent] = p
	s.state.Paused = s.pausedList()
	return !had
}

// resumeAgent lifts a pause. Returns true when there was one to lift.
func (s *Server) resumeAgent(agent string) bool {
	s.mu.Lock()
	_, had := s.paused[agent]
	if had {
		delete(s.paused, agent)
		s.state.Paused = s.pausedList()
	}
	s.mu.Unlock()
	return had
}

// pausedList is the view of s.paused, in a stable order. Caller holds mu.
func (s *Server) pausedList() []PauseView {
	out := make([]PauseView, 0, len(s.paused))
	for _, p := range s.paused {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// agentPaused reports whether work for this agent is being held back, and
// clears a pause whose deadline has passed.
//
// Expiry is checked here rather than on a timer: the reset the agent named is
// the moment the quota comes back, and the scheduler asks this question every
// time it looks for work anyway.
func (s *Server) agentPaused(agent string) bool {
	if agent == "" {
		return false // a stage that runs no agent spends nobody's quota
	}
	s.mu.RLock()
	p, held := s.paused[agent]
	s.mu.RUnlock()
	if !held {
		return false
	}
	if p.Live(time.Now()) {
		return true
	}
	if s.resumeAgent(agent) {
		fmt.Fprintf(os.Stderr, "conveyor: %s's quota window has passed; taking work again\n", agent)
	}
	return false
}

// anyPaused reports whether any agent is being held back. It is what stops the
// stall timer clearing marks into a closed door.
func (s *Server) anyPaused() bool {
	s.mu.RLock()
	names := make([]string, 0, len(s.paused))
	for name := range s.paused {
		names = append(names, name)
	}
	s.mu.RUnlock()
	for _, name := range names {
		if s.agentPaused(name) {
			return true
		}
	}
	return false
}

// limitKind is the one word in the agents' blocked vocabulary the engine acts
// on. It is still the scripts' word, not the engine's: agents/_blocked defines
// it, and an agent that never says it simply never gets this behaviour.
const limitKind = "limit"

// unblock is one provider write: the same move that set the mark, with the mark
// off. The engine's note goes with it — the run that explains it is still
// filed, but the item is no longer asking anything of anyone.
func (s *Server) unblock(ctx context.Context, item model.Item) error {
	client, ok := s.eng.Client(item.Source)
	if !ok {
		return fmt.Errorf("%s: no provider client", item.Source)
	}
	if _, err := client.Move(ctx, &item, item.Stage, source.Mark{}); err != nil {
		return err
	}
	s.mu.Lock()
	for i := range s.state.Items {
		if s.state.Items[i].ID == item.ID {
			s.state.Items[i] = item
			break
		}
	}
	delete(s.blocks, item.ID)
	// Handing an item back is an answer, and an answer is exactly the change a
	// deferred stage was waiting to see.
	delete(s.resting, item.ID)
	delete(s.restingAt, item.ID)
	delete(s.waiting, item.ID)
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
	s.wakeUp()
	return nil
}

// whyStuck says why an item has nowhere to go, in the operator's terms.
//
// deps is the graph the caller already built from the full listing — never
// rebuilt here, since s.mu must already be released by the time this is
// called (see handleStart) and a fresh build would need the lock again.
func (s *Server) whyStuck(it model.Item, deps pipeline.Deps) string {
	if it.Blocked {
		return it.ID + " is waiting for a person — clear its mark and the pipeline takes it back"
	}
	st, ok := s.cfg.Stage(it.Stage)
	switch {
	case !ok:
		return fmt.Sprintf("%s is in %s, which is not a stage in this pipeline", it.ID, it.Stage)
	case st.Terminal:
		return fmt.Sprintf("%s is in %s, the end of the line", it.ID, it.Stage)
	}
	next := st.OnSuccess
	if st.Runs() {
		next = st.Name
	}
	if next != "" {
		if hold, held := deps.Held(&it, next); held {
			if hold.Blocked {
				return fmt.Sprintf("%s is held behind %s, which is marked in %s — clear that mark and this moves on its own",
					it.ID, hold.By, hold.Stage)
			}
			return fmt.Sprintf("%s is held behind %s, which is still in %s — it cannot enter %s first",
				it.ID, hold.By, hold.Stage, hold.Target)
		}
	}
	return fmt.Sprintf("%s has nowhere to go from %s", it.ID, it.Stage)
}

// heldOf is every item the sequencing rule is currently holding, keyed by id.
//
// Computed rather than remembered: a hold is a fact about where two items
// are standing right now, so a cached one would be wrong the moment either
// moves. deps must come from the same, full listing items was drawn from.
func heldOf(cfg *config.Config, items []model.Item, deps pipeline.Deps) map[string]pipeline.Hold {
	out := map[string]pipeline.Hold{}
	for i := range items {
		it := &items[i]
		if it.Blocked {
			continue // a mark is the whole story; a hold behind it says nothing
		}
		stage, ok := cfg.Stage(it.Stage)
		if !ok || stage.Terminal {
			continue
		}
		next := stage.OnSuccess
		if stage.Runs() {
			next = stage.Name
		} else if next == "" {
			continue
		}
		if h, held := deps.Held(it, next); held {
			out[it.ID] = h
		}
	}
	return out
}

// whyBusy names what is holding the slot, in the operator's terms.
//
// The locks are asked first and believed: they know which axis actually
// refused, and a resource — one account's quota, one deploy host — is held by
// work in some other repository as often as by anything visible here, so
// guessing from the active list alone names the wrong thing.
func (s *Server) whyBusy(src, stage string) string {
	held := s.eng.Locks().Holding(src, stage, s.cfg.ResourcesFor(src, stage)...)
	if name, ok := strings.CutPrefix(held, "resource "); ok {
		r := s.eng.Locks().Resources()[name]
		return fmt.Sprintf("all %d of %s is in use; %s waits for one to come free", r[1], name, stage)
	}
	active := s.activeList()
	for _, a := range active {
		if a.Source == src {
			return fmt.Sprintf("%s is busy with %s in %s", src, a.ItemID, a.Stage)
		}
	}
	for _, a := range active {
		if a.Stage == stage {
			return fmt.Sprintf("%s is busy with %s", stage, a.ItemID)
		}
	}
	return fmt.Sprintf("%s or %s is already busy", src, stage)
}

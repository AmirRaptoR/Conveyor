package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// poll re-lists every source on the configured interval, and does nothing
// else.
//
// Discovery is the only thing that finds new work, so it must not be able to
// wait behind it. Draining used to run on this goroutine and returned only when
// the pipeline was at rest: a 90-minute implement was a 90-minute blackout in
// which no source was listed at all, and an issue opened in another repository
// was not deprioritised but simply unseen.
func (s *Server) poll(ctx context.Context) {
	s.refresh(ctx)
	t := time.NewTicker(s.cfg.Poll.D())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refresh(ctx)
		}
	}
}

// stalled watches for the board stopping altogether and, on an interval, hands
// it back. Only when *everything* is marked: while one item can still move, a
// mark is a decision a person has to answer, and clearing it spends an agent
// run to be told the same thing.
//
// A total stall is a different animal. It is almost always the outside world —
// every agent over its usage limit, a credential that expired overnight, a
// worktree one dead run left dirty — and those come back on their own, hours
// after the board gave up. Without this the line stays stopped until somebody
// looks at it, which on a Sunday is the whole weekend.
//
// It reports what it did. An automatic recovery nobody can see is how a board
// starts lying about why work restarted.
func (s *Server) stalled(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.inFlight.Load() != 0 {
			continue // something is running; by definition not stalled
		}
		// The one stall this must not touch. Its whole premise is that the
		// cause may have passed — but an agent out of quota is a cause with a
		// known end, and handing every item back before that end spends one
		// refused run per item to learn what the pause already knows. That was
		// this timer's own worst hour: eight or nine items, every hour, all
		// night, against a weekly limit thirty hours from resetting.
		if s.anyPaused() {
			continue
		}
		s.mu.RLock()
		var held []model.Item
		moving := 0
		for _, it := range s.state.Items {
			switch {
			case it.Blocked:
				// A question is not part of a stall. It is not waiting for the
				// world to come back, it is waiting for a person, and clearing
				// it on a timer spends a run to be asked the same thing again.
				// It is not counted as movable either: a board holding nothing
				// but questions is genuinely stopped, and retrying it would be
				// re-asking every one of them every hour.
				if !s.blocks[it.ID].Asked {
					held = append(held, it)
				}
			default:
				if _, ok := pipeline.Target(s.cfg, &it); ok {
					moving++
				}
			}
		}
		s.mu.RUnlock()
		if moving > 0 || len(held) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "conveyor: every item is marked and nothing can move; clearing %d mark(s) to try again\n", len(held))
		n := s.unblockAll(ctx, held)
		fmt.Fprintf(os.Stderr, "conveyor: handed %d item(s) back to the pipeline\n", n)
		s.wakeUp()
	}
}

// button serves the tick control. In a running pipeline it means "look now", so
// it wakes the scheduler; when watching, it is the only thing that ever moves
// an item, so it performs the transition itself.
//
// Its own goroutine, for the same reason as everything else here: an advance
// takes as long as a stage does, and the poll must not be behind it.
func (s *Server) button(ctx context.Context, mode Mode) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.tick:
			switch mode {
			case ModeObserve:
				// Nothing here ever advances an item, not even a press
				// reaching this channel directly — the route above it
				// already refuses, but this is the guarantee, not the route.
				continue
			case ModeManual:
				s.advance(ctx)
				s.refresh(ctx)
			default: // auto
				// "Look again now", so nothing gets to say "not yet".
				s.mu.Lock()
				clear(s.resting)
				clear(s.restingAt)
				s.mu.Unlock()
				s.wakeUp()
			}
		}
	}
}

// wakeUp asks the scheduler for another pass. Never blocks: a full slot already
// carries the same question.
func (s *Server) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// schedule launches everything the locks permit and then sleeps until something
// could have changed — a transition finished, a listing arrived, the button was
// pressed. It never waits for the pipeline to be at rest, which is what let a
// long stage own the loop.
//
// One goroutine, so every launch decision is serialised: a concurrent refresh
// cannot make two passes decide the same transition, and the slot is still
// claimed before anything starts.
func (s *Server) schedule(ctx context.Context) {
	// since counts transitions launched since the pipeline was last at rest.
	// The bound is a circuit breaker, not a budget: items are finite and
	// terminal stages end the walk, so exceeding it means the stage graph
	// loops. That is a configuration error, and spinning on it silently would
	// burn an agent's turns until someone noticed.
	since, stalled := 0, false
	for {
		n := 0
		if !stalled {
			n = s.launch(ctx)
			since += n
		}
		switch {
		case n == 0 && s.inFlight.Load() == 0:
			// Nothing launchable and nothing running: the walk is over, and the
			// next one starts its count from zero.
			since, stalled = 0, false
		case !stalled && since > s.walkLimit():
			stalled = true
			fmt.Fprintln(os.Stderr, "conveyor: launched more transitions than there are items; the stage graph may loop")
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
	}
}

// walkLimit is how many transitions one walk of the board can reasonably take.
func (s *Server) walkLimit() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return 2*len(s.state.Items) + 1
}

func (s *Server) refresh(ctx context.Context) {
	if !s.polling.CompareAndSwap(false, true) {
		return // one is already running; a second would re-run every list script
	}
	defer s.polling.Store(false)
	// A listing is the other thing that can make work launchable, so the
	// scheduler is told about it the moment it lands.
	defer s.wakeUp()

	var items []model.Item
	var warnings []string
	// Accumulated during the loop below and applied only once, inside the
	// same lock that assigns Items/Warnings/UpdatedAt — so /api/state can
	// never serve a fresh lastListedAt beside the previous poll's items.
	// Two of the three ways to skip a source below (config problems, no
	// engine client) leave both maps untouched for that source: neither is
	// an attempt, so there is no outcome to record — the absence itself is
	// what the board reads as "never listed".
	listedNow := map[string]time.Time{}
	failedNow := map[string]string{}

	s.mu.Lock()
	s.state.Polling = true
	s.mu.Unlock()
	s.hub.publish(event{Kind: "polling"})

	for _, src := range s.cfg.Sources {
		if !src.OK() {
			continue // its problems are already on the board
		}
		client, ok := s.eng.Client(src.Name)
		if !ok {
			continue
		}
		// Stamped immediately before this source's own List call, not once
		// for the whole refresh: a slow source earlier in the loop must not
		// make this source's fresh listing look like it began before a
		// transition that actually completed while the slow one was still
		// running (F03).
		genStart := time.Now()
		s.mu.Lock()
		s.sourceGen[src.Name] = genStart
		s.mu.Unlock()
		res, err := client.List(ctx)
		if err != nil {
			warnings = append(warnings, src.Name+": "+err.Error())
			failedNow[src.Name] = err.Error()
			// A failed listing is no information about this source, not a
			// signal that its work vanished: keep what was already known
			// about it rather than have the wholesale assignment below wipe
			// it off the board. A *successful* listing that genuinely omits
			// an item still removes it — tombstones remain out of scope.
			items = append(items, s.mergeSourceListing(src.Name, genStart, nil, true)...)
			continue
		}
		listedNow[src.Name] = time.Now()
		for _, w := range res.Warnings {
			warnings = append(warnings, src.Name+": "+w.String())
		}
		items = append(items, s.mergeSourceListing(src.Name, genStart, res.Items, false)...)
	}
	items = dedupeCrossSource(items, func(msg string) { warnings = append(warnings, msg) })

	s.askAgents(ctx)
	s.recallBlocks(items)
	storage := s.storageUse()

	s.mu.Lock()
	// Both fields describe the latest attempt, not a high-water mark: a
	// success clears the previous failure, and a later failure sets it again
	// without disturbing the lastListedAt a prior success already wrote.
	for name, t := range listedNow {
		s.listedAt[name] = t
		delete(s.listErr, name)
	}
	for name, e := range failedNow {
		s.listErr[name] = e
	}
	s.state.Items = items
	s.state.Warnings = warnings
	s.state.Sources = sourceViews(s.cfg, s.listedAt, s.listErr)
	s.state.Order = s.order.IDs()
	s.state.UpdatedAt = time.Now()
	s.state.Polling = false
	s.state.Storage = storage
	s.state.PollNs = s.cfg.Poll.D()
	// The listing every deferred stage was waiting for. Whatever it exited 10
	// over has had a poll interval to change — but only for an item whose
	// deferral was set *before* its source's own listing began (F03). One set
	// by a transition that completed after that listing began is not yet
	// answered by it: the listing may well have reported that transition's
	// pre-image, and clearing the deferral would have the scheduler retry a
	// stage the item is not actually resting in front of anymore, or retry an
	// infrastructure failure the listing never had a chance to resolve.
	srcOf := make(map[string]string, len(items))
	for _, it := range items {
		srcOf[it.ID] = it.Source
	}
	for id := range s.resting {
		src, onBoard := srcOf[id]
		gen, knownGen := s.sourceGen[src]
		since, haveSince := s.restingAt[id]
		if onBoard && knownGen && haveSince && since.After(gen) {
			continue // the listing that just landed predates this deferral
		}
		delete(s.resting, id)
		delete(s.restingAt, id)
	}
	// A mark cleared on the provider — a label removed by hand — takes its
	// note with it. The provider is the authority on whether, always.
	marked := map[string]bool{}
	onBoard := make(map[string]bool, len(items))
	for _, it := range items {
		onBoard[it.ID] = true
		if it.Blocked {
			marked[it.ID] = true
		}
	}
	for id := range s.blocks {
		if !marked[id] {
			delete(s.blocks, id)
		}
	}
	// An item that is no longer on the board — done and rolled off, or gone
	// from the source entirely — has nothing left for its clock to measure.
	for id := range s.times {
		if !onBoard[id] {
			delete(s.times, id)
		}
	}
	for id := range s.transitionErrs {
		if !onBoard[id] {
			delete(s.transitionErrs, id)
		}
	}
	for id := range s.confirmedAt {
		if !onBoard[id] {
			delete(s.confirmedAt, id)
		}
	}
	for id := range s.answerInfo {
		if !onBoard[id] {
			delete(s.answerInfo, id)
		}
	}
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
}

// mergeSourceListing reconciles one source's listing (fresh may be nil, for a
// failed List) against the board's own live cache, so that a listing which
// began before a transition completed cannot publish that transition's
// pre-image (F03). genStart is when this source's List call began.
//
// The rule is by *when* the listing began, never by what it says: an item
// reported differently by a listing that began after the transition
// completed — a person moved it by hand — is trusted, even though its content
// disagrees with the transition. Only a listing that began earlier is
// refused, because it may be reporting what the provider looked like before
// the transition wrote to it.
func (s *Server) mergeSourceListing(src string, genStart time.Time, fresh []model.Item, failed bool) []model.Item {
	s.mu.RLock()
	cached := make(map[string]model.Item, len(s.state.Items))
	for _, it := range s.state.Items {
		if it.Source == src {
			cached[it.ID] = it
		}
	}
	s.mu.RUnlock()

	if failed {
		// No information about this source at all, not a signal that its
		// work vanished: keep everything already known about it.
		out := make([]model.Item, 0, len(cached))
		for _, it := range cached {
			out = append(out, it)
		}
		return out
	}

	out := make([]model.Item, 0, len(fresh)+len(cached))
	seen := make(map[string]bool, len(fresh))
	for _, it := range fresh {
		seen[it.ID] = true
		s.mu.RLock()
		confirmedAt, haveConfirmed := s.confirmedAt[it.ID]
		s.mu.RUnlock()
		if haveConfirmed && confirmedAt.After(genStart) {
			// A transition finished after this listing began: trust the
			// cache, which already holds that transition's own outcome,
			// over content this listing may have read beforehand.
			if cur, ok := cached[it.ID]; ok {
				out = append(out, cur)
				continue
			}
		}
		out = append(out, it)
	}
	// An item this source previously reported that the fresh listing does not
	// (or could not, on a failed List) is normally gone — a successful
	// listing that genuinely omits an item still removes it; tombstones stay
	// out of scope. But its absence here is not new information when a
	// transition for it is still running, or completed after this listing
	// began: the listing did not include it because it was not done yet, not
	// because the provider stopped reporting it.
	for id, cur := range cached {
		if seen[id] {
			continue
		}
		if _, working := s.working.Load(id); working {
			out = append(out, cur)
			continue
		}
		s.mu.RLock()
		confirmedAt, haveConfirmed := s.confirmedAt[id]
		s.mu.RUnlock()
		if haveConfirmed && confirmedAt.After(genStart) {
			out = append(out, cur)
		}
	}
	return out
}

// dedupeCrossSource enforces CONTRACTS.md §1's duplicate-id rule across the
// whole poll, not merely within one source's own listing.
// source.Client.validate already catches a source repeating its own id; this
// catches two different sources emitting the same one. The server keys
// working, blocks, times and the manual order by the bare id, and a second
// source silently sharing it would have all of those disagree about which
// item a given id names. First in configuration order wins, matching the
// single-source rule, because items arrive here in that same order.
func dedupeCrossSource(items []model.Item, warn func(string)) []model.Item {
	winner := make(map[string]string, len(items)) // id -> the source that kept it
	out := make([]model.Item, 0, len(items))
	for _, it := range items {
		if src, dup := winner[it.ID]; dup {
			warn(fmt.Sprintf("%s: duplicate id %q also reported by %s; %s wins", it.Source, it.ID, src, src))
			continue
		}
		winner[it.ID] = it.Source
		out = append(out, it)
	}
	return out
}

// askAgents runs each agent's status script and collects what it says.
//
// On the discovery tick, beside the list scripts, because it is the same kind
// of question: what does the outside world look like right now. Not on a timer
// of its own — a status probe every minute would file a run a minute, and the
// run history is the log, not a metrics store.
//
// The engine reads three fields and interprets one word of them. It does not
// know what a usage window is, and must not learn: which limits an agent has,
// and what counts against them, is the agent's business.
func (s *Server) askAgents(ctx context.Context) {
	agents := s.cfg.AgentsInUse()
	out := make([]AgentView, 0, len(agents))
	for _, a := range agents {
		if a.Status == "" {
			continue // an agent with nothing to say is not a problem
		}
		v := AgentView{Name: a.Name, State: "unknown", At: time.Now()}
		res, err := s.run.Run(ctx, runner.Spec{
			Script:  a.Status,
			Kind:    "status",
			Source:  a.Name,
			Timeout: 30 * time.Second,
		})
		switch {
		case err != nil:
			v.Error = err.Error()
		case res.Run.Outcome != model.OutcomeSuccess:
			v.Error = fmt.Sprintf("status exited %d (%s)", res.Run.ExitCode, res.Run.Outcome)
		case len(res.Data) == 0:
			v.Error = "status wrote nothing to $CONVEYOR_RESULT"
		default:
			var got AgentView
			if json.Unmarshal(res.Data, &got) != nil {
				v.Error = "status result is not a JSON object"
				break
			}
			got.Name, got.At = a.Name, time.Now()
			if got.State != "ok" && got.State != "limited" {
				got.State = "unknown" // the vocabulary is closed; anything else is silence
			}
			v = got
		}
		out = append(out, v)
	}
	s.mu.Lock()
	was := make(map[string]string, len(s.state.Agents))
	for _, a := range s.state.Agents {
		was[a.Name] = a.State
	}
	s.state.Agents = out
	s.mu.Unlock()

	s.applyAgentStates(out, agents)

	// An agent saying it is well again is the one thing besides a person that
	// takes a mark off. See releaseLimited for why only this one.
	//
	// Any state but `ok` before it counts, not just `limited`, so that the first
	// probe after a restart is an edge too: coming up in the morning to marks
	// left by a quota that returned at 3am is the case this is for, and there is
	// no transition to see because the process that saw the limit is gone.
	// `ok` → `ok` is not an edge, which is what keeps a status script that lags
	// behind a fresh limit from marking and releasing the same item in a loop.
	for _, a := range out {
		if a.State == "ok" && was[a.Name] != "ok" {
			s.releaseLimited(ctx)
			break
		}
	}
}

// recallBlocks fills in why the marked items are marked, for marks this
// process did not make — everything on the board after a restart, and
// anything a person labelled by hand — and, in the same pass, fills in when
// any board item arrived in the stage it currently sits in, for entries this
// process did not write itself.
//
// The runs are the durable record: the one that marked an item wrote its
// reason to $CONVEYOR_RESULT, and CONTRACTS §6 pins that run against
// retention for exactly this; a successful move is the durable record of an
// arrival, for the same reason. One walk fills every gap at once — for both
// blocks and times — because walking it per item, or walking it twice, would
// be one or two full history scans for each card on a stalled board.
func (s *Server) recallBlocks(items []model.Item) {
	s.mu.RLock()
	wantBlocks := map[string]bool{}
	listedReason := map[string]string{}
	for _, it := range items {
		if it.Blocked {
			if _, known := s.blocks[it.ID]; !known {
				wantBlocks[it.ID] = true
			}
			if it.BlockReason != "" {
				listedReason[it.ID] = it.BlockReason
			}
		}
	}
	stageOf := make(map[string]string, len(items))
	wantTimes := map[string]bool{}
	for _, it := range items {
		stageOf[it.ID] = it.Stage
		t, known := s.times[it.ID]
		if !known || t.Stage != it.Stage {
			wantTimes[it.ID] = true
		}
	}
	s.mu.RUnlock()
	if len(wantBlocks) == 0 && len(wantTimes) == 0 {
		return
	}

	foundBlocks := map[string]Block{}
	foundTimes := map[string]ItemTime{}
	s.walkRuns(func(m RunMeta) bool {
		if wantBlocks[m.ItemID] && m.Kind == "stage" {
			switch m.Outcome {
			case model.OutcomeBlocked, model.OutcomeFailure, model.OutcomeTimeout:
				var data json.RawMessage
				if b, err := os.ReadFile(filepath.Join(m.Dir, "result.json")); err == nil {
					data = b
				}
				timeout := time.Duration(0)
				if st, ok := s.cfg.Stage(m.To); ok {
					timeout = st.Timeout.D()
				}
				mark := pipeline.Marked(m.To, m.Run, data, timeout, 0)
				asked, session, questions := saidAt(m.Dir)
				foundBlocks[m.ItemID] = Block{
					Kind:      mark.Kind,
					Reason:    mark.Reason,
					Stage:     m.To,
					RunID:     m.ID,
					At:        m.FinishedAt,
					Asked:     asked,
					Session:   session,
					Questions: questions,
				}
				delete(wantBlocks, m.ItemID)
			}
		}
		// Because the walk is newest-first, the first successful move that
		// landed this item in its current stage *is* the latest entry into
		// it — exactly the instant a card's age should be measured from.
		if wantTimes[m.ItemID] && m.Kind == "move" && m.Outcome == model.OutcomeSuccess &&
			m.From != m.To && m.To == stageOf[m.ItemID] {
			foundTimes[m.ItemID] = ItemTime{Stage: m.To, EnteredStage: m.FinishedAt, RunID: m.ID}
			delete(wantTimes, m.ItemID)
		}
		return len(wantBlocks) > 0 || len(wantTimes) > 0
	})

	s.mu.Lock()
	for id, b := range foundBlocks {
		if _, known := s.blocks[id]; !known {
			s.blocks[id] = b
		}
	}
	// Marked, and no run to explain it: either someone put the label on by
	// hand, or this poll's listing supplied its own reason (a closed issue
	// found sitting in a non-terminal stage, say) — CONTRACTS.md §6. The
	// listing's own words beat the generic fallback whenever it gave one.
	for id := range wantBlocks {
		if _, known := s.blocks[id]; !known {
			if reason, ok := listedReason[id]; ok {
				s.blocks[id] = Block{Kind: "by hand", Reason: reason}
				continue
			}
			s.blocks[id] = Block{Kind: "by hand",
				Reason: "marked outside the pipeline; there is no run to explain it"}
		}
	}
	// No matching move survives in retained history: an item onboarded
	// straight into its stage, one whose move was swept by retention, or one
	// whose stage changed some other way — a person relabelling it by hand.
	// History that is not there is not invented, so a stale entry left over
	// from before is dropped rather than kept naming a stage the item has
	// since left: a wrong chip is worse than no chip.
	//
	// Both loops check the entry as it stands now, not as it stood when the
	// scan above started: walkRuns runs unlocked, and a transition landing on
	// this same item while it ran already wrote the correct entry through
	// applyTransition. A scan that started before that write must not undo it
	// on the strength of what it saw before the write happened — the same
	// race the found-block merge above is already guarded against.
	for id := range wantTimes {
		if t, known := s.times[id]; !known || t.Stage != stageOf[id] {
			delete(s.times, id)
		}
	}
	for id, t := range foundTimes {
		if cur, known := s.times[id]; !known || cur.Stage != stageOf[id] {
			s.times[id] = t
		}
	}
	s.mu.Unlock()
}

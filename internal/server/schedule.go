package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
)

// claimRefusal is why claimAndLaunch declined to start a transition.
type claimRefusal int

const (
	claimAccepted        claimRefusal = iota
	claimItemBusy                     // this item already has a transition in flight
	claimSlotBusy                     // the (source, stage) slot has none free
	claimAgentPaused                  // the stage's agent is over quota and overridePause is false
	claimManuallyPaused               // an operator paused this source, or the whole board
	claimBudgetExhausted              // this item, or the board's day, spent its execution ceiling
)

// claim is the only place s.working is populated. Every dispatch path — the
// scheduler (launch), the tick button (advance) and a manual start
// (handleStart) — calls this and nothing else stores into working, so "an
// item has a transition in flight" is asserted in one place instead of three.
//
// The reservation is atomic: working.LoadOrStore either claims the item or
// finds it already claimed, with no gap in which a second caller can read
// "free" before the first writes "taken". The (source, stage) slot is taken
// only after the item is reserved, and released again immediately if the slot
// turns out to be full — a caller that gets back anything other than
// claimAccepted has changed nothing: working and Locks are exactly as they
// were found.
//
// overridePause is true only for the tick button: CLAUDE.md is explicit that
// "the tick button still overrides" a paused agent, because under -watch it
// is the only thing that moves anything. Every other caller is stopped by a
// paused agent exactly as it is stopped by a busy slot.
//
// A configured execution budget (#39) is likewise never lifted by
// overridePause: it is an operator-defined ceiling, not a quota the outside
// world reports, so only an explicit budget override — never the tick
// button — spends past it. It is checked last, once every other gate has
// already passed, so a run is counted only when it is certain to actually
// launch: a claim refused for a busy slot or a paused source spends nothing.
//
// claim only reserves; it does not run anything. advance runs the transition
// inline so it can refresh right afterwards (the button's contract under
// -watch), while launch and handleStart hand it to a goroutine — both call
// this same function first.
func (s *Server) claim(item model.Item, target string, overridePause bool) claimRefusal {
	// An operator's own pause, unlike a paused agent's quota, is never
	// overridden — not even by overridePause, which exists only because the
	// tick button is the sole mover under -watch and must not be wedged shut
	// by an agent's own quota. A manual pause is the person's own decision,
	// and only their Resume undoes it (#39).
	if s.manuallyPaused(item.Source) {
		return claimManuallyPaused
	}
	if !overridePause && s.agentPaused(s.cfg.AgentFor(item.Source, target)) {
		return claimAgentPaused
	}
	if _, already := s.working.LoadOrStore(item.ID, struct{}{}); already {
		return claimItemBusy
	}
	if !s.eng.Locks().TryAcquire(item.Source, target, s.cfg.ResourcesFor(item.Source, target)...) {
		s.working.Delete(item.ID)
		return claimSlotBusy
	}
	// Spent last, once nothing else stands between this item and actually
	// launching: a claim refused for a busy slot or a paused source has run
	// nothing and must not count against either ceiling, so the reservation
	// itself is what claim() returns claimAccepted for. On refusal, unwind
	// everything already reserved — the same "changed nothing" guarantee
	// claimSlotBusy above keeps.
	if ok, _ := s.reserveBudget(item.ID); !ok {
		s.working.Delete(item.ID)
		s.eng.Locks().Release(item.Source, target, s.cfg.ResourcesFor(item.Source, target)...)
		return claimBudgetExhausted
	}
	return claimAccepted
}

// claimAndLaunch reserves the item (via claim) and, once reserved, runs the
// transition asynchronously — the shape every dispatch path except the tick
// button wants.
func (s *Server) claimAndLaunch(ctx context.Context, item model.Item, target string, overridePause bool) claimRefusal {
	r := s.claim(item, target, overridePause)
	if r == claimAccepted {
		s.inFlight.Add(1)
		go s.transition(ctx, item, target)
	}
	return r
}

// launch starts every transition the locks currently permit and returns how
// many it started. Candidates whose source or target stage is already busy are
// skipped rather than queued, so a slow stage never holds up a free one.
func (s *Server) launch(ctx context.Context) int {
	// Shutting down: nothing new starts, whatever the locks would otherwise
	// permit. schedule's own ctx.Done() case returns it from the loop right
	// after, but a claim made in the meantime would still need reaping.
	if ctx.Err() != nil {
		return 0
	}
	s.mu.RLock()
	items := append([]model.Item(nil), s.state.Items...)
	resting := make(map[string]bool, len(s.resting))
	for id := range s.resting {
		resting[id] = true
	}
	// A source whose latest listing failed or timed out is showing its
	// last-good items, not this poll's — stale state is for reading, never
	// for acting on (CLAUDE.md), so nothing routed through it is dispatched
	// until a listing confirms it again.
	staleSrc := make(map[string]bool, len(s.listErr))
	for name := range s.listErr {
		staleSrc[name] = true
	}
	s.mu.RUnlock()
	order := s.order.IDs()

	n := 0
	// Refused axes. A refusal means that source or that stage is genuinely full,
	// so nothing else routed there can start either; without this the loop keeps
	// re-picking the same candidate and never terminates.
	fullSrc := map[string]bool{}
	fullStage := map[string]bool{}
	fullResource := map[string]bool{}
	// Items claimAndLaunch has already turned away this pass — a race with a
	// manual start, not a full axis, so it excludes only this one candidate
	// rather than every item sharing its source or stage.
	busy := map[string]bool{}

	for {
		free := items[:0:0]
		for _, it := range items {
			target, ok := pipeline.Target(s.cfg, &it)
			if !ok || fullSrc[it.Source] || fullStage[target] || busy[it.ID] ||
				s.eng.Locks().Busy(it.Source, target, s.cfg.ResourcesFor(it.Source, target)...) {
				continue
			}
			if staleSrc[it.Source] {
				continue // its items are its last-good listing, not this poll's
			}
			if spends(s.cfg.ResourcesFor(it.Source, target), fullResource) {
				continue // something it needs is already all in use
			}
			// Whose quota this would spend, and whether they have any. Checked
			// here and not in Pick, because it is a fact about the world right
			// now rather than about the ordering: the item is still next, the
			// line simply cannot afford it yet.
			if s.agentPaused(s.cfg.AgentFor(it.Source, target)) {
				continue
			}
			// An operator's own pause, global or on this item's source —
			// filtered here, before Pick, for the same reason a paused
			// agent is: it is a fact about the world right now, not about
			// the ordering, so a held item is simply not a candidate this
			// pass rather than one Pick ranks and claim then refuses.
			if s.manuallyPaused(it.Source) {
				continue
			}
			// This item's own execution budget, or the board's daily one —
			// a fact about the world right now, filtered here for the same
			// reason a paused agent and a manual pause are: claim spends the
			// reservation atomically and is the authority, this only saves
			// Pick from ranking a candidate that cannot actually launch.
			if !s.budgetAvailable(it.ID) {
				continue
			}
			if _, running := s.working.Load(it.ID); running {
				continue // already being worked; the locks do not know that
			}
			if resting[it.ID] {
				continue // it exited 10 here and asked for the next poll, not this one
			}
			free = append(free, it)
		}
		item, target := pipeline.Pick(s.cfg, free, order)
		if item == nil {
			return n
		}
		// Claim before launching, atomically: claimAndLaunch reserves the item
		// and the (source, stage) slot together and releases both if either is
		// refused, so a concurrent manual start or scheduler pass can never
		// double-launch this item.
		switch s.claimAndLaunch(ctx, *item, target, false) {
		case claimSlotBusy:
			// Which axis refused decides how much of the listing this rules
			// out. A full source or stage rules out everything routed there;
			// a full resource rules out everything that spends it, which may
			// be a different set entirely — an agent's quota is shared across
			// every repository and stage that names it.
			switch held := s.eng.Locks().Holding(item.Source, target, s.cfg.ResourcesFor(item.Source, target)...); {
			case strings.HasPrefix(held, "resource "):
				fullResource[strings.TrimPrefix(held, "resource ")] = true
			default:
				fullSrc[item.Source] = true
				fullStage[target] = true
			}
			continue
		case claimItemBusy, claimAgentPaused, claimManuallyPaused, claimBudgetExhausted:
			busy[item.ID] = true
			continue
		}
		n++
	}
}

// transition runs one launched transition and gives its slot back.
//
// The order of the three finishing steps is the whole of it: the lock goes
// first, the count second, and only then is the scheduler woken. Waking it
// while the slot is still held would have it find the source busy and go back
// to sleep, and the release that followed would wake nothing.
func (s *Server) transition(ctx context.Context, item model.Item, target string) {
	defer s.wakeUp()
	defer s.inFlight.Add(-1)
	defer s.working.Delete(item.ID)
	defer s.eng.Locks().Release(item.Source, target, s.cfg.ResourcesFor(item.Source, target)...)
	s.runOne(ctx, item, target)
}

// spends reports whether any of these resources is in the exhausted set.
func spends(resources []string, full map[string]bool) bool {
	for _, r := range resources {
		if full[r] {
			return true
		}
	}
	return false
}

// runOne performs one transition and keeps the board honest about it.
func (s *Server) runOne(ctx context.Context, item model.Item, target string) {
	// A child of ctx, one per transition, so cancelling this item's run —
	// handleCancel calling the func stored below — reaches only this run's
	// process group (runner.Run's own runCtx.Done() case) and never another
	// one sharing the same parent. A fresh attempt supersedes whatever audit
	// trail the last one left.
	//
	// Registered — and the stale cancel record cleared — before setActive
	// publishes this run as active: a client that reacts to that event by
	// cancelling at once must find a cancel func already in place, never a
	// 409 for a run the board just told it was running.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	s.cancelFns.Store(item.ID, cancelRun)
	defer s.cancelFns.Delete(item.ID)
	s.mu.Lock()
	delete(s.cancels, item.ID)
	s.mu.Unlock()

	s.setActive(item.ID, &Active{Source: item.Source, Stage: target, ItemID: item.ID, Title: item.Title, StartedAt: time.Now()})
	defer s.setActive(item.ID, nil)

	// Read, not taken. An answer is spent when the run it was written for
	// actually ran — including one that stops to ask something else, which is a
	// new question and wants a new answer. A run that failed outright never got
	// to use it, and a paragraph a person typed is not something to lose to a
	// bad agent invocation: the answer is kept and the session dropped, because
	// a resume that did not work names a conversation worth abandoning.
	resume := s.answers.Get(item.ID)
	tr, err := s.eng.Advance(runCtx, item.Source, &item, target, resume)
	if tr == nil {
		// An unknown source or stage: a config problem, not a transient one,
		// and nothing ran — there is nothing to mark, move or spend an answer
		// on. Recorded and rested the same way an initial move failure is
		// (below), so the scheduler does not retry it on every wake, and the
		// board keeps the reason past the next successful listing.
		if err != nil {
			fmt.Fprintf(os.Stderr, "conveyor: %s: %v\n", item.ID, err)
			now := time.Now()
			s.mu.Lock()
			s.transitionErrs[item.ID] = TransitionError{Reason: err.Error(), At: now}
			s.resting[item.ID] = true
			s.restingAt[item.ID] = now
			s.mu.Unlock()
		}
		return
	}
	switch {
	case tr.Outcome == "":
		// Unset only when nothing ran at all — the initial provider move
		// failed before any script had the chance to consume the answer. It
		// must survive to be handed to whichever run actually receives it.
	case tr.Outcome == model.OutcomeFailure || tr.Outcome == model.OutcomeTimeout:
		if resume.Answer != "" && resume.Session != "" {
			// The session is dropped and everything else a person said is
			// kept: a resume that did not work names a conversation worth
			// abandoning, but the reply and the button they pressed were
			// never actually acted on and should reach the run that is.
			_ = s.answers.Set(item.ID, model.Resume{Answer: resume.Answer, Manual: resume.Manual})
		}
	default:
		spent, err := s.answers.Take(item.ID, resume)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conveyor: %s: could not record its answer as spent: %v\n", item.ID, err)
		} else if spent && resume.Answer != "" {
			// Only now — after the run named by tr has actually been handed
			// the answer — does the board get to say who received it.
			s.mu.Lock()
			info := s.answerInfo[item.ID]
			info.ConsumedBy = tr.RunID
			s.answerInfo[item.ID] = info
			s.mu.Unlock()
		}
	}
	s.applyTransition(tr)
	s.hub.publish(event{Kind: "transition", Transition: tr})
}

// applyTransition writes an item's new stage into the cache the moment it is
// known, rather than waiting for the next poll.
//
// Without it the drain re-listed while transitions were in flight, saw an item
// still sitting in a stage that runs a script, and treated finished work as an
// interrupted job — running an agent over it a second time. The provider is
// still the authority; this only stops the cache lying in the gap.
func (s *Server) applyTransition(tr *pipeline.Transition) {
	s.mu.Lock()
	for i := range s.state.Items {
		if s.state.Items[i].ID == tr.Item.ID {
			s.state.Items[i] = tr.Item
			break
		}
	}
	// Written here rather than recovered later: this is the one moment the
	// reason exists in full, and a run history sweep must not be what stands
	// between an operator and why their board stopped.
	now := time.Now()
	// This transition just wrote the authoritative cache entry for the item;
	// a listing whose own List call for this source began before now may
	// still be reporting whatever the provider looked like beforehand, and
	// refresh must not let it overwrite this (F03).
	s.confirmedAt[tr.Item.ID] = now
	asked := false
	if tr.Blocked {
		var session string
		var questions json.RawMessage
		asked, session, questions = saidAt(tr.RunDir)
		s.blocks[tr.Item.ID] = Block{Kind: tr.Kind, Reason: tr.Reason, Stage: tr.Stage,
			RunID: tr.RunID, At: now, Asked: asked, Session: session, Questions: questions}
	} else {
		delete(s.blocks, tr.Item.ID)
	}
	// The item arrived somewhere new, so its clock starts fresh. A transition
	// that leaves it in the stage it was already in — a re-run, a
	// confirmation, a mark — did not arrive anywhere, and its existing entry,
	// if it has one, is left untouched.
	if tr.Item.Stage != tr.From {
		s.times[tr.Item.ID] = ItemTime{Stage: tr.Item.Stage, EnteredStage: now, RunID: tr.RunID}
	}
	// tr.Err is set on an infrastructure failure — an initial provider move
	// that failed before any script ran (tr.Outcome == "") is the one F04
	// specifically calls out, since there is no run and no mark to show for
	// it otherwise. Recorded the same way Blocks is: kept until a later
	// transition for this item succeeds, so a listing wholesale-replacing
	// State.Warnings does not erase the only trace of it.
	if tr.Err != nil {
		s.transitionErrs[tr.Item.ID] = TransitionError{Reason: tr.Err.Error(), At: now}
	} else {
		delete(s.transitionErrs, tr.Item.ID)
	}
	// A no-op in the stage the item was already in is the script saying it has
	// nothing to do yet — a pull request still settling, a check still running.
	// Nothing about the board will answer it differently one second later, so
	// it waits for the listing. Any other outcome is progress, and progress
	// ends the deferral: the item is somewhere new, or marked, or due a retry.
	//
	// An initial provider move that failed gets the same deferral and for the
	// same reason: nothing ran, nothing changed, and retrying it on every
	// scheduler wake instead of waiting for the next listing would hammer a
	// provider that is already failing.
	if (tr.Outcome == model.OutcomeNoop && tr.From == tr.Stage) || (tr.Outcome == "" && tr.Err != nil) {
		s.resting[tr.Item.ID] = true
		s.restingAt[tr.Item.ID] = now
		// What it said it is waiting for, if it said anything. Set and
		// cleared together with the deferral, so a countdown can never
		// outlive the wait it was counting down to.
		if wt, ok := waitingAt(tr.RunDir); ok {
			s.waiting[tr.Item.ID] = wt
		} else {
			delete(s.waiting, tr.Item.ID)
		}
	} else {
		delete(s.resting, tr.Item.ID)
		delete(s.restingAt, tr.Item.ID)
		delete(s.waiting, tr.Item.ID)
	}
	s.mu.Unlock()

	// The two moments a person wants to hear about without watching: a
	// question only they can answer, and an item reaching the end of the line.
	if asked {
		s.notify("Needs you: "+tr.Item.Title, tr.Reason, tr.Item.ID)
	} else if st, ok := s.cfg.Stage(tr.Item.Stage); ok && st.Terminal && tr.From != tr.Item.Stage {
		s.notify("Shipped: "+tr.Item.Title, tr.Item.ID+" reached "+tr.Item.Stage, tr.Item.ID)
	}

	// A `limit` mark is account-level news, not a fact about this one item:
	// every other item routed to the same agent is about to meet the same wall.
	// Waiting for the next status probe to notice costs a whole poll of runs
	// spent to be told what this one just said, so the mark itself pauses the
	// agent and the probe refines the deadline afterwards.
	if tr.Blocked && tr.Kind == limitKind {
		agent := s.cfg.AgentFor(tr.Item.Source, tr.Stage)
		if s.pauseFor(agent, time.Time{}, tr.Reason, true) {
			fmt.Fprintf(os.Stderr, "conveyor: %s was refused for being out of quota; holding work back from it\n", agent)
		}
	}
}

// advance runs a single transition: the button, which works whether or not the
// pipeline is running itself.
func (s *Server) advance(ctx context.Context) bool {
	// Shutting down: the button is the only mover in -watch mode, so this is
	// the whole of what a drain waits for there, not an edge of it.
	if ctx.Err() != nil {
		return false
	}
	s.mu.RLock()
	items := append([]model.Item(nil), s.state.Items...)
	s.mu.RUnlock()

	item, target := pipeline.Pick(s.cfg, items, s.order.IDs())
	if item == nil {
		return false
	}
	// overridePause: true. CLAUDE.md — "the tick button still overrides" — and
	// under -watch this is the only thing that ever moves an item, so a paused
	// agent must not be able to wedge the whole board shut.
	if s.claim(*item, target, true) != claimAccepted {
		return false
	}
	defer s.eng.Locks().Release(item.Source, target, s.cfg.ResourcesFor(item.Source, target)...)
	defer s.working.Delete(item.ID)
	// Counted the same way schedule's launches are, so a drain waiting on
	// inFlight actually waits for this too — the only mover with -watch set,
	// where schedule never launches anything at all.
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	s.runOne(ctx, *item, target)
	return true
}

// setActive records a transition as in flight, or clears it when a is nil.
func (s *Server) setActive(itemID string, a *Active) {
	if a == nil {
		s.active.Delete(itemID)
	} else {
		s.active.Store(itemID, *a)
	}
	s.mu.Lock()
	s.state.Active = s.activeList()
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
}

func (s *Server) activeList() []Active {
	out := []Active{}
	s.active.Range(func(_, v any) bool {
		out = append(out, v.(Active))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ItemID < out[j].ItemID })
	return out
}

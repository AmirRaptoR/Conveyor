package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
)

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
	s.mu.RUnlock()
	order := s.order.IDs()

	n := 0
	// Refused axes. A refusal means that source or that stage is genuinely full,
	// so nothing else routed there can start either; without this the loop keeps
	// re-picking the same candidate and never terminates.
	fullSrc := map[string]bool{}
	fullStage := map[string]bool{}

	for {
		free := items[:0:0]
		for _, it := range items {
			target, ok := pipeline.Target(s.cfg, &it)
			if !ok || fullSrc[it.Source] || fullStage[target] ||
				s.eng.Locks().Busy(it.Source, target) {
				continue
			}
			// Whose quota this would spend, and whether they have any. Checked
			// here and not in Pick, because it is a fact about the world right
			// now rather than about the ordering: the item is still next, the
			// line simply cannot afford it yet.
			if s.agentPaused(s.cfg.AgentFor(it.Source, target)) {
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
		// Claim before launching. Checking Busy and starting a goroutine leaves
		// a gap in which the next pass sees the slot free and decides the same
		// thing again; the duplicate then bails, having spent a launch. The
		// locks are the authority on capacity — Busy above re-reads them every
		// iteration, so the pass keeps filling slots until they are actually
		// gone.
		if !s.eng.Locks().TryAcquire(item.Source, target) {
			fullSrc[item.Source] = true
			fullStage[target] = true
			continue
		}
		it, to := *item, target
		s.working.Store(it.ID, struct{}{})
		s.inFlight.Add(1)
		n++
		go s.transition(ctx, it, to)
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
	defer s.eng.Locks().Release(item.Source, target)
	s.runOne(ctx, item, target)
}

// runOne performs one transition and keeps the board honest about it.
func (s *Server) runOne(ctx context.Context, item model.Item, target string) {
	s.setActive(item.ID, &Active{Source: item.Source, Stage: target, ItemID: item.ID, Title: item.Title, StartedAt: time.Now()})
	defer s.setActive(item.ID, nil)

	// Read, not taken. An answer is spent when the run it was written for
	// actually ran — including one that stops to ask something else, which is a
	// new question and wants a new answer. A run that failed outright never got
	// to use it, and a paragraph a person typed is not something to lose to a
	// bad agent invocation: the answer is kept and the session dropped, because
	// a resume that did not work names a conversation worth abandoning.
	resume := s.answers.Get(item.ID)
	tr, _ := s.eng.Advance(ctx, item.Source, &item, target, resume)
	if tr != nil {
		switch tr.Outcome {
		case model.OutcomeFailure, model.OutcomeTimeout:
			if resume.Answer != "" && resume.Session != "" {
				_ = s.answers.Set(item.ID, model.Resume{Answer: resume.Answer})
			}
		default:
			s.answers.Take(item.ID)
		}
		s.applyTransition(tr)
		s.hub.publish(event{Kind: "transition", Transition: tr})
	}
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
	// A no-op in the stage the item was already in is the script saying it has
	// nothing to do yet — a pull request still settling, a check still running.
	// Nothing about the board will answer it differently one second later, so
	// it waits for the listing. Any other outcome is progress, and progress
	// ends the deferral: the item is somewhere new, or marked, or due a retry.
	if tr.Outcome == model.OutcomeNoop && tr.From == tr.Stage {
		s.resting[tr.Item.ID] = true
	} else {
		delete(s.resting, tr.Item.ID)
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
	if !s.eng.Locks().TryAcquire(item.Source, target) {
		return false
	}
	defer s.eng.Locks().Release(item.Source, target)
	s.working.Store(item.ID, struct{}{})
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

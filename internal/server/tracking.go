package server

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/source"
)

const (
	trackingKind = "tracking"
	noOutputKind = "no-output"
	statusKind   = "status"
)

// reconcileTracking applies the non-work lifecycle after a fresh listing.
// Evaluation belongs to pipeline and uses the full listing; this function owns
// only the narrow provider reconciliation policy around its result.
func (s *Server) reconcileTracking(ctx context.Context) int {
	if s.mode != ModeAuto {
		return 0
	}
	s.mu.RLock()
	items := append([]model.Item(nil), s.state.Items...)
	blocks := make(map[string]Block, len(s.blocks))
	for id, block := range s.blocks {
		blocks[id] = block
	}
	stale := make(map[string]bool, len(s.listErr))
	for name := range s.listErr {
		stale[name] = true
	}
	s.mu.RUnlock()

	deps := pipeline.NewDeps(s.cfg, items)
	changed := 0
	for i := range items {
		it := items[i]
		if stale[it.Source] {
			continue
		}
		if _, working := s.working.Load(it.ID); working {
			continue
		}
		block := blocks[it.ID]
		if !it.Tracking {
			// Removing the explicit opt-in is one of the repairs named by an
			// empty-child mark. Do not make the operator clear a second field.
			if it.Blocked && !block.Asked && block.Kind == trackingKind {
				if err := s.unblockKind(ctx, it, trackingKind); err != nil {
					fmt.Fprintf(os.Stderr, "conveyor: could not clear retired tracking mark on %s: %v\n", it.ID, err)
					continue
				}
				changed++
			}
			continue
		}
		lifecycle := deps.Track(&it)
		if lifecycle.State == pipeline.TrackingInvalid {
			// A provider-native terminal status remains authoritative. The
			// computed lifecycle stays visible in /api/state, but writing a mark
			// onto a closed item would be stripped by the next listing forever.
			if stage, ok := s.cfg.Stage(it.Stage); ok && stage.Terminal {
				continue
			}
			if it.Blocked {
				if block.Asked || (block.Kind != trackingKind && block.Kind != noOutputKind) {
					continue
				}
				if block.Kind == trackingKind && block.Reason == lifecycle.Reason {
					continue
				}
			}
			wrote, err := s.writeTrackingMark(ctx, it, lifecycle.Reason)
			if err != nil {
				fmt.Fprintf(os.Stderr, "conveyor: could not reconcile tracking mark on %s: %v\n", it.ID, err)
				continue
			}
			if wrote {
				changed++
			}
			continue
		}

		if lifecycle.State == pipeline.TrackingSettled && it.Blocked && !block.Asked && block.Kind == statusKind {
			if err := s.finishSettledTracking(ctx, it); err != nil {
				fmt.Fprintf(os.Stderr, "conveyor: could not finish settled tracking item %s: %v\n", it.ID, err)
				continue
			}
			changed++
			continue
		}

		if it.Blocked && !block.Asked && (block.Kind == trackingKind || block.Kind == noOutputKind) {
			if err := s.unblockKind(ctx, it, block.Kind); err != nil {
				fmt.Fprintf(os.Stderr, "conveyor: could not clear repaired tracking mark on %s: %v\n", it.ID, err)
				continue
			}
			changed++
		}
	}
	return changed
}

// finishSettledTracking handles the recovery shape where GitHub reports an
// explicit tracker open while it already wears a terminal stage label. Generic
// unblocking carries no completion proof; only this full-list recheck may close
// the native issue and clear its status contradiction together.
func (s *Server) finishSettledTracking(ctx context.Context, item model.Item) error {
	if _, busy := s.unblocking.LoadOrStore(item.ID, struct{}{}); busy {
		return fmt.Errorf("a tracking completion write is already in progress")
	}
	defer s.unblocking.Delete(item.ID)

	s.mu.RLock()
	current, found := model.Item{}, false
	items := append([]model.Item(nil), s.state.Items...)
	for _, it := range s.state.Items {
		if it.ID == item.ID {
			current, found = it, true
			break
		}
	}
	block := s.blocks[item.ID]
	stale := s.listErr[current.Source] != ""
	s.mu.RUnlock()
	if !found || !current.Tracking || !current.Blocked || block.Asked || block.Kind != statusKind {
		return fmt.Errorf("%s is no longer a settled tracker with a status mark", item.ID)
	}
	if stale {
		return fmt.Errorf("%s has a stale source listing", item.ID)
	}
	if _, working := s.working.Load(item.ID); working {
		return fmt.Errorf("%s is currently moving", item.ID)
	}
	tracked := pipeline.NewDeps(s.cfg, items).Track(&current)
	if tracked.State != pipeline.TrackingSettled {
		return fmt.Errorf("%s tracking state changed before completion", item.ID)
	}
	for _, childID := range current.Children {
		if _, working := s.working.Load(childID); working {
			return fmt.Errorf("%s child %s is currently moving", item.ID, childID)
		}
	}
	client, ok := s.eng.Client(current.Source)
	if !ok {
		return fmt.Errorf("%s: no provider client", current.Source)
	}
	if _, err := client.CompleteTracking(ctx, &current, current.Stage, source.Mark{}); err != nil {
		return err
	}
	now := time.Now()
	s.mu.Lock()
	for i := range s.state.Items {
		if s.state.Items[i].ID == current.ID {
			s.state.Items[i] = current
			break
		}
	}
	delete(s.blocks, current.ID)
	s.confirmedAt[current.ID] = now
	delete(s.resting, current.ID)
	delete(s.restingAt, current.ID)
	delete(s.waiting, current.ID)
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
	s.wakeUp()
	return nil
}

// writeTrackingMark sets or updates one lifecycle-owned mark. It shares the
// per-item provider-write claim with every clearing path so an older lifecycle
// snapshot cannot overwrite a question or fault that arrived meanwhile.
func (s *Server) writeTrackingMark(ctx context.Context, item model.Item, reason string) (bool, error) {
	if _, busy := s.unblocking.LoadOrStore(item.ID, struct{}{}); busy {
		return false, fmt.Errorf("a mark reconciliation write is already in progress")
	}
	defer s.unblocking.Delete(item.ID)

	s.mu.RLock()
	current, found := model.Item{}, false
	items := append([]model.Item(nil), s.state.Items...)
	for _, it := range s.state.Items {
		if it.ID == item.ID {
			current, found = it, true
			break
		}
	}
	block := s.blocks[item.ID]
	stale := s.listErr[current.Source] != ""
	s.mu.RUnlock()
	if !found || !current.Tracking {
		return false, fmt.Errorf("%s is no longer an explicit tracking item", item.ID)
	}
	if stale {
		return false, fmt.Errorf("%s has a stale source listing", item.ID)
	}
	latest := pipeline.NewDeps(s.cfg, items).Track(&current)
	if latest.State != pipeline.TrackingInvalid || latest.Reason != reason {
		return false, fmt.Errorf("%s tracking state changed before its mark could be written", item.ID)
	}
	for _, childID := range current.Children {
		if _, working := s.working.Load(childID); working {
			return false, fmt.Errorf("%s child %s is currently moving", item.ID, childID)
		}
	}
	if current.Blocked {
		if block.Asked || (block.Kind != trackingKind && block.Kind != noOutputKind) {
			return false, fmt.Errorf("%s now carries unrelated mark %s", item.ID, block.Kind)
		}
		if block.Kind == trackingKind && block.Reason == reason {
			return false, nil
		}
	}
	item = current
	client, ok := s.eng.Client(item.Source)
	if !ok {
		return false, fmt.Errorf("%s: no provider client", item.Source)
	}
	mark := source.Mark{Blocked: true, Kind: trackingKind, Reason: reason}
	if _, err := client.Move(ctx, &item, item.Stage, mark); err != nil {
		return false, err
	}
	now := time.Now()
	s.mu.Lock()
	for i := range s.state.Items {
		if s.state.Items[i].ID == item.ID {
			s.state.Items[i] = item
			break
		}
	}
	s.blocks[item.ID] = Block{Kind: trackingKind, Reason: reason, Stage: item.Stage, At: now}
	s.confirmedAt[item.ID] = now
	delete(s.resting, item.ID)
	delete(s.restingAt, item.ID)
	delete(s.waiting, item.ID)
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
	return true, nil
}

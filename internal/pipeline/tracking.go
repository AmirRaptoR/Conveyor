package pipeline

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// TrackingState is the deterministic lifecycle of an explicit tracking item.
type TrackingState string

const (
	TrackingInvalid  TrackingState = "invalid"
	TrackingPartial  TrackingState = "partial"
	TrackingComplete TrackingState = "complete"
	TrackingSettled  TrackingState = "settled"
)

// TrackingResult is everything callers need from tracking evaluation. Reason
// is set only for invalid state; Target is set only when all children are
// terminal and the tracker itself still needs its one provider-only move.
type TrackingResult struct {
	State  TrackingState `json:"state"`
	Reason string        `json:"reason,omitempty"`
	Target string        `json:"target,omitempty"`
}

// Track reports the lifecycle computed when this Deps was built from the full
// listing. The zero Deps deliberately knows nothing and therefore authorizes no
// tracker transition.
func (d Deps) Track(it *model.Item) TrackingResult {
	if it == nil || !it.Tracking || d.tracking == nil {
		return TrackingResult{}
	}
	return d.tracking[it.ID]
}

func evaluateTracking(cfg *config.Config, items []model.Item) map[string]TrackingResult {
	out := map[string]TrackingResult{}
	listed := make(map[string]model.Item, len(items))
	terminal := map[string]bool{}
	firstTerminal := ""
	if cfg != nil {
		for _, st := range cfg.Stages {
			terminal[st.Name] = st.Terminal
			if st.Terminal && firstTerminal == "" {
				firstTerminal = st.Name
			}
		}
	}
	for _, it := range items {
		listed[it.ID] = it
	}

	for _, it := range items {
		if !it.Tracking {
			continue
		}
		invalid := func(reason string) {
			out[it.ID] = TrackingResult{State: TrackingInvalid, Reason: reason}
		}
		if it.TrackingError != "" {
			invalid(fmt.Sprintf("tracking item %s has an incomplete child set: %s", it.ID, it.TrackingError))
			continue
		}
		if len(it.Children) == 0 {
			invalid(fmt.Sprintf("tracking item %s declares no children; add at least one required child or remove tracking", it.ID))
			continue
		}
		if cycle := trackingParentCycle(it.ID, listed); len(cycle) > 0 {
			invalid(fmt.Sprintf("tracking item %s is in a parent cycle: %s; remove the cycle", it.ID, strings.Join(cycle, " -> ")))
			continue
		}

		children := append([]string(nil), it.Children...)
		sort.Strings(children)
		unfinishedID, unfinishedStage := "", ""
		bad := false
		seen := map[string]bool{}
		for _, childID := range children {
			if seen[childID] {
				invalid(fmt.Sprintf("tracking item %s names child %s more than once; remove the duplicate", it.ID, childID))
				bad = true
				break
			}
			seen[childID] = true
			switch {
			case childID == it.ID:
				invalid(fmt.Sprintf("tracking item %s names itself as child %s; remove the self-reference", it.ID, childID))
				bad = true
			case relationSource(childID) == "":
				invalid(fmt.Sprintf("tracking item %s has unqualified child id %q; use <source>:<ref>", it.ID, childID))
				bad = true
			case relationSource(childID) != "" && relationSource(childID) != it.Source:
				invalid(fmt.Sprintf("tracking item %s has cross-source child %s (%s -> %s); tracking children must use the same source", it.ID, childID, it.Source, relationSource(childID)))
				bad = true
			default:
				child, found := listed[childID]
				switch {
				case !found:
					invalid(fmt.Sprintf("tracking item %s declares missing child %s; onboard or correct that child", it.ID, childID))
					bad = true
				case child.Source != it.Source:
					invalid(fmt.Sprintf("tracking item %s has cross-source child %s (%s -> %s); tracking children must use the same source", it.ID, childID, it.Source, child.Source))
					bad = true
				case child.Parent != it.ID:
					if child.Parent == "" {
						invalid(fmt.Sprintf("tracking item %s names child %s, but %s names no parent; set its parent to %s", it.ID, childID, childID, it.ID))
					} else {
						invalid(fmt.Sprintf("tracking item %s names child %s, but %s names parent %s; make the relationship reciprocal", it.ID, childID, childID, child.Parent))
					}
					bad = true
				case cfg == nil:
					invalid(fmt.Sprintf("tracking item %s cannot validate child %s without a stage configuration", it.ID, childID))
					bad = true
				default:
					if _, known := cfg.Stage(child.Stage); !known {
						invalid(fmt.Sprintf("tracking item %s has child %s whose stage %q is not declared; repair the provider stage", it.ID, childID, child.Stage))
						bad = true
					} else if terminal[child.Stage] && child.Blocked {
						invalid(fmt.Sprintf("tracking item %s has child %s marked in terminal stage %s; reconcile the child's native status before completing the tracker", it.ID, childID, child.Stage))
						bad = true
					} else if !terminal[child.Stage] && unfinishedID == "" {
						unfinishedID, unfinishedStage = childID, child.Stage
					}
				}
			}
			if bad {
				break // sorted child ids make the first error stable
			}
		}
		if bad {
			continue
		}
		if terminal[it.Stage] && unfinishedID != "" {
			invalid(fmt.Sprintf("tracking item %s is terminal in %s while child %s is unfinished in %s; reopen or move the tracker back, or finish the child", it.ID, it.Stage, unfinishedID, unfinishedStage))
			continue
		}
		if unfinishedID != "" {
			out[it.ID] = TrackingResult{State: TrackingPartial}
			continue
		}
		if terminal[it.Stage] {
			out[it.ID] = TrackingResult{State: TrackingSettled}
			continue
		}
		if firstTerminal == "" {
			invalid(fmt.Sprintf("tracking item %s is complete but the pipeline declares no terminal stage", it.ID))
			continue
		}
		out[it.ID] = TrackingResult{State: TrackingComplete, Target: firstTerminal}
	}
	return out
}

func trackingParentCycle(id string, listed map[string]model.Item) []string {
	path, positions := []string{}, map[string]int{}
	for current := id; current != ""; {
		if at, found := positions[current]; found {
			return canonicalCycle(path[at:])
		}
		positions[current] = len(path)
		path = append(path, current)
		next, found := listed[current]
		if !found {
			return nil
		}
		current = next.Parent
	}
	return nil
}

func relationSource(id string) string {
	source, _, ok := strings.Cut(id, ":")
	if !ok {
		return ""
	}
	return source
}

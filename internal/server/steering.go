package server

import (
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/steering"
)

// SteeringCommandView is one command with its disk-derived resolution.
type SteeringCommandView struct {
	ID     string    `json:"id"`
	Seq    int       `json:"seq"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	State  string    `json:"state"`
	Reason string    `json:"reason"`
}

// RunSteeringView is the full per-run representation used by run detail and
// SSE. SteeringSummary is embedded so snapshot and event cursors cannot drift.
type RunSteeringView struct {
	SteeringSummary
	Commands []SteeringCommandView `json:"commands"`
}

func steeringView(runID, stage string, res steering.Resolution, live bool) RunSteeringView {
	v := RunSteeringView{SteeringSummary: SteeringSummary{
		RunID: runID, Stage: stage, Session: res.Session,
		Accepts: append([]string{}, res.Accepts...), Malformed: res.Malformed,
		MaxSeq: res.MaxSeq, AckSeq: res.AckSeq, Version: res.MaxSeq + res.AckVersion,
		Live: live,
	}}
	v.Commands = make([]SteeringCommandView, 0, len(res.Commands))
	for _, state := range res.Commands {
		cmd := state.Command
		v.Commands = append(v.Commands, SteeringCommandView{
			ID: cmd.ID, Seq: cmd.Seq, Kind: cmd.Kind, Text: cmd.Text,
			At: cmd.At, By: cmd.By, State: state.State, Reason: state.Reason,
		})
		switch state.State {
		case steering.StateQueued:
			v.Queued++
		case steering.StateConsumed:
			v.Consumed++
		case steering.StateRejected:
			v.Rejected++
		case steering.StateCarried:
			v.Carried++
		case steering.StateDropped:
			v.Dropped++
		}
	}
	return v
}

func hasSteering(res steering.Resolution) bool {
	return res.Hello || len(res.Commands) > 0
}

func (s *Server) handleSteeringUpdate(runID, itemID string, u runner.SteeringUpdate) {
	live := !u.Final
	if lease, ok := s.liveRuns.Acquire(itemID, runID); ok {
		lease.SetSession(u.Resolution.Session)
		lease.Release()
	} else {
		live = false
	}
	v := steeringView(runID, u.Stage, u.Resolution, live)
	if u.Kind == "stage" && u.Stage != "" && itemID != "" {
		s.mu.Lock()
		s.steeringGen[itemID]++
		if hasSteering(u.Resolution) {
			s.steering[itemID] = v.SteeringSummary
			delete(s.steeringMisses, itemID)
		} else {
			delete(s.steering, itemID)
			s.steeringMisses[itemID] = planCursor{Stage: u.Stage, RunID: runID}
		}
		s.mu.Unlock()
	}
	if u.Resolution.AckVersion > 0 || hasSteering(u.Resolution) || u.Resolution.Malformed > 0 {
		s.hub.publish(event{Kind: "steering", RunID: runID, ItemID: itemID, Steering: &v})
	}
}

func (s *Server) mergeRecoveredSteering(runStoreGen uint64, generations map[string]uint64, stages map[string]string, found map[string]SteeringSummary, foundRuns map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runStoreGen != runStoreGen {
		return
	}
	for id, generation := range generations {
		if s.steeringGen[id] != generation {
			continue
		}
		delete(s.steering, id)
		delete(s.steeringMisses, id)
		if v, ok := found[id]; ok {
			s.steering[id] = v
		} else {
			s.steeringMisses[id] = planCursor{Stage: stages[id], RunID: foundRuns[id]}
		}
		s.steeringGen[id]++
	}
}

func effectiveStages(items []model.Item, active []Active) map[string]string {
	stages := make(map[string]string, len(items))
	for _, it := range items {
		stages[it.ID] = it.Stage
	}
	for _, a := range active {
		stages[a.ItemID] = a.Stage
	}
	return stages
}

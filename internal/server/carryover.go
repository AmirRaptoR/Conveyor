package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/steering"
)

const carriedSessionPrefix = "opencode:"

// CarryOverInterrupted arms instructions that the newest stage run for each
// item did not consume before serve stopped. It is called once by cmdServe,
// after New has opened Answers and before Run starts any work.
func (s *Server) CarryOverInterrupted() error {
	s.runStoreMu.Lock()
	defer s.runStoreMu.Unlock()

	seen := map[string]bool{}
	var errs []error
	s.walkRunsUnlocked(func(m RunMeta) bool {
		if m.Kind != "stage" || m.ItemID == "" || seen[m.ItemID] {
			return true
		}
		seen[m.ItemID] = true
		if m.Outcome != model.OutcomeInterrupted {
			return true
		}
		if err := s.carryOverRun(m); err != nil {
			errs = append(errs, fmt.Errorf("carry over run %s: %w", m.ID, err))
		}
		return true
	})
	return errors.Join(errs...)
}

func (s *Server) carryOverRun(run RunMeta) error {
	res := steering.Load(run.Dir, false)
	entries := make([]steering.CarriedEntry, 0, len(res.Commands))
	var instructions []string
	for _, state := range res.Commands {
		if state.State != steering.StateQueued {
			continue
		}
		switch state.Command.Kind {
		case steering.KindInstruction:
			instructions = append(instructions, state.Command.Text)
			entries = append(entries, steering.CarriedEntry{ID: state.Command.ID, State: steering.StateCarried})
		case steering.KindPause:
			entries = append(entries, steering.CarriedEntry{
				ID: state.Command.ID, State: steering.StateDropped,
				Reason: "the process it would have stopped is already gone",
			})
		}
	}
	if len(entries) == 0 {
		return nil
	}

	if len(instructions) > 0 {
		block := "\n\n--- carried from the interrupted run " + run.ID + " ---\n" + strings.Join(instructions, "\n\n")
		armed := s.answers.Get(run.ItemID)
		reason := "carried into the next run's armed answer"
		if strings.Contains(armed.Answer, block) {
			reason = "already present in the next run's armed answer"
		} else {
			armed.Answer += block
			if res.Session != "" {
				carriedSession := carriedSessionPrefix + res.Session
				switch {
				case armed.Session == "":
					armed.Session = carriedSession
				case armed.Session != carriedSession:
					reason += "; session conflict: kept the existing armed-answer session"
				}
			}
			if err := s.answers.Set(run.ItemID, armed); err != nil {
				return err
			}
		}
		for i := range entries {
			if entries[i].State == steering.StateCarried {
				entries[i].Reason = reason
			}
		}
	}

	_, err := steering.AppendCarried(filepath.Join(run.Dir, "control.jsonl"), time.Now(), entries)
	return err
}

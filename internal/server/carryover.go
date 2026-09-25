package server

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/steering"
)

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
	var instructions, alreadyCarried []string
	for _, state := range res.Commands {
		if state.State == steering.StateCarried && state.Command.Kind == steering.KindInstruction {
			alreadyCarried = append(alreadyCarried, state.Command.Text)
		}
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
		if len(alreadyCarried) > 0 {
			return s.bindLegacyCarryOver(run, res.Session, alreadyCarried)
		}
		return nil
	}

	if len(instructions) > 0 {
		block := "\n\n--- carried from the interrupted run " + run.ID + " ---\n" + strings.Join(instructions, "\n\n")
		armed := s.answers.Get(run.ItemID)
		reason := "carried into the next run's armed answer"
		binding, bindErr := runScriptBinding(run)
		bindingConflict := bindErr != nil || (armed.Stage != "" && armed.Stage != run.To) ||
			(armed.Script != "" && armed.Script != binding)
		if bindingConflict {
			reason = "dropped because the armed input belongs to a different stage or script"
			if bindErr != nil {
				reason = "dropped because the interrupted run's script identity is unavailable"
			}
		} else if strings.Contains(armed.Answer, block) {
			reason = "already present in the next run's armed answer"
			changed := false
			if armed.Stage == "" {
				armed.Stage, changed = run.To, true
			}
			if armed.Script == "" {
				armed.Script, changed = binding, true
			}
			if armed.Session == "" && res.Session != "" {
				armed.Session, changed = res.Session, true
			}
			if changed {
				if err := s.answers.Set(run.ItemID, armed); err != nil {
					return err
				}
			}
		} else {
			armed.Answer += block
			armed.Stage = run.To
			armed.Script = binding
			if res.Session != "" {
				switch {
				case armed.Session == "":
					armed.Session = res.Session
				case armed.Session != res.Session:
					reason += "; session conflict: kept the existing armed-answer session"
				}
			}
			if err := s.answers.Set(run.ItemID, armed); err != nil {
				return err
			}
		}
		for i := range entries {
			if entries[i].State == steering.StateCarried {
				if bindingConflict {
					entries[i].State = steering.StateDropped
				}
				entries[i].Reason = reason
			}
		}
	}

	_, err := steering.AppendCarried(filepath.Join(run.Dir, "control.jsonl"), time.Now(), entries)
	return err
}

func (s *Server) bindLegacyCarryOver(run RunMeta, session string, instructions []string) error {
	block := "\n\n--- carried from the interrupted run " + run.ID + " ---\n" + strings.Join(instructions, "\n\n")
	armed := s.answers.Get(run.ItemID)
	if !strings.Contains(armed.Answer, block) || (armed.Stage != "" && armed.Stage != run.To) {
		return nil
	}
	binding, err := runScriptBinding(run)
	if err != nil || (armed.Script != "" && armed.Script != binding) {
		return nil
	}
	changed := false
	if armed.Stage == "" {
		armed.Stage, changed = run.To, true
	}
	if armed.Script == "" {
		armed.Script, changed = binding, true
	}
	if armed.Session == "" && session != "" {
		armed.Session, changed = session, true
	}
	if changed {
		return s.answers.Set(run.ItemID, armed)
	}
	return nil
}

func runScriptBinding(run RunMeta) (string, error) {
	if run.Script == "" {
		return "", errors.New("run recorded no script")
	}
	if filepath.Clean(filepath.Dir(run.Script)) == filepath.Clean(run.Dir) {
		body, err := os.ReadFile(run.Script)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("inline:%x", sha256.Sum256(body)), nil
	}
	return "path:" + stableReleasePath(run.Script), nil
}

func (s *Server) targetScriptBinding(sourceName, stageName string) string {
	stage, ok := s.cfg.Stage(stageName)
	if !ok || !stage.Runs() {
		return ""
	}
	if stage.Run != "" {
		return fmt.Sprintf("inline:%x", sha256.Sum256([]byte(stage.Run)))
	}
	source, ok := s.cfg.Source(sourceName)
	if !ok {
		return ""
	}
	return "path:" + stableReleasePath(source.Paths[stage.Script])
}

func stableReleasePath(path string) string {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "releases" {
			parts[i+1] = "*"
			break
		}
	}
	return filepath.Join(parts...)
}

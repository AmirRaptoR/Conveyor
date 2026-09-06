// Package server serves the pipeline view: the board, run history and live
// logs. It observes by default — ticking is what moves items, and that opens
// pull requests on real repositories, so it happens only when asked.
package server

import (
	"embed"
	"fmt"
	"time"
)

//go:embed web
var webFS embed.FS

// whyPaused names the agent and the reset it reported, for a manual start
// refused because the stage's agent is over quota — the policy launch already
// applies by silently skipping the item.
func (s *Server) whyPaused(agent string) string {
	s.mu.RLock()
	p, held := s.paused[agent]
	s.mu.RUnlock()
	if !held {
		return fmt.Sprintf("%s is paused", agent)
	}
	if p.Until.IsZero() {
		return fmt.Sprintf("%s is paused: %s", agent, p.Reason)
	}
	return fmt.Sprintf("%s is paused until %s: %s", agent, p.Until.Format(time.RFC3339), p.Reason)
}

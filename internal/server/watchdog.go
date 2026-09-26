package server

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
)

type WatchdogFinding struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type WatchdogView struct {
	EvaluatedAt    time.Time         `json:"evaluatedAt"`
	LastProgressAt time.Time         `json:"lastProgressAt"`
	StallWindow    time.Duration     `json:"stallWindowNs"`
	Unfinished     int               `json:"unfinished"`
	Runnable       int               `json:"runnable"`
	Active         int               `json:"active"`
	Incident       bool              `json:"incident"`
	AlertedAt      time.Time         `json:"alertedAt,omitempty"`
	Findings       []WatchdogFinding `json:"findings,omitempty"`
	Alert          bool              `json:"-"`
}

func hasFinding(findings []WatchdogFinding, kind string) bool {
	for _, finding := range findings {
		if finding.Kind == kind {
			return true
		}
	}
	return false
}

func (s *Server) evaluateWatchdog(now time.Time) WatchdogView {
	s.watchdogMu.Lock()
	defer s.watchdogMu.Unlock()
	s.mu.RLock()
	items := append([]model.Item(nil), s.state.Items...)
	listed := make(map[string]time.Time, len(s.listedAt))
	for name, at := range s.listedAt {
		listed[name] = at
	}
	listErr := make(map[string]string, len(s.listErr))
	for name, err := range s.listErr {
		listErr[name] = err
	}
	storage := s.state.Storage
	rel := s.state.Release
	resting := make(map[string]bool, len(s.resting))
	for id := range s.resting {
		resting[id] = true
	}
	paused := make(map[string]bool, len(s.paused))
	for agent, pause := range s.paused {
		paused[agent] = pause.Live(now)
	}
	s.mu.RUnlock()

	state := s.watchdogState
	if state.LastProgressAt.IsZero() {
		state.LastProgressAt = now
	}
	view := WatchdogView{EvaluatedAt: now, LastProgressAt: state.LastProgressAt,
		StallWindow: s.cfg.Watchdog.StallWindow.D(), Active: len(s.activeList())}
	for _, item := range items {
		if stage, ok := s.cfg.Stage(item.Stage); ok && !stage.Terminal {
			view.Unfinished++
		}
	}

	deps := pipeline.NewDeps(s.cfg, items)
	staleSrc := map[string]bool{}
	for _, source := range s.cfg.Sources {
		at, listedOK := listed[source.Name]
		if !listedOK || now.Sub(at) > s.cfg.Watchdog.StallWindow.D() || listErr[source.Name] != "" {
			staleSrc[source.Name] = true
			detail := "never listed successfully"
			if listedOK {
				detail = "last successful listing " + at.UTC().Format(time.RFC3339)
			}
			if listErr[source.Name] != "" {
				detail += ": " + listErr[source.Name]
			}
			view.Findings = append(view.Findings, WatchdogFinding{Kind: "source-stale", Detail: source.Name + ": " + detail})
		}
	}
	staleItems := staleItemIDs(items, staleSrc)
	for _, item := range items {
		target, ok := pipeline.Target(s.cfg, &item, deps)
		agent := s.cfg.AgentFor(item.Source, target)
		if !ok || dispatchUsesStaleState(item, deps, staleSrc, staleItems) || resting[item.ID] || s.manuallyPaused(item.Source) ||
			paused[agent] || !s.budgetAvailable(item.ID, item.Source, target) ||
			(agent != "" && storage.Level != "" && storage.Level != "ok") ||
			s.eng.Locks().Busy(item.Source, target, s.cfg.ResourcesFor(item.Source, target)...) {
			continue
		}
		if _, busy := s.working.Load(item.ID); !busy {
			view.Runnable++
		}
	}
	if view.Unfinished > 0 && view.Runnable > 0 && view.Active == 0 && now.Sub(state.LastProgressAt) >= view.StallWindow {
		view.Findings = append(view.Findings, WatchdogFinding{Kind: "dead-scheduler", Detail: fmt.Sprintf("%d item(s) runnable with no active transition or useful progress for %s", view.Runnable, view.StallWindow)})
		view.Incident = true
	}
	if storage.Level == "high" || storage.Level == "critical" {
		view.Findings = append(view.Findings, WatchdogFinding{Kind: "storage-headroom", Detail: "storage level is " + storage.Level})
	}
	if rel.Managed && (filepath.Base(rel.Dir) != rel.Revision || rel.ConfigSchema != s.cfg.Version) {
		view.Findings = append(view.Findings, WatchdogFinding{Kind: "revision-coherence", Detail: fmt.Sprintf("release revision %q does not match directory/config identity", rel.Revision)})
	}
	metrics := s.metrics(now)
	if metrics.StageRuns >= 3 && metrics.SuccessRate < .25 {
		view.Findings = append(view.Findings, WatchdogFinding{Kind: "completion-rate", Detail: fmt.Sprintf("seven-day stage success rate is %.1f%%", metrics.SuccessRate*100)})
	}
	blocked := map[string]int{}
	auditedRuns := map[string]bool{}
	for _, event := range s.audit.Since(now.Add(-metricsWindow)) {
		if event.Kind == "run" && event.Outcome == string(model.OutcomeBlocked) {
			blocked[event.ItemID+"\x00"+event.BlockKind]++
		}
		if event.Kind == "run" {
			auditedRuns[event.RunID] = true
		}
	}
	s.walkRuns(func(run RunMeta) bool {
		if run.StartedAt.Before(now.Add(-metricsWindow)) {
			return false
		}
		if run.Kind == "stage" && run.Outcome == model.OutcomeBlocked && !auditedRuns[run.ID] {
			blocked[run.ItemID+"\x00"+run.MarkKind]++
		}
		return true
	})
	for key, count := range blocked {
		if count >= 3 {
			item, _, _ := strings.Cut(key, "\x00")
			view.Findings = append(view.Findings, WatchdogFinding{Kind: "repeated-blocker", Detail: fmt.Sprintf("%s blocked %d times in seven days", item, count)})
		}
	}
	sort.Slice(view.Findings, func(i, j int) bool {
		if view.Findings[i].Kind == view.Findings[j].Kind {
			return view.Findings[i].Detail < view.Findings[j].Detail
		}
		return view.Findings[i].Kind < view.Findings[j].Kind
	})
	key := ""
	if view.Incident {
		h := sha256.Sum256([]byte(fmt.Sprint(view.Unfinished, ":", view.Runnable, ":", state.LastProgressAt.Unix())))
		key = fmt.Sprintf("%x", h[:8])
		if state.IncidentKey != key {
			view.Alert = true
			state.IncidentKey, state.AlertedAt = key, now
		}
	} else {
		state.IncidentKey, state.AlertedAt = "", time.Time{}
	}
	view.AlertedAt = state.AlertedAt
	s.watchdogState = state
	if err := s.watchdogStore.Set(state); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: persist watchdog state: %v\n", err)
	}
	return view
}

func (s *Server) watchdog(ctxDone <-chan struct{}) {
	run := func() {
		view := s.evaluateWatchdog(time.Now())
		metrics := s.metrics(time.Now())
		s.mu.Lock()
		s.state.Watchdog = view
		s.state.Metrics = metrics
		s.mu.Unlock()
		s.hub.publish(event{Kind: "state"})
		if view.Alert {
			body := "unfinished runnable work made no useful progress"
			for _, finding := range view.Findings {
				if finding.Kind == "dead-scheduler" {
					body = finding.Detail
					break
				}
			}
			s.notify("Conveyor stalled", body, "watchdog-stall")
		}
	}
	run()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctxDone:
			return
		case <-ticker.C:
			run()
		}
	}
}

func (s *Server) noteUsefulProgress(at time.Time) {
	s.watchdogMu.Lock()
	defer s.watchdogMu.Unlock()
	state := s.watchdogState
	state.LastProgressAt, state.IncidentKey, state.AlertedAt = at, "", time.Time{}
	s.watchdogState = state
	if err := s.watchdogStore.Set(state); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: persist watchdog progress: %v\n", err)
	}
}

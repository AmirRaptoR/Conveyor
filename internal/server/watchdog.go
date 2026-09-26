package server

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/push"
)

type WatchdogFinding struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type WatchdogView struct {
	EvaluatedAt          time.Time         `json:"evaluatedAt"`
	LastProgressAt       time.Time         `json:"lastProgressAt"`
	StallWindow          time.Duration     `json:"stallWindowNs"`
	Unfinished           int               `json:"unfinished"`
	Potential            int               `json:"potential"`
	Runnable             int               `json:"runnable"`
	Active               int               `json:"active"`
	Incident             bool              `json:"incident"`
	DetectedAt           time.Time         `json:"detectedAt,omitempty"`
	DeliveryStatus       string            `json:"deliveryStatus,omitempty"`
	DeliveryAttemptedAt  time.Time         `json:"deliveryAttemptedAt,omitempty"`
	DeliveredAt          time.Time         `json:"deliveredAt,omitempty"`
	DeliveryError        string            `json:"deliveryError,omitempty"`
	DeliveryAcknowledged int               `json:"deliveryAcknowledged"`
	EvidenceHealthy      bool              `json:"evidenceHealthy"`
	EvidenceError        string            `json:"evidenceError,omitempty"`
	Findings             []WatchdogFinding `json:"findings,omitempty"`
	Alert                bool              `json:"-"`
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
	persistFault := s.state.PersistFault != nil
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
	evidence := s.watchdogStore.Status()
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
	freshFor := 2 * s.cfg.Poll.D()
	if freshFor < time.Minute {
		freshFor = time.Minute
	}
	for _, source := range s.cfg.Sources {
		at, listedOK := listed[source.Name]
		if !listedOK || now.Sub(at) > freshFor || listErr[source.Name] != "" {
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
	stalePotential := 0
	for _, item := range items {
		target, ok := pipeline.Target(s.cfg, &item, deps)
		if ok {
			view.Potential++
		}
		agent := s.cfg.AgentFor(item.Source, target)
		stale := dispatchUsesStaleState(item, deps, staleSrc, staleItems)
		if ok && stale {
			stalePotential++
		}
		_, itemBusy := s.working.Load(item.ID)
		reason := admissionReason(admissionFacts{targetOK: ok, itemBusy: itemBusy, resting: resting[item.ID], stale: stale,
			manualPaused: s.manuallyPaused(item.Source), agentPaused: paused[agent],
			budgetBlocked:  !s.budgetAvailable(item.ID, item.Source, target),
			storageBlocked: agent != "" && storage.Level != "" && storage.Level != "ok",
			slotBlocked:    s.eng.Locks().Busy(item.Source, target, s.cfg.ResourcesFor(item.Source, target)...),
			persistFault:   agent != "" && persistFault})
		if reason != "" {
			continue
		}
		view.Runnable++
	}
	_, _, held, _, _, _ := s.eng.Locks().Snapshot()
	if view.Potential > 0 && view.Active == 0 && now.Sub(state.LastProgressAt) >= view.StallWindow {
		if held > 0 {
			view.Findings = append(view.Findings, WatchdogFinding{Kind: "capacity-leak", Detail: fmt.Sprintf("%d global slot(s) held with no active transition for %s", held, view.StallWindow)})
			view.Incident = true
		}
		if stalePotential > 0 {
			view.Findings = append(view.Findings, WatchdogFinding{Kind: "stale-source-stall", Detail: fmt.Sprintf("%d potential item(s) unavailable because provider evidence is stale for %s", stalePotential, view.StallWindow)})
			view.Incident = true
		}
		if view.Runnable > 0 {
			view.Findings = append(view.Findings, WatchdogFinding{Kind: "dead-scheduler", Detail: fmt.Sprintf("%d item(s) runnable with no active transition or useful progress for %s", view.Runnable, view.StallWindow)})
			view.Incident = true
		}
	}
	if storage.Level == "high" || storage.Level == "critical" {
		view.Findings = append(view.Findings, WatchdogFinding{Kind: "storage-headroom", Detail: "storage level is " + storage.Level})
	}
	if !rel.Managed || filepath.Base(rel.Dir) != rel.Revision || rel.ConfigSchema != s.cfg.Version {
		view.Findings = append(view.Findings, WatchdogFinding{Kind: "revision-coherence", Detail: fmt.Sprintf("release revision %q does not match directory/config identity", rel.Revision)})
	}
	metrics := s.metricsAt(now)
	if metrics.StageRuns >= 3 && metrics.CompletionRate == 0 {
		view.Findings = append(view.Findings, WatchdogFinding{Kind: "completion-rate", Detail: fmt.Sprintf("seven-day completion rate is %.2f per day after %d settled stage runs", metrics.CompletionRate, metrics.StageRuns)})
	}
	if status := s.audit.Status(); !status.EvidenceComplete {
		detail := status.Error
		if detail == "" && !status.ReconciliationComplete {
			detail = "run audit reconciliation is incomplete"
		}
		if detail == "" && status.PendingHuman > 0 {
			detail = fmt.Sprintf("%d human audit intent(s) are unresolved", status.PendingHuman)
		}
		view.Findings = append(view.Findings, WatchdogFinding{Kind: "audit-evidence", Detail: detail})
		view.Incident = true
	}
	view.EvidenceHealthy, view.EvidenceError = evidence.Healthy, evidence.Error
	if !evidence.Healthy {
		view.Findings = append(view.Findings, WatchdogFinding{Kind: "watchdog-evidence", Detail: evidence.Error})
		view.Incident = true
	}
	blocked := map[string]int{}
	for _, event := range s.audit.Since(now.Add(-metricsWindow)) {
		if event.Kind == "run" && event.Outcome == string(model.OutcomeBlocked) {
			blocked[event.ItemID+"\x00"+event.BlockKind]++
		}
	}
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
		var causes []string
		for _, finding := range view.Findings {
			switch finding.Kind {
			case "dead-scheduler", "stale-source-stall", "capacity-leak", "audit-evidence", "watchdog-evidence":
				causes = append(causes, finding.Kind)
			}
		}
		h := sha256.Sum256([]byte(fmt.Sprint(state.LastProgressAt.Unix(), ":", strings.Join(causes, ","))))
		key = fmt.Sprintf("%x", h[:8])
		if state.IncidentKey != key {
			state.IncidentKey, state.DetectedAt = key, now
			state.DeliveryStatus, state.DeliveryAttemptedAt, state.DeliveredAt, state.DeliveryError = "pending", time.Time{}, time.Time{}, ""
			state.DeliveryAcks = nil
		}
		view.Alert = state.DeliveryStatus != "delivered"
	} else {
		state.IncidentKey, state.DetectedAt = "", time.Time{}
		state.DeliveryStatus, state.DeliveryAttemptedAt, state.DeliveredAt, state.DeliveryError = "", time.Time{}, time.Time{}, ""
		state.DeliveryAcks = nil
	}
	view.DetectedAt, view.DeliveryStatus = state.DetectedAt, state.DeliveryStatus
	view.DeliveryAttemptedAt, view.DeliveredAt, view.DeliveryError = state.DeliveryAttemptedAt, state.DeliveredAt, state.DeliveryError
	view.DeliveryAcknowledged = len(state.DeliveryAcks)
	s.watchdogState = state
	if err := s.watchdogStore.Set(state); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: persist watchdog state: %v\n", err)
		view.Alert = false
		view.DeliveryError = "watchdog incident is not durable: " + err.Error()
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
			s.spawn(func() { s.deliverWatchdogAlert(view) })
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

func (s *Server) deliverWatchdogAlert(view WatchdogView) {
	if !s.watchdogAlerting.CompareAndSwap(false, true) {
		return
	}
	defer s.watchdogAlerting.Store(false)
	s.watchdogMu.Lock()
	state := s.watchdogState
	if state.IncidentKey == "" || !state.DetectedAt.Equal(view.DetectedAt) || state.DeliveryStatus == "delivered" {
		s.watchdogMu.Unlock()
		return
	}
	state.DeliveryStatus = "attempted"
	state.DeliveryAttemptedAt = time.Now()
	state.DeliveryError = ""
	s.watchdogState = state
	if err := s.watchdogStore.Set(state); err != nil {
		state.DeliveryStatus = "pending"
		state.DeliveryError = "persist delivery attempt: " + err.Error()
		s.watchdogState = state
		fmt.Fprintf(os.Stderr, "conveyor: persist watchdog delivery attempt: %v\n", err)
		s.watchdogMu.Unlock()
		return
	}
	s.watchdogMu.Unlock()

	body := "Conveyor detected an operational stall"
	if len(view.Findings) > 0 {
		body = view.Findings[0].Detail
	}
	incidentKey, detectedAt := state.IncidentKey, state.DetectedAt
	endpoints := s.watchdogEndpoints()
	if len(endpoints) == 0 {
		s.finishWatchdogDelivery(incidentKey, detectedAt, nil, errors.New("no push subscriber"), false)
		return
	}
	var lastErr error
	for _, endpoint := range endpoints {
		s.watchdogMu.Lock()
		state = s.watchdogState
		already := state.DeliveryAcks[endpoint]
		current := state.IncidentKey == incidentKey && state.DetectedAt.Equal(detectedAt)
		s.watchdogMu.Unlock()
		if !current {
			return
		}
		if already {
			continue
		}
		err := s.watchdogNotifyEndpoint(endpoint, "Conveyor stalled", body, "watchdog-stall")
		accepted := err == nil || errors.Is(err, push.Gone)
		if !accepted {
			lastErr = err
		}
		if !s.finishWatchdogDelivery(incidentKey, detectedAt, []string{endpoint}, err, accepted) {
			return
		}
	}
	currentEndpoints := s.watchdogEndpoints()
	s.watchdogMu.Lock()
	state = s.watchdogState
	all := state.IncidentKey == incidentKey && state.DetectedAt.Equal(detectedAt)
	for _, endpoint := range currentEndpoints {
		all = all && state.DeliveryAcks[endpoint]
	}
	s.watchdogMu.Unlock()
	if all {
		s.finishWatchdogDelivery(incidentKey, detectedAt, nil, nil, true)
	} else if lastErr != nil {
		s.finishWatchdogDelivery(incidentKey, detectedAt, nil, lastErr, false)
	} else {
		s.finishWatchdogDelivery(incidentKey, detectedAt, nil, errors.New("not every current push subscriber acknowledged the incident"), false)
	}
}

func (s *Server) finishWatchdogDelivery(key string, detected time.Time, endpoints []string, deliveryErr error, accepted bool) bool {
	s.watchdogMu.Lock()
	defer s.watchdogMu.Unlock()
	state := s.watchdogState
	if state.IncidentKey != key || !state.DetectedAt.Equal(detected) {
		return false
	}
	if accepted && len(endpoints) > 0 {
		if state.DeliveryAcks == nil {
			state.DeliveryAcks = map[string]bool{}
		}
		for _, endpoint := range endpoints {
			state.DeliveryAcks[endpoint] = true
		}
	}
	if accepted && len(endpoints) == 0 {
		state.DeliveryStatus, state.DeliveredAt, state.DeliveryError = "delivered", time.Now(), ""
	} else if deliveryErr != nil {
		state.DeliveryStatus, state.DeliveryError = "pending", deliveryErr.Error()
	}
	s.watchdogState = state
	if err := s.watchdogStore.Set(state); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: persist watchdog delivery: %v\n", err)
		return false
	}
	return true
}

func (s *Server) noteUsefulProgress(at time.Time) {
	s.watchdogMu.Lock()
	defer s.watchdogMu.Unlock()
	state := s.watchdogState
	state.LastProgressAt, state.IncidentKey, state.DetectedAt = at, "", time.Time{}
	state.DeliveryStatus, state.DeliveryAttemptedAt, state.DeliveredAt, state.DeliveryError = "", time.Time{}, time.Time{}, ""
	state.DeliveryAcks = nil
	s.watchdogState = state
	if err := s.watchdogStore.Set(state); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: persist watchdog progress: %v\n", err)
	}
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"github.com/AmirRaptoR/Conveyor/internal/store"
)

const (
	recoveryNetwork  = "network"
	recoveryPoll     = "poll"
	recoveryUntil    = "until"
	recoveryQuota    = "quota"
	recoveryWorktree = "worktree"
	recoveryOperator = "operator"

	recoveryTransitionPrefix = "transition:"
	recoveryMovePrefix       = "provider-move:"
	recoveryMarkPrefix       = "provider-mark:"
)

var (
	recoveryNetworkBase   = 30 * time.Second
	recoveryNetworkCap    = 30 * time.Minute
	recoveryPollBase      = time.Minute
	recoveryPollCap       = 5 * time.Minute
	recoveryWorktreeBase  = time.Minute
	recoveryWorktreeCap   = 15 * time.Minute
	recoverySweepInterval = time.Second
)

// nextRecovery binds one observation to a retry instant. Only network/API
// failures use the required exponential policy; deterministic gates and local
// worktree reconciliation poll more gently, while authoritative quota/quiet
// deadlines are used exactly as reported. Jitter is hash-derived so a restart
// preserves both the attempt and its schedule instead of reshuffling it.
func nextRecovery(old, observed store.RecoveryEntry, now time.Time) store.RecoveryEntry {
	if old.Scope == observed.Scope && old.ID == observed.ID && old.Class == observed.Class && old.Key == observed.Key {
		observed.Attempt = old.Attempt + 1
	} else {
		observed.Attempt = 0
	}

	switch observed.Class {
	case recoveryUntil, recoveryQuota:
		if observed.NotBefore.Before(now) {
			observed.NotBefore = now
		}
	case recoveryNetwork:
		observed.NotBefore = now.Add(jitteredBackoff(observed, recoveryNetworkBase, recoveryNetworkCap, 20))
	case recoveryPoll:
		observed.NotBefore = now.Add(jitteredBackoff(observed, recoveryPollBase, recoveryPollCap, 10))
	case recoveryWorktree:
		observed.NotBefore = now.Add(jitteredBackoff(observed, recoveryWorktreeBase, recoveryWorktreeCap, 10))
	}
	return observed
}

func recoveryClass(class string) bool {
	switch class {
	case recoveryNetwork, recoveryPoll, recoveryUntil, recoveryQuota, recoveryWorktree:
		return true
	default:
		return false
	}
}

// recover waits for typed conditions to become due and probes them outside the
// stage history. The same adapter owns the deterministic check, but receives a
// hard probe flag and no answer/session. Shipped adapters must return before
// model dispatch in this mode; Runner.Transient ensures an unchanged check
// creates no run record, live event, plan, steering entry or notification.
func (s *Server) recover(ctx context.Context) {
	ticker := time.NewTicker(recoverySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.recoverDue(ctx, time.Now())
		}
	}
}

func (s *Server) recoverDue(ctx context.Context, now time.Time) {
	for _, entry := range s.recovery.All() {
		if entry.Scope != "item" || now.Before(entry.NotBefore) {
			continue
		}
		if entry.Class == recoveryOperator {
			continue
		}
		if entry.Class == recoveryUntil || entry.Class == recoveryQuota || engineManagedRecovery(entry) {
			s.finishRecoveryIf(entry)
			continue
		}
		s.probeRecovery(ctx, entry, now)
	}
}

func engineManagedRecovery(entry store.RecoveryEntry) bool {
	return entry.Class == recoveryNetwork &&
		(strings.HasPrefix(entry.Key, recoveryTransitionPrefix) || strings.HasPrefix(entry.Key, recoveryMovePrefix) ||
			strings.HasPrefix(entry.Key, recoveryMarkPrefix))
}

func (s *Server) probeRecovery(ctx context.Context, entry store.RecoveryEntry, now time.Time) {
	s.mu.RLock()
	var item model.Item
	found := false
	for _, it := range s.state.Items {
		if it.ID == entry.ID {
			item, found = it, true
			break
		}
	}
	s.mu.RUnlock()
	if !found || item.Blocked || item.Source != entry.Source || item.Stage != entry.Stage ||
		s.targetScriptBinding(item.Source, item.Stage) != entry.Script {
		s.finishRecoveryIf(entry)
		return
	}
	stage, stageOK := s.cfg.Stage(entry.Stage)
	src, sourceOK := s.cfg.Source(entry.Source)
	if !stageOK || !sourceOK || !stage.Runs() || !src.OK() {
		s.finishRecoveryIf(entry)
		return
	}
	if !s.claimRecoveryProbe(item) {
		return
	}
	defer s.releaseRecoveryProbe(item)
	if current, ok := s.recovery.Get(entry.Scope, entry.ID); !ok || current != entry {
		return
	}

	script := src.Paths[stage.Script]
	env := mergeRecoveryEnv(src.Env, src.Scripts[stage.Script].Params)
	if stage.Run != "" {
		script = ""
		env = mergeRecoveryEnv(nil, src.Env)
	}
	env = mergeRecoveryEnv(env, map[string]string{
		"CONVEYOR_RECOVERY_PROBE":  "1",
		"CONVEYOR_RECOVERY_CLASS":  entry.Class,
		"CONVEYOR_RECOVERY_KEY":    entry.Key,
		"CONVEYOR_DEPENDENCIES_AT": stage.DependenciesAt,
	})
	res, err := s.run.Run(ctx, runner.Spec{
		Script: script, Inline: stage.Run, Kind: "probe", Transient: true,
		Workdir: s.cfg.Workdir(*src), Env: env, Source: item.Source, Item: &item,
		From: item.Stage, To: item.Stage, Timeout: s.cfg.Discovery.D(),
		Stdin: model.StageInput{Item: &item, Stage: item.Stage, From: item.Stage},
	})
	if err != nil || res == nil {
		next := entry
		next.Class = recoveryNetwork
		next.Key = "probe"
		if err != nil {
			next.Why = err.Error()
		}
		next = nextRecovery(entry, next, now)
		if _, putErr := s.recovery.ReplaceIf(entry, next); putErr != nil {
			fmt.Fprintf(os.Stderr, "conveyor: %s: persist failed recovery probe: %v\n", entry.ID, putErr)
		}
		return
	}

	wait, waiting := waitingData(res.Data)
	if res.Run.Outcome == model.OutcomeNoop && waiting && recoveryClass(wait.Class) &&
		(wait.Key != "" || wait.Class == recoveryUntil) {
		if wait.Class != entry.Class || wait.Key != entry.Key {
			s.finishRecoveryIf(entry)
			return
		}
		observed := entry
		observed.Class, observed.Key, observed.Why, observed.NotBefore = wait.Class, wait.Key, wait.Why, wait.Until
		next := nextRecovery(entry, observed, now)
		replaced, err := s.recovery.ReplaceIf(entry, next)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conveyor: %s: persist recovery probe: %v\n", entry.ID, err)
		}
		if !replaced {
			return
		}
		s.mu.Lock()
		s.waiting[entry.ID] = model.Waiting{Class: next.Class, Key: next.Key, Why: next.Why, Until: next.NotBefore}
		s.restingAt[entry.ID] = next.NotBefore
		s.mu.Unlock()
		s.hub.publish(event{Kind: "state"})
		return
	}
	// Success, failure, a human question, or an untyped wait all mean the
	// diagnosis changed. Let the ordinary stage run record and route it once.
	s.finishRecoveryIf(entry)
}

// A probe is not a dispatch and spends no execution budget, but it still owns
// the exact item and resources its stage would use. Reserving both atomically
// closes the race with a manual start or scheduler launch; checking Busy alone
// leaves a gap in which both processes can enter the same worktree.
func (s *Server) claimRecoveryProbe(item model.Item) bool {
	if s.manuallyPaused(item.Source) {
		return false
	}
	if _, busy := s.working.LoadOrStore(item.ID, struct{}{}); busy {
		return false
	}
	if !s.eng.Locks().TryAcquire(item.Source, item.Stage, s.cfg.ResourcesFor(item.Source, item.Stage)...) {
		s.working.Delete(item.ID)
		return false
	}
	return true
}

func (s *Server) releaseRecoveryProbe(item model.Item) {
	s.eng.Locks().Release(item.Source, item.Stage, s.cfg.ResourcesFor(item.Source, item.Stage)...)
	s.working.Delete(item.ID)
}

func (s *Server) finishRecovery(itemID string) {
	if err := s.recovery.Delete("item", itemID); err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: %s: clear recovery: %v\n", itemID, err)
		return
	}
	s.finishRecoveryState(itemID)
}

func (s *Server) finishRecoveryIf(entry store.RecoveryEntry) {
	deleted, err := s.recovery.DeleteIf(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conveyor: %s: clear recovery: %v\n", entry.ID, err)
		return
	}
	if deleted {
		s.finishRecoveryState(entry.ID)
	}
}

func (s *Server) finishRecoveryState(itemID string) {
	s.mu.Lock()
	delete(s.resting, itemID)
	delete(s.restingAt, itemID)
	delete(s.waiting, itemID)
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})
	s.wakeUp()
}

const itemResumeBodyLimit = 4 << 10

// handleItemResume is the only automatic-work escape from an operator
// cancellation. It compare-and-clears that exact durable hold and records who
// resumed it and why; other recovery classes are never affected.
func (s *Server) handleItemResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Reason string `json:"reason"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, itemResumeBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `expected {"reason": "..."}`, http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		http.Error(w, "reason is required: resuming cancelled work must be auditable", http.StatusBadRequest)
		return
	}
	entry, ok := s.recovery.Get("item", id)
	if !ok || entry.Class != recoveryOperator {
		http.Error(w, id+" has no operator cancellation to resume", http.StatusConflict)
		return
	}
	resolved, err := s.recovery.ResolveOperator(entry, requestedBy(r), reason, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !resolved {
		http.Error(w, id+" recovery changed before it could be resumed", http.StatusConflict)
		return
	}
	s.finishRecoveryState(id)
	w.WriteHeader(http.StatusNoContent)
}

func waitingData(data json.RawMessage) (model.Waiting, bool) {
	var v struct {
		Waiting *model.Waiting `json:"waiting"`
	}
	if len(data) == 0 || json.Unmarshal(data, &v) != nil || v.Waiting == nil {
		return model.Waiting{}, false
	}
	if v.Waiting.Until.IsZero() && v.Waiting.Why == "" && v.Waiting.Class == "" && v.Waiting.Key == "" {
		return model.Waiting{}, false
	}
	return *v.Waiting, true
}

func mergeRecoveryEnv(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func jitteredBackoff(entry store.RecoveryEntry, base, cap time.Duration, percent int64) time.Duration {
	d := base
	for i := 0; i < entry.Attempt && d < cap; i++ {
		if d > cap/2 {
			d = cap
			break
		}
		d *= 2
	}
	if d > cap {
		d = cap
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(entry.Scope))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(entry.ID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(entry.Class))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(entry.Key))
	_, _ = h.Write([]byte{byte(entry.Attempt), byte(entry.Attempt >> 8)})
	span := int64(d) * percent / 100
	if span == 0 {
		return d
	}
	offset := int64(h.Sum64()%uint64(2*span+1)) - span
	return d + time.Duration(offset)
}

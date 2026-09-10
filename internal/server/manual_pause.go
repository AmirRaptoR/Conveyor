package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/store"
)

// pauseGlobalScope is the scope name that holds dispatch back everywhere,
// rather than for one source. The empty string, not a reserved word like
// "global", because a source is never named "" and there is nothing to
// collide with.
const pauseGlobalScope = ""

// manuallyPausedScope reports the scope actually holding this source back —
// itself, or the whole board — and the pause responsible, checking the global
// scope first since it is the more sweeping of the two and the one whose
// reason belongs on the response.
func (s *Server) manuallyPausedScope(source string) (string, store.ManualPause, bool) {
	if mp, ok := s.manualPauses.Get(pauseGlobalScope); ok {
		return pauseGlobalScope, mp, true
	}
	if source != "" {
		if mp, ok := s.manualPauses.Get(source); ok {
			return source, mp, true
		}
	}
	return "", store.ManualPause{}, false
}

// manuallyPaused reports whether new dispatch for this source is being held
// back by an operator's own pause — global, or specific to this source.
// Unlike a paused agent's quota, this is never overridden: not by the tick
// button, not by a manual start, because it is a person's decision and only a
// person's Resume undoes it (#39).
func (s *Server) manuallyPaused(source string) bool {
	_, _, held := s.manuallyPausedScope(source)
	return held
}

// pauseManual records an operator's hold on dispatch, persists it, and
// updates the board. by is the identity behind it — the Basic Auth user, when
// the board has one configured — recorded for the same reason a cancellation
// records who asked: a hold nobody can trace back to a person is a mystery
// the next person to look at the board has to solve from scratch.
func (s *Server) pauseManual(scope, reason, by string) error {
	mp := store.ManualPause{Reason: reason, By: by, At: time.Now()}
	if err := s.manualPauses.Set(scope, mp); err != nil {
		return err
	}
	s.hub.publish(event{Kind: "state"})
	return nil
}

// resumeManual lifts an operator's hold and wakes the scheduler — resuming is
// exactly the moment dispatch may have something to do again, the same reason
// unblocking one wakes it.
func (s *Server) resumeManual(scope string) error {
	if err := s.manualPauses.Clear(scope); err != nil {
		return err
	}
	s.hub.publish(event{Kind: "state"})
	s.wakeUp()
	return nil
}

// manualPauseList is the board's view of every scope currently held, global
// first and then sources in a stable order — never map iteration order,
// which would make the board's own list flicker between reads.
func (s *Server) manualPauseList() []ManualPauseView {
	all := s.manualPauses.All()
	out := make([]ManualPauseView, 0, len(all))
	for scope, mp := range all {
		out = append(out, ManualPauseView{Scope: scope, Reason: mp.Reason, By: mp.By, At: mp.At})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope == pauseGlobalScope {
			return true
		}
		if out[j].Scope == pauseGlobalScope {
			return false
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// whyManuallyPaused names the scope and reason holding a source back, for a
// manual start refused because of it.
func (s *Server) whyManuallyPaused(source string) string {
	scope, mp, held := s.manuallyPausedScope(source)
	if !held {
		return fmt.Sprintf("%s is paused", source)
	}
	if scope == pauseGlobalScope {
		return fmt.Sprintf("dispatch is paused for every source: %s", mp.Reason)
	}
	return fmt.Sprintf("%s is paused: %s", scope, mp.Reason)
}

// pauseBodyLimit bounds POST /api/pause and /api/resume — a JSON object with
// two short string fields.
const pauseBodyLimit = 4 << 10

// requestedBy is the identity behind a mutating request that carries an
// audit trail — the Basic Auth username, when the board has one configured,
// and empty otherwise. Auth is optional (CONTRACTS), so an unauthenticated
// board simply cannot say who; the audit record still carries the reason and
// the time, which is not nothing.
func requestedBy(r *http.Request) string {
	user, _, ok := r.BasicAuth()
	if !ok {
		return ""
	}
	return user
}

// handlePause holds dispatch back, globally or for one named source, until a
// person resumes it. Pausing must never affect a run already in flight — it
// only ever stops a new one from being claimed, through the same claim every
// dispatch path already goes through.
func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope  string `json:"scope"`
		Reason string `json:"reason"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, pauseBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if bodyTooLarge(err) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `expected {"scope": "...", "reason": "..."}`, http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		http.Error(w, "reason is required: a pause with no reason is indistinguishable from a stuck board", http.StatusBadRequest)
		return
	}
	if body.Scope != pauseGlobalScope {
		if _, ok := s.cfg.Source(body.Scope); !ok {
			http.Error(w, fmt.Sprintf("no source named %q", body.Scope), http.StatusBadRequest)
			return
		}
	}
	if err := s.pauseManual(body.Scope, reason, requestedBy(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleResume lifts a hold placed by handlePause. Idempotent: resuming a
// scope that was never paused is not an error, the same as unblocking an item
// that is not blocked.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope string `json:"scope"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, pauseBodyLimit)).Decode(&body)
	}
	if err := s.resumeManual(body.Scope); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

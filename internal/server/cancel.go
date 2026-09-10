package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// cancelBodyLimit bounds POST /api/items/{id}/cancel — a JSON object with one
// short string field.
const cancelBodyLimit = 4 << 10

// handleCancel stops one active run explicitly: the corrected process-group
// shutdown path (runner.Run cancelling its own runCtx, TERM then KILL across
// the whole group) reached through the cancel func runOne stored for this
// item alone, so no other run in flight is touched.
//
// A reason is required for the same reason a pause's is: an audit record with
// nothing in it is not an audit record. Recorded before the cancel func is
// called, not after — the run may finish observing the cancellation before
// this handler's own goroutine gets to write anything, and the audit trail
// must already be there when it does.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Reason string `json:"reason"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, cancelBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if bodyTooLarge(err) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `expected {"reason": "..."}`, http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		http.Error(w, "reason is required: a cancellation with no reason is a mystery for whoever looks at this item next",
			http.StatusBadRequest)
		return
	}

	v, ok := s.cancelFns.Load(id)
	if !ok {
		http.Error(w, id+" has no run in flight to cancel", http.StatusConflict)
		return
	}

	s.mu.Lock()
	s.cancels[id] = CancelView{By: requestedBy(r), Reason: reason, At: time.Now()}
	s.mu.Unlock()
	s.hub.publish(event{Kind: "state"})

	v.(context.CancelFunc)()
	w.WriteHeader(http.StatusAccepted)
}

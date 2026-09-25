package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
	"github.com/AmirRaptoR/Conveyor/internal/plan"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// --- SSE -------------------------------------------------------------------

type event struct {
	Kind       string               `json:"kind"` // log | state | polling | transition | plan | steering
	RunID      string               `json:"runId,omitempty"`
	ItemID     string               `json:"itemId,omitempty"`
	Line       *runner.LogLine      `json:"line,omitempty"`
	Transition *pipeline.Transition `json:"transition,omitempty"`
	// Plan and Rejected are the "plan" event's payload: the last accepted
	// revision (nil when there is none yet) and the running rejected-line
	// count for the run named by RunID — published for every accepted
	// revision and every rejection alike, so a run that publishes one good
	// revision and then only bad ones still moves the board's count.
	Plan     *plan.Revision   `json:"plan,omitempty"`
	Accepted *int             `json:"accepted,omitempty"`
	Rejected *int             `json:"rejected,omitempty"`
	Steering *RunSteeringView `json:"steering,omitempty"`
}

type hub struct {
	mu   sync.Mutex
	subs map[chan event]struct{}
}

func newHub() *hub { return &hub{subs: map[chan event]struct{}{}} }

func (h *hub) subscribe() chan event {
	ch := make(chan event, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}

// publish never blocks: a browser that has stopped reading must not be able to
// stall the pipeline that is producing the logs.
func (h *hub) publish(e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e := <-ch:
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

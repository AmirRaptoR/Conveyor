package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AmirRaptoR/Conveyor/internal/steering"
)

// steerBodyLimit bounds POST /api/items/{id}/steer.
const steerBodyLimit = 8 << 10

// steerAppendMu serialises appends to one run's control.jsonl across
// concurrent requests: AppendCommand itself derives seq from the file's
// current line count and is not safe for concurrent callers on its own
// (see its own doc comment). One mutex per run id, not a global one, so
// steering two different runs at once never waits on each other.
var (
	steerAppendMu   sync.Map // runID -> *sync.Mutex
	steerAppendMuMu sync.Mutex
)

func steerLockFor(runID string) *sync.Mutex {
	if v, ok := steerAppendMu.Load(runID); ok {
		return v.(*sync.Mutex)
	}
	steerAppendMuMu.Lock()
	defer steerAppendMuMu.Unlock()
	if v, ok := steerAppendMu.Load(runID); ok {
		return v.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	steerAppendMu.Store(runID, m)
	return m
}

// handleSteer enqueues one command — an instruction or a pause — against a
// live stage run, binding it to an exact item, run id and, when supplied,
// session. It refuses rather than guessing: no item, no live run, a run id
// naming a run that has already finished, a run that never advertised
// steering at all or not this command's kind, a session that is not the
// one the run currently reports, an unknown kind, empty or over-cap text,
// and the run's own 50-command lifetime ceiling.
func (s *Server) handleSteer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Kind    string `json:"kind"`
		Text    string `json:"text"`
		RunID   string `json:"runId"`
		Session string `json:"session"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, steerBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if bodyTooLarge(err) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `expected {"kind": "instruction"|"pause", "text": "...", "runId": "...", "session": "..."}`, http.StatusBadRequest)
		return
	}

	if body.Kind != steering.KindInstruction && body.Kind != steering.KindPause {
		http.Error(w, `kind must be "instruction" or "pause"`, http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(body.Text)
	if body.Kind == steering.KindPause {
		text = ""
	} else {
		if text == "" {
			http.Error(w, "text is required for an instruction", http.StatusBadRequest)
			return
		}
		if utf8.RuneCountInString(text) > steering.MaxTextLen {
			http.Error(w, "text is too long", http.StatusBadRequest)
			return
		}
	}
	if body.RunID == "" {
		http.Error(w, "runId is required", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	found := false
	for _, it := range s.state.Items {
		if it.ID == id {
			found = true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		http.Error(w, "no item "+id+" on the board", http.StatusNotFound)
		return
	}

	entry, ok := s.liveRuns.Lookup(id)
	if !ok {
		http.Error(w, id+" has no live run to steer", http.StatusConflict)
		return
	}
	if entry.RunID != body.RunID {
		http.Error(w, "that run has already finished", http.StatusConflict)
		return
	}

	lock := steerLockFor(entry.RunID)
	lock.Lock()
	defer lock.Unlock()

	res := steering.Load(entry.Dir, false)
	if len(res.Accepts) == 0 {
		http.Error(w, "this run has not advertised any steering capability", http.StatusConflict)
		return
	}
	advertised := false
	for _, k := range res.Accepts {
		if k == body.Kind {
			advertised = true
			break
		}
	}
	if !advertised {
		http.Error(w, "this run does not accept "+body.Kind, http.StatusConflict)
		return
	}
	if body.Session != "" && body.Session != res.Session {
		http.Error(w, "that is not this run's current session", http.StatusConflict)
		return
	}
	if len(res.Commands) >= steering.MaxCommands {
		http.Error(w, "this run has already reached its command limit", http.StatusConflict)
		return
	}

	cmd := steering.Command{
		ID: steerID(), At: time.Now(), Kind: body.Kind, Text: text,
		ItemID: id, RunID: body.RunID, Session: body.Session, By: requestedBy(r),
	}
	ctlPath := filepath.Join(entry.Dir, "control.jsonl")
	if _, err := steering.AppendCommand(ctlPath, cmd); err != nil {
		http.Error(w, "could not record the command: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publish(event{Kind: "state"})
	w.WriteHeader(http.StatusAccepted)
}

// steerID mints a command id: short enough for the 32-character shape the
// issue's own protocol sketch names, unique enough for one run's lifetime
// (at most 50 commands ever).
func steerID() string {
	return "cmd-" + time.Now().UTC().Format("150405.000000000")
}

package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AmirRaptoR/Conveyor/internal/steering"
)

// steerBodyLimit bounds POST /api/items/{id}/steer.
const steerBodyLimit = 8 << 10

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
		http.Error(w, "runId is required: a command must name the live run it is for", http.StatusConflict)
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

	lease, ok := s.liveRuns.Acquire(id, body.RunID)
	if !ok {
		http.Error(w, "that run has already finished", http.StatusConflict)
		return
	}
	defer lease.Release()
	entry = lease.Entry()

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
	lease.SetSession(res.Session)
	if len(res.Commands) >= steering.MaxCommands {
		http.Error(w, "this run has already reached its command limit", http.StatusConflict)
		return
	}

	commandID, err := steerID()
	if err != nil {
		http.Error(w, "could not mint command id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cmd := steering.Command{
		ID: commandID, At: time.Now(), Kind: body.Kind, Text: text,
		ItemID: id, RunID: body.RunID, Session: res.Session, By: requestedBy(r),
	}
	intent, audited := s.beginHumanAuditHTTP(w, r, "steer-"+body.Kind, id)
	if !audited {
		return
	}
	ctlPath := filepath.Join(entry.Dir, "control.jsonl")
	seq, err := steering.AppendCommand(ctlPath, cmd)
	if err != nil {
		s.resolveHumanAudit(intent, false)
		http.Error(w, "could not record the command: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cmd.Seq = seq
	res = steering.Load(entry.Dir, false)
	view := steeringView(body.RunID, entry.Stage, res, true)
	s.mu.Lock()
	s.steeringGen[id]++
	s.steering[id] = view.SteeringSummary
	delete(s.steeringMisses, id)
	s.mu.Unlock()
	s.hub.publish(event{Kind: "steering", RunID: body.RunID, ItemID: id, Steering: &view})
	s.resolveHumanAudit(intent, true)
	w.WriteHeader(http.StatusAccepted)
}

// steerID mints a command id: short enough for the 32-character shape the
// issue's own protocol sketch names, unique enough for one run's lifetime
// (at most 50 commands ever).
func steerID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cmd-" + hex.EncodeToString(b), nil
}

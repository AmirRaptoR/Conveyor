package store

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// ManualPause is one operator-initiated hold on new dispatch: unlike an
// agent's quota pause (a fact the agent itself reported, rediscovered fresh
// from its status script on every restart), this is a person's own decision
// and nothing else can lift it, so it is the one pause worth writing to disk.
type ManualPause struct {
	Reason string    `json:"reason"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at"`
}

// Pauses holds the manual holds a person has placed on dispatch, keyed by
// scope: the empty string for the whole board, or one source's name.
//
// Same shape as Order and Answers and for the same reason: this is a decision
// a person made, not a fact rederivable from a provider or an agent's status
// script, so a restart must not silently forget it — see CONTRACTS' "Restart
// retains manual control state" for #39.
type Pauses struct {
	path string

	dataMu sync.RWMutex
	m      map[string]ManualPause

	writeMu sync.Mutex
}

// OpenPauses reads the pauses at path, or starts empty. A malformed file is an
// empty set rather than a fatal error, for the same reason Order and Answers
// do it: refusing to start would strand every repository over one unreadable
// file, and the safe direction to be wrong in is to dispatch, not to wedge.
func OpenPauses(path string) *Pauses {
	p := &Pauses{path: path, m: map[string]ManualPause{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	_ = json.Unmarshal(b, &p.m)
	if p.m == nil {
		p.m = map[string]ManualPause{}
	}
	return p
}

// Set records a pause on a scope, replacing any existing one for it.
func (p *Pauses) Set(scope string, mp ManualPause) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	next := p.snapshot()
	next[scope] = mp
	if err := p.persist(next); err != nil {
		return err
	}
	p.dataMu.Lock()
	p.m = next
	p.dataMu.Unlock()
	return nil
}

// Clear lifts a pause on a scope. Clearing a scope that was not paused is not
// an error: resuming is idempotent, the same as unblocking an item that is
// not blocked.
func (p *Pauses) Clear(scope string) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	next := p.snapshot()
	delete(next, scope)
	if err := p.persist(next); err != nil {
		return err
	}
	p.dataMu.Lock()
	p.m = next
	p.dataMu.Unlock()
	return nil
}

// Get reports the pause on one scope, if there is one.
func (p *Pauses) Get(scope string) (ManualPause, bool) {
	p.dataMu.RLock()
	defer p.dataMu.RUnlock()
	mp, ok := p.m[scope]
	return mp, ok
}

// All is every scope currently paused.
func (p *Pauses) All() map[string]ManualPause {
	p.dataMu.RLock()
	defer p.dataMu.RUnlock()
	out := make(map[string]ManualPause, len(p.m))
	for k, v := range p.m {
		out[k] = v
	}
	return out
}

func (p *Pauses) snapshot() map[string]ManualPause {
	p.dataMu.RLock()
	defer p.dataMu.RUnlock()
	out := make(map[string]ManualPause, len(p.m))
	for k, v := range p.m {
		out[k] = v
	}
	return out
}

func (p *Pauses) persist(m map[string]ManualPause) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(p.path, append(b, '\n'))
}

package store

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// RecoveryEntry is one deterministic condition that must survive a restart.
// Scope and ID form its identity; the remaining fields bind an item wait to
// the exact source, stage and script that observed it, or describe one source's
// listing backoff. Class is engine vocabulary while Key is adapter-owned and
// compared only for equality.
type RecoveryEntry struct {
	Scope     string    `json:"scope"`
	ID        string    `json:"id"`
	Source    string    `json:"source,omitempty"`
	Stage     string    `json:"stage,omitempty"`
	Script    string    `json:"script,omitempty"`
	Class     string    `json:"class"`
	Key       string    `json:"key,omitempty"`
	Why       string    `json:"why,omitempty"`
	Attempt   int       `json:"attempt,omitempty"`
	NotBefore time.Time `json:"notBefore,omitempty"`
}

type Recovery struct {
	path string

	dataMu sync.RWMutex
	m      map[string]RecoveryEntry

	writeMu sync.Mutex
}

func recoveryKey(scope, id string) string { return scope + "\x00" + id }

// OpenRecovery reads the recovery ledger, or starts empty when it is absent or
// malformed. Provider state remains fail-closed after an unreadable listing;
// losing a delay must not make the entire board refuse to start.
func OpenRecovery(path string) *Recovery {
	r := &Recovery{path: path, m: map[string]RecoveryEntry{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return r
	}
	_ = json.Unmarshal(b, &r.m)
	if r.m == nil {
		r.m = map[string]RecoveryEntry{}
	}
	return r
}

func (r *Recovery) Put(entry RecoveryEntry) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	next := r.snapshot()
	next[recoveryKey(entry.Scope, entry.ID)] = entry
	if err := r.persist(next); err != nil {
		return err
	}
	r.dataMu.Lock()
	r.m = next
	r.dataMu.Unlock()
	return nil
}

func (r *Recovery) Delete(scope, id string) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	next := r.snapshot()
	delete(next, recoveryKey(scope, id))
	if err := r.persist(next); err != nil {
		return err
	}
	r.dataMu.Lock()
	r.m = next
	r.dataMu.Unlock()
	return nil
}

// ReplaceIf atomically replaces old only while the ledger still contains that
// exact observation. A transient probe uses this after it returns so a manual
// action, a newer run, or another probe cannot be undone by stale output.
func (r *Recovery) ReplaceIf(old, nextEntry RecoveryEntry) (bool, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	next := r.snapshot()
	key := recoveryKey(old.Scope, old.ID)
	if current, ok := next[key]; !ok || current != old {
		return false, nil
	}
	next[key] = nextEntry
	if err := r.persist(next); err != nil {
		return false, err
	}
	r.dataMu.Lock()
	r.m = next
	r.dataMu.Unlock()
	return true, nil
}

func (r *Recovery) DeleteIf(old RecoveryEntry) (bool, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	next := r.snapshot()
	key := recoveryKey(old.Scope, old.ID)
	if current, ok := next[key]; !ok || current != old {
		return false, nil
	}
	delete(next, key)
	if err := r.persist(next); err != nil {
		return false, err
	}
	r.dataMu.Lock()
	r.m = next
	r.dataMu.Unlock()
	return true, nil
}

func (r *Recovery) Get(scope, id string) (RecoveryEntry, bool) {
	r.dataMu.RLock()
	defer r.dataMu.RUnlock()
	e, ok := r.m[recoveryKey(scope, id)]
	return e, ok
}

func (r *Recovery) All() []RecoveryEntry {
	r.dataMu.RLock()
	defer r.dataMu.RUnlock()
	out := make([]RecoveryEntry, 0, len(r.m))
	for _, e := range r.m {
		out = append(out, e)
	}
	return out
}

func (r *Recovery) snapshot() map[string]RecoveryEntry {
	r.dataMu.RLock()
	defer r.dataMu.RUnlock()
	out := make(map[string]RecoveryEntry, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}

func (r *Recovery) persist(m map[string]RecoveryEntry) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(r.path, append(b, '\n'))
}

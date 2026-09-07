package store

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// Answers holds what a person typed when they handed a marked item back, until
// the run that asked for it has been given it.
//
// On disk, and not merely in memory, because this is the one piece of state
// that cannot be re-derived from anywhere: the provider does not have it, the
// run history does not have it, and a person wrote it by hand. A restart in the
// seconds between "send" and the next run would otherwise silently drop a
// paragraph and let the agent ask the same question again — which is exactly
// the trip this whole feature exists to save.
//
// Same shape as Order and for the same reasons: a JSON file, rewritten whole,
// a handful of entries at most. An answer lives only until it is spent. See
// the package comment for the two-lock discipline every method here follows.
type Answers struct {
	path string

	dataMu sync.RWMutex
	m      map[string]model.Resume

	writeMu sync.Mutex
}

// OpenAnswers reads the answers at path, or starts empty. A malformed file is
// an empty set rather than a fatal error, for the same reason Order does it:
// refusing to start would strand every repository over one unreadable note.
func OpenAnswers(path string) *Answers {
	a := &Answers{path: path, m: map[string]model.Resume{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return a
	}
	_ = json.Unmarshal(b, &a.m)
	if a.m == nil {
		a.m = map[string]model.Resume{}
	}
	return a
}

// Set records an answer for an item, replacing any unspent one. The session is
// captured here and not at the next run because clearing a mark forgets why the
// item stopped, and the conversation to reply into is part of why.
//
// Persisted before it is visible in memory, under the writer lock: a caller
// that gets a nil error back knows the reply is on disk, and a failed write
// leaves the previous answer in place on both sides rather than recording one
// the disk does not have.
func (a *Answers) Set(id string, r model.Resume) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	next := a.snapshot()
	// Empty in every field is a caller saying "nothing is armed for this
	// item", which is a delete. Manual counts: an action a person pressed is
	// held here for the same reason a reply is — something they said, kept
	// until the run that was waiting for it happens, then gone.
	if r == (model.Resume{}) {
		delete(next, id)
	} else {
		next[id] = r
	}
	if err := a.persist(next); err != nil {
		return err
	}
	a.dataMu.Lock()
	a.m = next
	a.dataMu.Unlock()
	return nil
}

// Get returns the unspent answer for an item, if there is one.
func (a *Answers) Get(id string) model.Resume {
	a.dataMu.RLock()
	defer a.dataMu.RUnlock()
	return a.m[id]
}

// Take spends the answer a caller already read with Get and is about to
// consume — not whatever is on file right now. It is a compare-and-delete: if
// a new answer was recorded for the same item while the caller's run was in
// flight, that new answer is not this call's to spend, and it survives.
//
// Reports whether it actually spent spent, so a caller that raced a fresher
// answer knows not to treat it as delivered. The write error is returned, not
// discarded: a Take whose write failed must not report the answer as spent,
// or a restart finds it gone from disk while every caller was told otherwise.
func (a *Answers) Take(id string, spent model.Resume) (bool, error) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.dataMu.RLock()
	cur, ok := a.m[id]
	a.dataMu.RUnlock()
	if !ok || cur != spent {
		return false, nil
	}

	next := a.snapshot()
	delete(next, id)
	if err := a.persist(next); err != nil {
		return false, err
	}
	a.dataMu.Lock()
	a.m = next
	a.dataMu.Unlock()
	return true, nil
}

// snapshot copies the current answers. Called only from within a writeMu
// critical section, so the copy it starts from cannot change under it before
// persist writes it out.
func (a *Answers) snapshot() map[string]model.Resume {
	a.dataMu.RLock()
	defer a.dataMu.RUnlock()
	out := make(map[string]model.Resume, len(a.m))
	for k, v := range a.m {
		out[k] = v
	}
	return out
}

func (a *Answers) persist(m map[string]model.Resume) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(a.path, append(b, '\n'))
}

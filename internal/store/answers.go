package store

import (
	"encoding/json"
	"os"
	"path/filepath"
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
// a handful of entries at most. An answer lives only until it is spent.
type Answers struct {
	path string

	mu sync.RWMutex
	m  map[string]model.Resume
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
func (a *Answers) Set(id string, r model.Resume) error {
	a.mu.Lock()
	if r.Answer == "" && r.Session == "" {
		delete(a.m, id)
	} else {
		a.m[id] = r
	}
	snap := a.snapshot()
	a.mu.Unlock()
	return a.write(snap)
}

// Get returns the unspent answer for an item, if there is one.
func (a *Answers) Get(id string) model.Resume {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.m[id]
}

// Take spends an answer: it is handed over once and then forgotten, so a second
// run of the same stage does not silently repeat a reply to a question nobody
// asked twice.
func (a *Answers) Take(id string) model.Resume {
	a.mu.Lock()
	r, ok := a.m[id]
	if !ok {
		a.mu.Unlock()
		return model.Resume{}
	}
	delete(a.m, id)
	snap := a.snapshot()
	a.mu.Unlock()
	_ = a.write(snap)
	return r
}

func (a *Answers) snapshot() map[string]model.Resume {
	out := make(map[string]model.Resume, len(a.m))
	for k, v := range a.m {
		out[k] = v
	}
	return out
}

// write replaces the file atomically, so a crash mid-write cannot leave a
// half-parsed answer behind.
func (a *Answers) write(m map[string]model.Resume) error {
	if a.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

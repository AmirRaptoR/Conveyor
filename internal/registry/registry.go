// Package registry is the live-run registry #111 needs: the one place that
// knows a stage process is actually running right now and where its run
// directory is.
//
// internal/server's existing Active cannot serve this — it is set before
// Engine.Advance and survives the post-stage provider move, so a handler
// gated on it would append a steering command to a run that has already
// finished and answer 202 for nothing. Registry is opened in
// Runner.OnStart and closed in Runner.OnResult instead, so an entry exists
// exactly while the stage process itself is running.
package registry

import "sync"

// Entry is what the registry remembers about one live stage run.
type Entry struct {
	RunID string
	Dir   string
	Stage string
}

// Registry maps an item id to its currently live stage run, if any.
type Registry struct {
	mu     sync.Mutex
	byItem map[string]Entry
}

func New() *Registry { return &Registry{byItem: map[string]Entry{}} }

// Open records a live run for itemID. Only a "stage" run with a non-empty
// item id and target stage should ever be passed here — a list, move,
// doctor or status run is never steerable, and that filtering is the
// caller's (Runner.OnStart's) job, not this package's.
func (r *Registry) Open(itemID string, e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byItem[itemID] = e
}

// Close removes the entry for itemID, but only if it still names runID — a
// second stage run for the same item that started after this one already
// closed must not have its own, newer entry clobbered by a late-arriving
// Close belonging to the first.
func (r *Registry) Close(itemID, runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byItem[itemID]; ok && e.RunID == runID {
		delete(r.byItem, itemID)
	}
}

// Lookup returns the live entry for itemID, if any.
func (r *Registry) Lookup(itemID string) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byItem[itemID]
	return e, ok
}

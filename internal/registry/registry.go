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
	// Session is the last session the server observed from this run's ack
	// channel. It is advisory metadata; the run directory remains the durable
	// record.
	Session string
}

type liveEntry struct {
	op     sync.Mutex
	closed bool
	entry  Entry
}

// Registry maps an item id to its currently live stage run, if any.
type Registry struct {
	mu     sync.Mutex
	byItem map[string]*liveEntry
}

func New() *Registry { return &Registry{byItem: map[string]*liveEntry{}} }

// Open records a live run for itemID. Only a "stage" run with a non-empty
// item id and target stage should ever be passed here — a list, move,
// list, move or status run is never steerable, and that filtering is the
// caller's (Runner.OnStart's) job, not this package's.
func (r *Registry) Open(itemID string, e Entry) {
	next := &liveEntry{entry: e}
	for {
		r.mu.Lock()
		old := r.byItem[itemID]
		if old == nil {
			r.byItem[itemID] = next
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()

		old.op.Lock()
		old.closed = true
		old.op.Unlock()

		r.mu.Lock()
		if r.byItem[itemID] == old {
			r.byItem[itemID] = next
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
	}
}

// Close removes the entry for itemID, but only if it still names runID — a
// second stage run for the same item that started after this one already
// closed must not have its own, newer entry clobbered by a late-arriving
// Close belonging to the first.
func (r *Registry) Close(itemID, runID string) {
	r.mu.Lock()
	e := r.byItem[itemID]
	r.mu.Unlock()
	if e == nil || e.entry.RunID != runID {
		return
	}

	e.op.Lock()
	defer e.op.Unlock()
	if e.closed || e.entry.RunID != runID {
		return
	}
	e.closed = true
	r.mu.Lock()
	if r.byItem[itemID] == e {
		delete(r.byItem, itemID)
	}
	r.mu.Unlock()
}

// Lookup returns the live entry for itemID, if any.
func (r *Registry) Lookup(itemID string) (Entry, bool) {
	r.mu.Lock()
	e, ok := r.byItem[itemID]
	r.mu.Unlock()
	if !ok {
		return Entry{}, false
	}
	e.op.Lock()
	defer e.op.Unlock()
	if e.closed {
		return Entry{}, false
	}
	return e.entry, true
}

// Lease holds one atomic operation against a live entry. Close waits for the
// lease, so an enqueue racing process exit has only two outcomes: it completes
// while the run is live, or Acquire refuses it. It can never append after
// Close has committed. Different entries have different operation locks.
type Lease struct {
	e *liveEntry
}

// Acquire returns a lease only when itemID still names runID. An empty runID
// is never inferred from the live entry: callers must bind an operation to the
// exact run they observed.
func (r *Registry) Acquire(itemID, runID string) (*Lease, bool) {
	if runID == "" {
		return nil, false
	}
	r.mu.Lock()
	e := r.byItem[itemID]
	r.mu.Unlock()
	if e == nil || e.entry.RunID != runID {
		return nil, false
	}
	e.op.Lock()
	if e.closed || e.entry.RunID != runID {
		e.op.Unlock()
		return nil, false
	}
	return &Lease{e: e}, true
}

func (l *Lease) Entry() Entry { return l.e.entry }

func (l *Lease) SetSession(session string) { l.e.entry.Session = session }

func (l *Lease) Release() {
	if l == nil || l.e == nil {
		return
	}
	l.e.op.Unlock()
	l.e = nil
}

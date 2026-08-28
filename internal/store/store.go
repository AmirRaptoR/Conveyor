// Package store persists the one thing a human controls: the order of the
// inputs. Everything else the engine needs it can re-derive by asking the
// sources, so this is deliberately the only state conveyor keeps.
//
// A JSON file, not a database. The order is a list of item ids a person
// arranged by hand — hundreds at most, rewritten whole on every change, read
// once per scheduling pass. SQLite would buy nothing here and would have to be
// explained to anyone reading a run directory.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Order is the manual input order. An id's position beats its priority; ids
// absent from it fall back to priority, so a partially ordered queue works and
// dragging one card does not demand ranking the rest.
type Order struct {
	path string

	mu  sync.RWMutex
	ids []string
}

// OpenOrder reads the order at path, or starts empty if it is not there yet.
// A malformed file is an empty order rather than a fatal error: losing a manual
// arrangement is a nuisance, but refusing to start over it would strand every
// repository the pipeline was working.
func OpenOrder(path string) *Order {
	o := &Order{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		return o
	}
	_ = json.Unmarshal(b, &o.ids)
	return o
}

// IDs is the order as Pick wants it.
func (o *Order) IDs() []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return append([]string(nil), o.ids...)
}

// Set replaces the order and writes it out. Duplicates are dropped, keeping the
// first occurrence, so a client that sends a stale list cannot make one item
// outrank itself.
func (o *Order) Set(ids []string) error {
	seen := make(map[string]bool, len(ids))
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}

	o.mu.Lock()
	o.ids = clean
	o.mu.Unlock()
	return o.write(clean)
}

// write replaces the file atomically: a half-written order read by the next
// scheduling pass would silently reorder the queue.
func (o *Order) write(ids []string) error {
	if o.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}

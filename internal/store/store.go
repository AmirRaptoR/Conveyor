// Package store persists the one thing a human controls: the order of the
// inputs. Everything else the engine needs it can re-derive by asking the
// sources, so this is deliberately the only state conveyor keeps.
//
// A JSON file, not a database. The order is a list of item ids a person
// arranged by hand — hundreds at most, rewritten whole on every change, read
// once per scheduling pass. SQLite would buy nothing here and would have to be
// explained to anyone reading a run directory.
//
// # Locking
//
// Every store here (Order, Answers) uses two locks, not one, and the second
// exists for a reason worth stating once:
//
//   - a data lock (sync.RWMutex) guards the in-memory copy. IDs()/Get() take
//     only this, as a read lock, so a slow write's fsync never stalls the
//     scheduler's hot path.
//   - a writer lock (sync.Mutex) serialises mutation, persistence and
//     publication for every call that changes the file: it is held across
//     marshal, write, fsync and rename, and the in-memory copy is only
//     updated after that succeeds. Two concurrent writers can therefore never
//     have the file and memory disagree, and neither can publish a stale
//     snapshot over a newer one — there is only ever one writer running.
//
// Neither lock is ever held while calling anything outside this package: a
// store here does not know its caller exists, so there is no lock order to
// invert against a caller's own lock (Server.mu). A caller that takes its own
// lock and then calls Set (as handleOrder does) or takes it after calling IDs
// (as refresh does) is safe either way, because the two never nest.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// writeAtomic writes b to a uniquely named temporary file beside path, fsyncs
// it, and renames it onto path.
//
// The name is unique per call, not a fixed "path.tmp": two writers racing a
// fixed name can have one's write clobbered mid-flight by the other's, or one
// rename fail because the file it expected to replace was already replaced.
// A rename is still what makes the final replacement atomic to any reader —
// uniqueness only protects the writers from each other while they are
// writing, which matters because the callers here serialise their own
// mutation but still want a crash mid-write to never leave a corrupt file
// where the real one belongs.
func writeAtomic(path string, b []byte) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(name)
		}
	}()
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	return nil
}

// Order is the manual input order. An id's position beats its priority; ids
// absent from it fall back to priority, so a partially ordered queue works and
// dragging one card does not demand ranking the rest.
type Order struct {
	path string

	dataMu sync.RWMutex
	ids    []string

	writeMu sync.Mutex
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

// IDs is the order as Pick wants it. It takes only the data lock, so it is
// never blocked behind a concurrent Set's fsync.
func (o *Order) IDs() []string {
	o.dataMu.RLock()
	defer o.dataMu.RUnlock()
	return append([]string(nil), o.ids...)
}

// Set replaces the order and writes it out. Duplicates are dropped, keeping the
// first occurrence, so a client that sends a stale list cannot make one item
// outrank itself.
//
// Mutation, persistence and publication happen under the writer lock, in that
// order: the file is written and fsynced before the in-memory copy changes, so
// a caller that gets a nil error back knows disk and memory already agree, and
// a failed write leaves both exactly as they were.
func (o *Order) Set(ids []string) error {
	clean := cleanIDs(ids)

	o.writeMu.Lock()
	defer o.writeMu.Unlock()

	b, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(o.path, append(b, '\n')); err != nil {
		return err
	}

	o.dataMu.Lock()
	o.ids = clean
	o.dataMu.Unlock()
	return nil
}

func cleanIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}
	return clean
}

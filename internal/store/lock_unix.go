//go:build unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Lock is an exclusive claim on a data directory, held for as long as the
// process that took it lives.
//
// Two conveyors over one data directory is not a slow pipeline, it is a wrong
// one: each sweeps the other's in-flight runs to "interrupted", and each hands
// the same item to an agent in the same worktree. The claim is what makes the
// sweep safe to perform at all — a run still marked running is abandoned only
// if nobody else is running it.
//
// An advisory file lock rather than a pidfile, because the kernel drops it when
// the holder dies: a machine that lost power comes back with no stale claim to
// reason about, and no pid to check against a number that has since been
// reused.
type Lock struct{ f *os.File }

// Busy says the claim is someone else's, and names them where it can. The pid
// is written into the file purely so this sentence can be specific; the lock
// itself is the kernel's, never the number.
type Busy struct {
	Dir string
	PID int
}

func (b *Busy) Error() string {
	who := "another conveyor"
	if b.PID > 0 {
		who = fmt.Sprintf("another conveyor (pid %d)", b.PID)
	}
	return fmt.Sprintf("%s is already working %s", who, b.Dir)
}

// AcquireLock claims path without blocking, returning *Busy if it is held.
// Refusing is the point: a second scheduler over one set of sources would be
// discovered as corrupted history and duplicated work, hours later.
func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readPID(f)
		_ = f.Close()
		return nil, &Busy{Dir: filepath.Dir(path), PID: pid}
	}
	_ = f.Truncate(0)
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		// Losing the diagnostic is not losing the lock; keep the claim.
		_ = err
	}
	return &Lock{f: f}, nil
}

// Release drops the claim. Not required for correctness — process exit drops it
// too — but a command that finishes should not hold the pipeline out.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}

func readPID(f *os.File) int {
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	if err != nil {
		return 0
	}
	return pid
}

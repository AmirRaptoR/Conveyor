// Package linefeed implements the byte-offset tailing, partial-line
// buffering and oversized-fragment handling shared by every append-only,
// newline-delimited sidecar channel a run writes — plan.jsonl
// (internal/plan) and control-ack.jsonl (internal/steering) alike — so
// there is one copy of the offset/partial-line/fragment-cap mechanics
// instead of one per channel.
//
// This package owns no state of its own: a caller keeps its own Cursor
// (three plain fields) so its own tests can inspect offset/buffering
// directly, the way internal/plan's already do.
package linefeed

import (
	"bytes"
	"io"
	"os"
)

// Cursor holds what one Poll/Final pair needs to remember across calls: the
// byte offset already consumed, a pending partial-line buffer, and whether
// that buffer is mid-discard because it grew past the line cap before its
// terminating newline arrived.
type Cursor struct {
	Offset   int64
	Buf      []byte
	Skipping bool
}

// OnLine is called once per complete line found, in file order: a
// newline-terminated line's bytes with the newline stripped, or
// oversize=true and data=nil when the accumulated fragment exceeded maxLine
// before its terminating newline showed up — the line's content is
// unrecoverable at that point, only its existence and length are known. It
// returns halt to stop scanning further lines in this Poll call (a
// caller-specific cap being reached); halt is only ever checked after a
// callback runs, never before the first.
type OnLine func(data []byte, oversize bool) (halt bool)

// Poll reads whatever is new at path since c.Offset, splits it into
// complete lines and calls fn once per line (or per discarded oversized
// fragment) in order, advancing c as it goes. It returns stopped=true and a
// one-sentence reason once path shrinks below c.Offset or becomes
// unreadable — the caller's cue to stop tailing this file entirely, never
// reopening or restarting it.
func Poll(c *Cursor, path string, maxLine int, fn OnLine) (stopped bool, reason string) {
	info, err := os.Stat(path)
	if err != nil {
		return true, "unreadable: " + err.Error()
	}
	if info.Size() < c.Offset {
		return true, "truncated below the held offset"
	}
	if info.Size() == c.Offset {
		return false, ""
	}
	f, err := os.Open(path)
	if err != nil {
		return true, "unreadable: " + err.Error()
	}
	defer f.Close()
	if _, err := f.Seek(c.Offset, io.SeekStart); err != nil {
		return true, "unreadable: " + err.Error()
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			c.Offset += int64(n)
			if consume(c, buf[:n], maxLine, fn) {
				return false, ""
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return true, "unreadable: " + readErr.Error()
			}
			return false, ""
		}
	}
}

// consume splits data into complete lines against c, calling fn for each and
// returning true the moment fn asks to halt.
func consume(c *Cursor, data []byte, maxLine int, fn OnLine) (halted bool) {
	for {
		nl := bytes.IndexByte(data, '\n')
		if nl == -1 {
			if !c.Skipping {
				c.Buf = append(c.Buf, data...)
				if len(c.Buf) > maxLine {
					c.Buf = nil
					c.Skipping = true
					return fn(nil, true)
				}
			}
			return false
		}
		line := data[:nl]
		data = data[nl+1:]

		if c.Skipping {
			c.Skipping = false
			continue
		}

		full := line
		if len(c.Buf) > 0 {
			full = append(c.Buf, line...)
			c.Buf = nil
		}

		if len(full)+1 > maxLine {
			if fn(nil, true) {
				return true
			}
			continue
		}

		if fn(full, false) {
			return true
		}
	}
}

// Final resolves whatever partial fragment remains in c after the process
// writing path has exited: a non-empty, non-skipping buffer is a line the
// writer never finished, returned with truncated=true so a caller can count
// it as one rejection. Skipping is always cleared, since there is no writer
// left to ever complete it.
func Final(c *Cursor) (data []byte, truncated bool) {
	skipping := c.Skipping
	c.Skipping = false
	if len(c.Buf) > 0 && !skipping {
		data = c.Buf
		c.Buf = nil
		return data, true
	}
	c.Buf = nil
	return nil, false
}

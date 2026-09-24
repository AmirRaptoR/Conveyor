package plan

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

// maxDiagnostics bounds how many rejection events in one run are worth an
// "engine"-stream log line each; beyond that, only the running counts
// change, and Final's summary event says how many more there were.
const maxDiagnostics = 10

// Event is one thing Reader noticed: an accepted revision, a rejected line,
// a stopped-tailing notice, or the end-of-run suppressed-count summary.
// Exactly one of Accepted, Stopped or Summary describes a rejection-shaped
// event; a caller emitting a log line checks Diagnose (or Stopped/Summary)
// rather than assuming every non-accepted Event is worth printing.
type Event struct {
	Accepted bool
	Revision Revision
	Line     int
	Reason   string
	// Diagnose is true for the first maxDiagnostics rejections in a run —
	// the ones worth an actual engine-stream log line.
	Diagnose bool
	// Summary is the one event Final emits when rejections were suppressed,
	// naming how many.
	Summary bool
	// Stopped is true once plan.jsonl was truncated below the held offset,
	// removed, or became unreadable; tailing for this run ends here.
	Stopped bool
}

// Reader tails one run's plan.jsonl: it holds a byte offset, a pending
// partial-line buffer and the running accept/reject state a caller needs to
// serve /api/state, the SSE plan event and GET /api/runs/{id} without
// re-parsing the whole file on every poll.
type Reader struct {
	offset   int64
	buf      []byte
	skipping bool // discarding bytes of an over-long fragment until its newline

	lastRev      int
	lastAccepted *Revision
	acceptedN    int
	rejectedN    int
	diagnosedN   int
	lineNum      int
	stopped      bool
	capped       bool
}

// NewReader returns a Reader positioned at the start of a run's plan.jsonl.
func NewReader() *Reader { return &Reader{} }

// Accepted is the count of revisions accepted so far, capped at MaxRevisions.
func (r *Reader) Accepted() int { return r.acceptedN }

// Rejected is the count of lines rejected so far, uncapped.
func (r *Reader) Rejected() int { return r.rejectedN }

// Last is the most recently accepted revision, and whether there is one.
func (r *Reader) Last() (Revision, bool) {
	if r.lastAccepted == nil {
		return Revision{}, false
	}
	return *r.lastAccepted, true
}

// Poll reads whatever is new at path since the last call and returns the
// events it produced, in file order. Once tailing has stopped (a Stopped
// event was ever produced), Poll is a no-op returning nil.
func (r *Reader) Poll(path string) []Event {
	if r.stopped || r.capped {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		r.stopped = true
		return []Event{{Stopped: true, Reason: "plan.jsonl unreadable: " + err.Error()}}
	}
	if info.Size() < r.offset {
		r.stopped = true
		return []Event{{Stopped: true, Reason: "plan.jsonl truncated below the held offset"}}
	}
	if info.Size() == r.offset {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		r.stopped = true
		return []Event{{Stopped: true, Reason: "plan.jsonl unreadable: " + err.Error()}}
	}
	defer f.Close()
	if _, err := f.Seek(r.offset, os.SEEK_SET); err != nil {
		r.stopped = true
		return []Event{{Stopped: true, Reason: "plan.jsonl unreadable: " + err.Error()}}
	}
	var events []Event
	buf := make([]byte, 32*1024)
	for !r.capped {
		n, readErr := f.Read(buf)
		if n > 0 {
			r.offset += int64(n)
			events = append(events, r.consume(buf[:n])...)
		}
		if readErr != nil {
			if readErr != io.EOF {
				r.stopped = true
				events = append(events, Event{Stopped: true, Reason: "plan.jsonl unreadable: " + readErr.Error()})
			}
			break
		}
	}
	return events
}

// Final does one last Poll (to catch anything written between the process's
// exit and this call) and then resolves whatever partial line remains: a
// non-empty, non-terminated fragment is one rejection ("truncated final
// line"), since a line lacking its newline was never actually finished by
// the writer. It also appends the one suppressed-rejections summary event
// when more lines were rejected than were individually diagnosed.
func (r *Reader) Final(path string) []Event {
	if r.stopped {
		return nil
	}
	var events []Event
	if !r.capped {
		events = r.Poll(path)
	}
	if len(r.buf) > 0 && !r.skipping {
		r.lineNum++
		events = append(events, r.reject("truncated final line"))
		r.buf = nil
	}
	r.skipping = false
	if r.rejectedN > r.diagnosedN {
		events = append(events, Event{
			Summary: true,
			Reason:  fmt.Sprintf("%d further rejected lines suppressed", r.rejectedN-r.diagnosedN),
		})
	}
	return events
}

func (r *Reader) consume(data []byte) []Event {
	var events []Event
	for {
		nl := bytes.IndexByte(data, '\n')
		if nl == -1 {
			if !r.skipping {
				r.buf = append(r.buf, data...)
				if len(r.buf) > MaxLineBytes {
					r.lineNum++
					events = append(events, r.reject("line too long"))
					r.buf = nil
					r.skipping = true
				}
			}
			return events
		}
		line := data[:nl]
		data = data[nl+1:]

		if r.skipping {
			r.skipping = false
			continue
		}

		full := line
		if len(r.buf) > 0 {
			full = append(r.buf, line...)
			r.buf = nil
		}
		r.lineNum++

		if r.acceptedN >= MaxRevisions {
			continue
		}

		if len(full)+1 > MaxLineBytes {
			events = append(events, r.reject("line too long"))
			continue
		}

		rev, reason := Validate(full, r.lastRev)
		if reason == "" {
			r.lastRev = rev.Rev
			r.lastAccepted = &rev
			r.acceptedN++
			events = append(events, Event{Accepted: true, Revision: rev, Line: r.lineNum})
			if r.acceptedN == MaxRevisions {
				r.capped = true
				return events
			}
		} else {
			events = append(events, r.reject(reason))
		}
	}
}

func (r *Reader) reject(reason string) Event {
	r.rejectedN++
	diagnose := r.diagnosedN < maxDiagnostics
	if diagnose {
		r.diagnosedN++
	}
	return Event{Reason: reason, Line: r.lineNum, Diagnose: diagnose}
}

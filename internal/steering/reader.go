package steering

import (
	"fmt"

	"github.com/AmirRaptoR/Conveyor/internal/linefeed"
)

// maxDiagnostics bounds how many rejection events in one run are worth an
// engine-stream log line each, mirroring internal/plan's own cap.
const maxDiagnostics = 10

// Event is one thing a reader noticed on either channel: an accepted
// record, a rejected line, a stopped-tailing notice, or the end-of-run
// suppressed-rejection summary — the same shape internal/plan.Event uses,
// generalised over which kind of record a Reader accepts.
type CommandEvent struct {
	Accepted bool
	Record   ControlRecord
	Line     int
	Reason   string
	Diagnose bool
	Summary  bool
	Stopped  bool
}

type AckEvent struct {
	Accepted bool
	Record   AckRecord
	Line     int
	Reason   string
	Diagnose bool
	Summary  bool
	Stopped  bool
}

// CommandReader tails one run's control.jsonl.
type CommandReader struct {
	offset   int64
	buf      []byte
	skipping bool

	lastSeq    int
	acceptedN  int
	rejectedN  int
	diagnosedN int
	lineNum    int
	stopped    bool
	capped     bool
	commandIDs map[string]bool
}

func NewCommandReader() *CommandReader { return &CommandReader{commandIDs: map[string]bool{}} }

func (r *CommandReader) Accepted() int { return r.acceptedN }
func (r *CommandReader) Rejected() int { return r.rejectedN }
func (r *CommandReader) MaxSeq() int   { return r.lastSeq }
func (r *CommandReader) Lines() int    { return r.lineNum }

func (r *CommandReader) Poll(path string) []CommandEvent {
	if r.stopped || r.capped {
		return nil
	}
	var events []CommandEvent
	cur := linefeed.Cursor{Offset: r.offset, Buf: r.buf, Skipping: r.skipping}
	stopped, reason := linefeed.Poll(&cur, path, MaxLineBytes, func(data []byte, oversize bool) bool {
		r.lineNum++
		if oversize {
			events = append(events, r.reject("line too long"))
			return r.capReached()
		}
		rec, seq, reason := ValidateCommandLine(data, r.lastSeq)
		if seq > r.lastSeq {
			r.lastSeq = seq
		}
		if reason != "" {
			events = append(events, r.reject(reason))
			return r.capReached()
		}
		if rec.Command != nil && r.commandIDs[rec.Command.ID] {
			events = append(events, r.reject("command id is not unique"))
			return r.capReached()
		}
		if rec.Command != nil {
			r.commandIDs[rec.Command.ID] = true
		}
		r.acceptedN++
		events = append(events, CommandEvent{Accepted: true, Record: rec, Line: r.lineNum})
		return r.capReached()
	})
	r.offset, r.buf, r.skipping = cur.Offset, cur.Buf, cur.Skipping
	if stopped {
		r.stopped = true
		events = append(events, CommandEvent{Stopped: true, Reason: "control.jsonl " + reason})
	}
	return events
}

func (r *CommandReader) capReached() bool {
	if r.lineNum >= MaxCommands {
		r.capped = true
	}
	return r.capped
}

func (r *CommandReader) Final(path string) []CommandEvent {
	var events []CommandEvent
	if !r.stopped {
		if !r.capped {
			events = r.Poll(path)
		}
		cur := linefeed.Cursor{Offset: r.offset, Buf: r.buf, Skipping: r.skipping}
		if _, truncated := linefeed.Final(&cur); truncated {
			r.lineNum++
			events = append(events, r.reject("truncated final line"))
		}
		r.offset, r.buf, r.skipping = cur.Offset, cur.Buf, cur.Skipping
	}
	if r.rejectedN > r.diagnosedN {
		events = append(events, CommandEvent{Summary: true, Reason: fmt.Sprintf("%d further rejected lines suppressed", r.rejectedN-r.diagnosedN)})
	}
	return events
}

func (r *CommandReader) reject(reason string) CommandEvent {
	r.rejectedN++
	diagnose := r.diagnosedN < maxDiagnostics
	if diagnose {
		r.diagnosedN++
	}
	return CommandEvent{Reason: reason, Line: r.lineNum, Diagnose: diagnose}
}

// AckReader tails one run's control-ack.jsonl.
type AckReader struct {
	offset   int64
	buf      []byte
	skipping bool

	lastSeq    int
	acceptedN  int
	rejectedN  int
	diagnosedN int
	lineNum    int
	stopped    bool
	capped     bool
}

func NewAckReader() *AckReader { return &AckReader{} }

func (r *AckReader) Accepted() int { return r.acceptedN }
func (r *AckReader) Rejected() int { return r.rejectedN }
func (r *AckReader) MaxSeq() int   { return r.lastSeq }
func (r *AckReader) Lines() int    { return r.lineNum }

func (r *AckReader) Poll(path string) []AckEvent {
	if r.stopped || r.capped {
		return nil
	}
	var events []AckEvent
	cur := linefeed.Cursor{Offset: r.offset, Buf: r.buf, Skipping: r.skipping}
	stopped, reason := linefeed.Poll(&cur, path, MaxLineBytes, func(data []byte, oversize bool) bool {
		r.lineNum++
		if oversize {
			events = append(events, r.reject("line too long"))
			return r.capReached()
		}
		rec, seq, reason := ValidateAckLine(data, r.lastSeq)
		if seq > r.lastSeq {
			r.lastSeq = seq
		}
		if reason != "" {
			events = append(events, r.reject(reason))
			return r.capReached()
		}
		r.acceptedN++
		events = append(events, AckEvent{Accepted: true, Record: rec, Line: r.lineNum})
		return r.capReached()
	})
	r.offset, r.buf, r.skipping = cur.Offset, cur.Buf, cur.Skipping
	if stopped {
		r.stopped = true
		events = append(events, AckEvent{Stopped: true, Reason: "control-ack.jsonl " + reason})
	}
	return events
}

func (r *AckReader) capReached() bool {
	if r.lineNum >= MaxAckLines {
		r.capped = true
	}
	return r.capped
}

func (r *AckReader) Final(path string) []AckEvent {
	var events []AckEvent
	if !r.stopped {
		if !r.capped {
			events = r.Poll(path)
		}
		cur := linefeed.Cursor{Offset: r.offset, Buf: r.buf, Skipping: r.skipping}
		if _, truncated := linefeed.Final(&cur); truncated {
			r.lineNum++
			events = append(events, r.reject("truncated final line"))
		}
		r.offset, r.buf, r.skipping = cur.Offset, cur.Buf, cur.Skipping
	}
	if r.rejectedN > r.diagnosedN {
		events = append(events, AckEvent{Summary: true, Reason: fmt.Sprintf("%d further rejected lines suppressed", r.rejectedN-r.diagnosedN)})
	}
	return events
}

func (r *AckReader) reject(reason string) AckEvent {
	r.rejectedN++
	diagnose := r.diagnosedN < maxDiagnostics
	if diagnose {
		r.diagnosedN++
	}
	return AckEvent{Reason: reason, Line: r.lineNum, Diagnose: diagnose}
}

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
}

func NewCommandReader() *CommandReader { return &CommandReader{} }

func (r *CommandReader) Accepted() int { return r.acceptedN }
func (r *CommandReader) Rejected() int { return r.rejectedN }

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
			return r.capped
		}
		if r.acceptedN >= MaxCommands {
			return true
		}
		rec, _, reason := ValidateCommandLine(data, r.lastSeq)
		if reason != "" {
			events = append(events, r.reject(reason))
			return r.capped
		}
		r.lastSeq = seqOf(rec, r.lastSeq)
		r.acceptedN++
		events = append(events, CommandEvent{Accepted: true, Record: rec, Line: r.lineNum})
		if r.acceptedN == MaxCommands {
			r.capped = true
			return true
		}
		return false
	})
	r.offset, r.buf, r.skipping = cur.Offset, cur.Buf, cur.Skipping
	if stopped {
		r.stopped = true
		events = append(events, CommandEvent{Stopped: true, Reason: "control.jsonl " + reason})
	}
	return events
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

func seqOf(rec ControlRecord, fallback int) int {
	switch {
	case rec.Command != nil:
		return rec.Command.Seq
	case rec.Carried != nil:
		return rec.Carried.Seq
	default:
		return fallback
	}
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
			return r.capped
		}
		if r.acceptedN >= MaxAckLines {
			return true
		}
		rec, seq, reason := ValidateAckLine(data, r.lastSeq)
		if reason != "" {
			events = append(events, r.reject(reason))
			return r.capped
		}
		r.lastSeq = seq
		r.acceptedN++
		events = append(events, AckEvent{Accepted: true, Record: rec, Line: r.lineNum})
		if r.acceptedN == MaxAckLines {
			r.capped = true
			return true
		}
		return false
	})
	r.offset, r.buf, r.skipping = cur.Offset, cur.Buf, cur.Skipping
	if stopped {
		r.stopped = true
		events = append(events, AckEvent{Stopped: true, Reason: "control-ack.jsonl " + reason})
	}
	return events
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
	if r.rejectedN == MaxAckLines {
		r.capped = true
	}
	return AckEvent{Reason: reason, Line: r.lineNum, Diagnose: diagnose}
}

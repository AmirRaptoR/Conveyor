package steering

import "path/filepath"

// Load reads one run's control.jsonl and control-ack.jsonl in full — at
// most MaxCommands accepted commands and MaxAckLines accepted ack lines,
// the same bounds the writers enforce — and returns the resolved view:
// what the run advertised, its session, and every command's derived
// state. runEnded should be true once the run itself has finished, so any
// command still queued resolves to rejected: run ended rather than hanging
// there forever; a caller reading a run still in flight passes false.
//
// This is the disk-backed counterpart to Resolve, the way
// internal/plan/server.go's loadRunPlan is to plan.Reader — used by
// GET /api/runs/{id} and by restart recovery, neither of which is wired to
// call it yet.
func Load(dir string, runEnded bool) Resolution {
	ackPath := filepath.Join(dir, "control-ack.jsonl")

	var acks []AckRecord
	ar := NewAckReader()
	for _, ev := range ar.Final(ackPath) {
		if ev.Accepted {
			acks = append(acks, ev.Record)
		}
	}

	return LoadWithAcks(dir, acks, ar.Rejected(), ar.MaxSeq(), ar.Lines(), runEnded)
}

// LoadWithAcks resolves control.jsonl against ack records an already-live
// reader retained. It is what the runner uses after control-ack.jsonl is
// removed, truncated or unreadable: tailing stops, but accepted records from
// before that fault remain true and must not disappear from the final state.
func LoadWithAcks(dir string, acks []AckRecord, ackRejected, ackMaxSeq, ackVersion int, runEnded bool) Resolution {
	ctlPath := filepath.Join(dir, "control.jsonl")
	var commands []Command
	var carried []Carried
	cr := NewCommandReader()
	for _, ev := range cr.Final(ctlPath) {
		if !ev.Accepted {
			continue
		}
		switch {
		case ev.Record.Command != nil:
			commands = append(commands, *ev.Record.Command)
		case ev.Record.Carried != nil:
			carried = append(carried, *ev.Record.Carried)
		}
	}
	res := Resolve(commands, carried, acks, runEnded)
	res.Malformed = cr.Rejected() + ackRejected
	res.MaxSeq = cr.MaxSeq()
	res.AckSeq = ackMaxSeq
	res.AckVersion = ackVersion
	return res
}

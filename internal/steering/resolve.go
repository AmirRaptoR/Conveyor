package steering

// StateQueued is a command's state before any ack names it.
const StateQueued = "queued"

// CommandState is one command from control.jsonl plus its derived
// resolution — never stored twice: the state comes from the first ack that
// names it, a carried record, or (when the run has ended) the engine's own
// "run ended" derivation, and nowhere else.
type CommandState struct {
	Command Command
	State   string
	Reason  string
}

// Resolution is the resolved view of one run's steering channel, built from
// the accepted records of both its files.
type Resolution struct {
	// Accepts and Session come from the run's hello/session ack records.
	Accepts []string
	Session string
	Hello   bool
	// Commands is every command from control.jsonl, in seq order, each with
	// its derived State.
	Commands []CommandState
	// OrphanAcks counts ack records naming a command id this run's
	// control.jsonl never carried.
	OrphanAcks int
	// DuplicateAcks counts a second ack naming an id already resolved.
	DuplicateAcks int
	// IgnoredHellos counts a second (or later) hello record.
	IgnoredHellos int
	// Malformed is the number of rejected physical lines across both files.
	Malformed int
	// MaxSeq and AckSeq are the highest consumed sequence numbers in the
	// engine-written and script-written files. AckVersion advances for every
	// physical ack line, including one too malformed to carry a seq.
	MaxSeq     int
	AckSeq     int
	AckVersion int
}

// Resolve derives every command's state from the accepted ack records
// tailed from the same run's control-ack.jsonl and any carried records
// appended to control.jsonl by restart carry-over, applying the protocol's
// resolution rules in order: first-ack-wins, an orphan ack is ignored and
// counted, a second hello cannot shrink the advertised set, an empty
// session record is ignored while a non-empty one replaces the current
// session, a carried entry resolves a command carry-over touched, and —
// when runEnded is true — every command still unresolved after all of that
// is StateRejected with reason "run ended", so nothing is left queued
// against a run that can never produce another ack.
//
// commands, carriedRecords and acks must each be in the file order their
// reader produced them (ascending seq).
func Resolve(commands []Command, carriedRecords []Carried, acks []AckRecord, runEnded bool) Resolution {
	res := Resolution{}
	states := make(map[string]*CommandState, len(commands))
	order := make([]string, 0, len(commands))
	for _, c := range commands {
		cs := CommandState{Command: c, State: StateQueued}
		states[c.ID] = &cs
		order = append(order, c.ID)
	}

	helloSeen := false
	for _, a := range acks {
		switch {
		case a.Hello != nil:
			if helloSeen {
				res.IgnoredHellos++
				continue
			}
			helloSeen = true
			res.Hello = true
			res.Accepts = a.Hello.Accepts
			if a.Hello.Session != "" {
				res.Session = a.Hello.Session
			}
		case a.Session != nil:
			if a.Session.Session == "" {
				continue
			}
			previous := res.Session
			res.Session = a.Session.Session
			if previous != "" && previous != res.Session {
				for _, id := range order {
					cs := states[id]
					if cs.State == StateQueued && cs.Command.Session == previous {
						cs.State = StateRejected
						cs.Reason = "stale session"
					}
				}
			}
		case a.Ack != nil:
			cs, ok := states[a.Ack.ID]
			if !ok {
				res.OrphanAcks++
				continue
			}
			if cs.State != StateQueued {
				res.DuplicateAcks++
				continue
			}
			cs.State = a.Ack.State
			cs.Reason = a.Ack.Reason
		}
	}

	for _, cr := range carriedRecords {
		for _, e := range cr.Entries {
			cs, ok := states[e.ID]
			if !ok || cs.State != StateQueued {
				continue
			}
			cs.State = e.State
			cs.Reason = e.Reason
		}
	}

	if runEnded {
		for _, id := range order {
			cs := states[id]
			if cs.State == StateQueued {
				cs.State = StateRejected
				cs.Reason = "run ended"
			}
		}
	}

	res.Commands = make([]CommandState, 0, len(order))
	for _, id := range order {
		res.Commands = append(res.Commands, *states[id])
	}
	return res
}

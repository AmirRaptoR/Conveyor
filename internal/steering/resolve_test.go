package steering

import "testing"

func cmd(id string) Command { return Command{ID: id, Kind: KindInstruction} }

func TestResolveFirstAckWins(t *testing.T) {
	commands := []Command{cmd("c1")}
	acks := []AckRecord{
		{Ack: &AckAck{ID: "c1", State: StateConsumed, Reason: ""}},
		{Ack: &AckAck{ID: "c1", State: StateRejected, Reason: "changed my mind"}},
	}
	res := Resolve(commands, nil, acks, false)
	if res.DuplicateAcks != 1 {
		t.Fatalf("expected 1 duplicate ack, got %d", res.DuplicateAcks)
	}
	if res.Commands[0].State != StateConsumed {
		t.Fatalf("expected first ack to win, got %+v", res.Commands[0])
	}
}

func TestResolveOrphanAck(t *testing.T) {
	acks := []AckRecord{{Ack: &AckAck{ID: "ghost", State: StateConsumed}}}
	res := Resolve(nil, nil, acks, false)
	if res.OrphanAcks != 1 {
		t.Fatalf("expected 1 orphan ack, got %d", res.OrphanAcks)
	}
}

func TestResolveSecondHelloIgnored(t *testing.T) {
	acks := []AckRecord{
		{Hello: &AckHello{Accepts: []string{KindInstruction}}},
		{Hello: &AckHello{Accepts: []string{}}},
	}
	res := Resolve(nil, nil, acks, false)
	if res.IgnoredHellos != 1 {
		t.Fatalf("expected 1 ignored hello, got %d", res.IgnoredHellos)
	}
	if len(res.Accepts) != 1 || res.Accepts[0] != KindInstruction {
		t.Fatalf("expected the first hello's accepts to stand, got %+v", res.Accepts)
	}
}

func TestResolveSessionEmptyIgnoredNonEmptyReplaces(t *testing.T) {
	acks := []AckRecord{
		{Hello: &AckHello{Session: "s1"}},
		{Session: &AckSession{Session: ""}},
		{Session: &AckSession{Session: "s2"}},
	}
	res := Resolve(nil, nil, acks, false)
	if res.Session != "s2" {
		t.Fatalf("expected session s2, got %q", res.Session)
	}
}

func TestResolveRunEndedRejectsUnresolved(t *testing.T) {
	commands := []Command{cmd("c1"), cmd("c2")}
	acks := []AckRecord{{Ack: &AckAck{ID: "c1", State: StateConsumed}}}
	res := Resolve(commands, nil, acks, true)
	if res.Commands[0].State != StateConsumed {
		t.Fatalf("expected c1 to stay consumed, got %+v", res.Commands[0])
	}
	if res.Commands[1].State != StateRejected || res.Commands[1].Reason != "run ended" {
		t.Fatalf("expected c2 rejected as run ended, got %+v", res.Commands[1])
	}
}

func TestResolveNotRunEndedLeavesQueued(t *testing.T) {
	commands := []Command{cmd("c1")}
	res := Resolve(commands, nil, nil, false)
	if res.Commands[0].State != StateQueued {
		t.Fatalf("expected c1 still queued, got %+v", res.Commands[0])
	}
}

func TestResolveCarriedEntryResolvesCommand(t *testing.T) {
	commands := []Command{cmd("c1"), cmd("c2")}
	carried := []Carried{{Entries: []CarriedEntry{
		{ID: "c1", State: StateCarried, Reason: "moved to next run"},
		{ID: "c2", State: StateDropped, Reason: "pause left queued"},
	}}}
	res := Resolve(commands, carried, nil, true)
	if res.Commands[0].State != StateCarried || res.Commands[1].State != StateDropped {
		t.Fatalf("unexpected states: %+v", res.Commands)
	}
}

func TestResolveCarriedNeverOverwritesAlreadyResolved(t *testing.T) {
	commands := []Command{cmd("c1")}
	acks := []AckRecord{{Ack: &AckAck{ID: "c1", State: StateConsumed}}}
	carried := []Carried{{Entries: []CarriedEntry{{ID: "c1", State: StateCarried}}}}
	res := Resolve(commands, carried, acks, false)
	if res.Commands[0].State != StateConsumed {
		t.Fatalf("expected the ack's resolution to stand, got %+v", res.Commands[0])
	}
}

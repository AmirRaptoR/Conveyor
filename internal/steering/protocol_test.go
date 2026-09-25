package steering

import (
	"strings"
	"testing"
)

func TestValidateCommandLineAccepts(t *testing.T) {
	line := []byte(`{"v":1,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"do x","itemId":"i1","runId":"r1","session":"s1","by":"amir"}`)
	rec, seq, reason := ValidateCommandLine(line, 0)
	if reason != "" {
		t.Fatalf("expected accept, got reason %q", reason)
	}
	if seq != 1 || rec.Command == nil {
		t.Fatalf("expected command with seq 1, got %+v seq=%d", rec, seq)
	}
	if rec.Command.Kind != KindInstruction || rec.Command.Text != "do x" {
		t.Fatalf("unexpected command %+v", rec.Command)
	}
}

func TestValidateCommandLineCarriedRecord(t *testing.T) {
	line := []byte(`{"v":1,"type":"carried","seq":1,"at":"2026-09-24T12:00:00Z","entries":[{"id":"c1","state":"carried","reason":""}]}`)
	rec, seq, reason := ValidateCommandLine(line, 0)
	if reason != "" {
		t.Fatalf("expected accept, got %q", reason)
	}
	if seq != 1 || rec.Carried == nil || len(rec.Carried.Entries) != 1 {
		t.Fatalf("unexpected %+v", rec)
	}
	if rec.Carried.Entries[0].State != StateCarried {
		t.Fatalf("unexpected entry %+v", rec.Carried.Entries[0])
	}
}

func TestValidateCommandLineUnknownTypeAcceptedButEmpty(t *testing.T) {
	line := []byte(`{"v":1,"type":"future-thing","seq":1,"at":"2026-09-24T12:00:00Z","whatever":true}`)
	rec, seq, reason := ValidateCommandLine(line, 0)
	if reason != "" {
		t.Fatalf("expected an unknown type to be accepted, got %q", reason)
	}
	if seq != 1 || rec.Command != nil || rec.Carried != nil {
		t.Fatalf("expected an empty record consuming seq 1, got %+v seq=%d", rec, seq)
	}
}

func TestValidateCommandLineUnknownVRejected(t *testing.T) {
	line := []byte(`{"v":2,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"x","itemId":"i","runId":"r","session":"","by":""}`)
	_, _, reason := ValidateCommandLine(line, 0)
	if reason != "unknown v" {
		t.Fatalf("expected unknown v rejection, got %q", reason)
	}
}

func TestValidateCommandLineSeqNotIncreasing(t *testing.T) {
	line := []byte(`{"v":1,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"x","itemId":"i","runId":"r","session":"","by":""}`)
	_, _, reason := ValidateCommandLine(line, 1)
	if reason != "seq is not increasing" {
		t.Fatalf("expected seq rejection, got %q", reason)
	}
}

func TestValidateCommandLineBadKind(t *testing.T) {
	line := []byte(`{"v":1,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"nonsense","text":"x","itemId":"i","runId":"r","session":"","by":""}`)
	_, _, reason := ValidateCommandLine(line, 0)
	if reason != "kind is not valid" {
		t.Fatalf("expected kind rejection, got %q", reason)
	}
}

func TestValidateCommandLineMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"missing id", `{"v":1,"type":"command","seq":1,"at":"2026-09-24T12:00:00Z","kind":"instruction","text":"x","itemId":"i","runId":"r","session":"","by":""}`},
		{"missing itemId", `{"v":1,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"x","runId":"r","session":"","by":""}`},
		{"missing runId", `{"v":1,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"x","itemId":"i","session":"","by":""}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, reason := ValidateCommandLine([]byte(c.line), 0)
			if reason == "" {
				t.Fatalf("expected a rejection")
			}
		})
	}
}

func TestValidateCommandLineTextTooLong(t *testing.T) {
	long := strings.Repeat("a", MaxTextLen+1)
	line := []byte(`{"v":1,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"` + long + `","itemId":"i","runId":"r","session":"","by":""}`)
	_, _, reason := ValidateCommandLine(line, 0)
	if reason != "text too long" {
		t.Fatalf("expected text too long, got %q", reason)
	}
}

func TestValidateCommandLineFieldTooLong(t *testing.T) {
	long := strings.Repeat("a", MaxFieldLen+1)
	line := []byte(`{"v":1,"type":"command","seq":1,"id":"` + long + `","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"x","itemId":"i","runId":"r","session":"","by":""}`)
	_, _, reason := ValidateCommandLine(line, 0)
	if reason != "id too long" {
		t.Fatalf("expected id too long, got %q", reason)
	}
}

func TestValidateCommandLineNotJSON(t *testing.T) {
	_, _, reason := ValidateCommandLine([]byte("not json"), 0)
	if reason != "not valid JSON" {
		t.Fatalf("expected not valid JSON, got %q", reason)
	}
}

func TestValidateCommandLineLargestLegalLineAccepted(t *testing.T) {
	text := strings.Repeat("a", MaxTextLen)
	field := strings.Repeat("b", MaxFieldLen)
	line := []byte(`{"v":1,"type":"command","seq":1,"id":"` + field + `","at":"2026-09-24T12:00:00Z","kind":"instruction","text":"` + text + `","itemId":"` + field + `","runId":"` + field + `","session":"` + field + `","by":"` + field + `"}`)
	if len(line)+1 > MaxLineBytes {
		t.Fatalf("test line itself exceeds MaxLineBytes: %d", len(line))
	}
	_, _, reason := ValidateCommandLine(line, 0)
	if reason != "" {
		t.Fatalf("expected the largest legal command accepted, got %q", reason)
	}
}

func TestValidateAckLineHello(t *testing.T) {
	line := []byte(`{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":["instruction","pause"],"session":""}`)
	rec, seq, reason := ValidateAckLine(line, 0)
	if reason != "" {
		t.Fatalf("expected accept, got %q", reason)
	}
	if seq != 1 || rec.Hello == nil || len(rec.Hello.Accepts) != 2 {
		t.Fatalf("unexpected %+v", rec)
	}
}

func TestValidateAckLineHelloIgnoresUnknownAcceptsMember(t *testing.T) {
	line := []byte(`{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":["instruction","teleport"],"session":""}`)
	rec, _, reason := ValidateAckLine(line, 0)
	if reason != "" {
		t.Fatalf("expected accept, got %q", reason)
	}
	if len(rec.Hello.Accepts) != 1 || rec.Hello.Accepts[0] != KindInstruction {
		t.Fatalf("expected unknown accepts member ignored, got %+v", rec.Hello.Accepts)
	}
}

func TestValidateAckLineSession(t *testing.T) {
	line := []byte(`{"v":1,"type":"session","seq":1,"at":"2026-09-24T12:00:00Z","session":"abc"}`)
	rec, _, reason := ValidateAckLine(line, 0)
	if reason != "" {
		t.Fatalf("expected accept, got %q", reason)
	}
	if rec.Session == nil || rec.Session.Session != "abc" {
		t.Fatalf("unexpected %+v", rec)
	}
}

func TestValidateAckLineAck(t *testing.T) {
	line := []byte(`{"v":1,"type":"ack","seq":1,"at":"2026-09-24T12:00:00Z","id":"c1","state":"consumed","reason":""}`)
	rec, _, reason := ValidateAckLine(line, 0)
	if reason != "" {
		t.Fatalf("expected accept, got %q", reason)
	}
	if rec.Ack == nil || rec.Ack.State != StateConsumed {
		t.Fatalf("unexpected %+v", rec)
	}
}

func TestValidateAckLineBadState(t *testing.T) {
	line := []byte(`{"v":1,"type":"ack","seq":1,"at":"2026-09-24T12:00:00Z","id":"c1","state":"maybe","reason":""}`)
	_, _, reason := ValidateAckLine(line, 0)
	if reason != "state is not valid" {
		t.Fatalf("expected state rejection, got %q", reason)
	}
}

func TestValidateAckLineReasonTooLong(t *testing.T) {
	long := strings.Repeat("a", MaxReasonLen+1)
	line := []byte(`{"v":1,"type":"ack","seq":1,"at":"2026-09-24T12:00:00Z","id":"c1","state":"rejected","reason":"` + long + `"}`)
	_, _, reason := ValidateAckLine(line, 0)
	if reason != "reason too long" {
		t.Fatalf("expected reason too long, got %q", reason)
	}
}

func TestValidateAckLineUnknownTypeAcceptedButEmpty(t *testing.T) {
	line := []byte(`{"v":1,"type":"future","seq":1,"at":"2026-09-24T12:00:00Z"}`)
	rec, seq, reason := ValidateAckLine(line, 0)
	if reason != "" {
		t.Fatalf("expected accept, got %q", reason)
	}
	if seq != 1 || rec.Hello != nil || rec.Session != nil || rec.Ack != nil {
		t.Fatalf("expected an empty record consuming seq 1, got %+v", rec)
	}
}

func TestValidateAckLineSeqNotIncreasing(t *testing.T) {
	line := []byte(`{"v":1,"type":"session","seq":3,"at":"2026-09-24T12:00:00Z","session":"a"}`)
	_, _, reason := ValidateAckLine(line, 5)
	if reason != "seq is not increasing" {
		t.Fatalf("expected seq rejection, got %q", reason)
	}
}

func TestSeqMustStartExactlyAtOne(t *testing.T) {
	line := []byte(`{"v":1,"type":"future","seq":2,"at":"2026-09-24T12:00:00Z"}`)
	_, seq, reason := ValidateAckLine(line, 0)
	if seq != 2 || reason != "seq must start at 1" {
		t.Fatalf("seq=%d reason=%q", seq, reason)
	}
}

func TestStrictArraysAndSets(t *testing.T) {
	for _, line := range []string{
		`{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":null,"session":""}`,
		`{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":["pause","pause"],"session":""}`,
	} {
		if _, _, reason := ValidateAckLine([]byte(line), 0); reason == "" {
			t.Fatalf("accepted %s", line)
		}
	}
	for _, line := range []string{
		`{"v":1,"type":"carried","seq":1,"at":"2026-09-24T12:00:00Z","entries":null}`,
		`{"v":1,"type":"carried","seq":1,"at":"2026-09-24T12:00:00Z","entries":[{"id":"c","state":"carried","reason":""},{"id":"c","state":"dropped","reason":""}]}`,
	} {
		if _, _, reason := ValidateCommandLine([]byte(line), 0); reason == "" {
			t.Fatalf("accepted %s", line)
		}
	}
}

func TestPauseTextMustBeEmpty(t *testing.T) {
	line := []byte(`{"v":1,"type":"command","seq":1,"id":"c1","at":"2026-09-24T12:00:00Z","kind":"pause","text":"no","itemId":"i","runId":"r","session":"","by":""}`)
	if _, _, reason := ValidateCommandLine(line, 0); reason != "pause text must be empty" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestProtocolRequiresPresentNullableLookingFields(t *testing.T) {
	for _, line := range []string{
		`{"v":1,"type":"command","seq":1,"id":"c","at":"2026-09-24T12:00:00Z","kind":"pause","itemId":"i","runId":"r","session":"","by":""}`,
		`{"v":1,"type":"command","seq":1,"id":"c","at":"2026-09-24T12:00:00Z","kind":"pause","text":"","itemId":"i","runId":"r","by":""}`,
		`{"v":1,"type":"command","seq":1,"id":"c","at":"2026-09-24T12:00:00Z","kind":"pause","text":"","itemId":"i","runId":"r","session":""}`,
	} {
		if _, _, reason := ValidateCommandLine([]byte(line), 0); reason == "" {
			t.Fatalf("accepted command with a missing required field: %s", line)
		}
	}
	for _, line := range []string{
		`{"v":1,"type":"hello","seq":1,"at":"2026-09-24T12:00:00Z","accepts":[]}`,
		`{"v":1,"type":"ack","seq":1,"at":"2026-09-24T12:00:00Z","id":"c","state":"consumed"}`,
	} {
		if _, _, reason := ValidateAckLine([]byte(line), 0); reason == "" {
			t.Fatalf("accepted ack with a missing required field: %s", line)
		}
	}
}

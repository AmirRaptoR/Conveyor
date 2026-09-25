package plan

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateAccepts(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"do it","status":"pending"}]}`
	rev, reason := Validate([]byte(line), 0)
	if reason != "" {
		t.Fatalf("expected accept, got reason %q", reason)
	}
	if rev.Rev != 1 || rev.V != 1 || len(rev.Todos) != 1 {
		t.Fatalf("unexpected revision: %+v", rev)
	}
	if rev.Todos[0].ID != "a" || rev.Todos[0].Text != "do it" || rev.Todos[0].Status != "pending" {
		t.Fatalf("unexpected todo: %+v", rev.Todos[0])
	}
}

func TestValidateEmptyTodosAllowed(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`
	_, reason := Validate([]byte(line), 0)
	if reason != "" {
		t.Fatalf("expected accept, got reason %q", reason)
	}
}

func TestValidateUnknownFieldsIgnored(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"x","status":"pending","future":"field"}],"extra":true}`
	_, reason := Validate([]byte(line), 0)
	if reason != "" {
		t.Fatalf("expected accept, got reason %q", reason)
	}
}

func TestValidateRevNotIncreasing(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`
	_, reason := Validate([]byte(line), 1)
	if reason == "" {
		t.Fatalf("expected rejection for non-increasing rev")
	}
}

func TestValidateRevZeroInvalid(t *testing.T) {
	line := `{"v":1,"rev":0,"at":"2026-09-24T12:00:00Z","todos":[]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for rev < 1")
	}
}

func TestValidateNotJSON(t *testing.T) {
	_, reason := Validate([]byte(`not json`), 0)
	if reason == "" {
		t.Fatalf("expected rejection for invalid json")
	}
}

func TestValidateNotObject(t *testing.T) {
	_, reason := Validate([]byte(`[1,2,3]`), 0)
	if reason == "" {
		t.Fatalf("expected rejection for non-object")
	}
}

func TestValidateUnknownVersion(t *testing.T) {
	line := `{"v":2,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for unknown v")
	}
}

func TestValidateMissingV(t *testing.T) {
	line := `{"rev":1,"at":"2026-09-24T12:00:00Z","todos":[]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for missing v")
	}
}

func TestValidateBadAt(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"not-a-time","todos":[]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for bad at")
	}
}

func TestValidateTodosNotArray(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":"nope"}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for todos not array")
	}
}

func TestValidateTodoMissingID(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"text":"x","status":"pending"}]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for missing id")
	}
}

func TestValidateTodoEmptyTextAfterTrim(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"   ","status":"pending"}]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for empty text")
	}
}

func TestValidateTodoBadStatus(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"x","status":"done"}]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for bad status")
	}
}

func TestValidateDuplicateIDs(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"x","status":"pending"},{"id":"a","text":"y","status":"pending"}]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for duplicate ids")
	}
}

func TestValidateTooManyTodos(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[`)
	for i := 0; i < 201; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":"id%d","text":"x","status":"pending"}`, i)
	}
	sb.WriteString(`]}`)
	_, reason := Validate([]byte(sb.String()), 0)
	if reason == "" {
		t.Fatalf("expected rejection for too many todos")
	}
}

func TestValidateTextTooLong(t *testing.T) {
	longText := strings.Repeat("a", 501)
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"` + longText + `","status":"pending"}]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for text too long")
	}
}

func TestValidateIDTooLong(t *testing.T) {
	longID := strings.Repeat("a", 65)
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"` + longID + `","text":"x","status":"pending"}]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for id too long")
	}
}

func TestValidateLineTooLong(t *testing.T) {
	longText := strings.Repeat("a", MaxLineBytes)
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"` + longText + `","status":"pending"}]}`
	_, reason := Validate([]byte(line), 0)
	if reason == "" {
		t.Fatalf("expected rejection for oversized line")
	}
}

func TestValidateActiveMapsAndOmitted(t *testing.T) {
	line := `{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"x","status":"in_progress","active":"doing x"}]}`
	rev, reason := Validate([]byte(line), 0)
	if reason != "" {
		t.Fatalf("expected accept, got %q", reason)
	}
	if rev.Todos[0].Active != "doing x" {
		t.Fatalf("expected active to be set, got %+v", rev.Todos[0])
	}
}

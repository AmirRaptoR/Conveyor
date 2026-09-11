package store

import (
	"path/filepath"
	"testing"
	"time"
)

// A manual pause is the one pause a restart must not forget: it is a person's
// decision, not a fact an agent's status script will say again on its own.
func TestPausesSurviveAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pauses.json")
	p := OpenPauses(path)

	at := time.Now().Truncate(time.Second)
	if err := p.Set("", ManualPause{Reason: "deploy freeze", By: "amir", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := p.Set("s1", ManualPause{Reason: "flaky provider", By: "amir", At: at}); err != nil {
		t.Fatal(err)
	}

	reopened := OpenPauses(path)
	got, ok := reopened.Get("")
	if !ok || got.Reason != "deploy freeze" || got.By != "amir" || !got.At.Equal(at) {
		t.Errorf("global pause after reopen = %+v, ok=%v", got, ok)
	}
	if got, ok := reopened.Get("s1"); !ok || got.Reason != "flaky provider" {
		t.Errorf("s1 pause after reopen = %+v, ok=%v", got, ok)
	}
	if n := len(reopened.All()); n != 2 {
		t.Errorf("All() = %d entries, want 2", n)
	}
}

func TestPausesClearIsIdempotentAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pauses.json")
	p := OpenPauses(path)

	// Clearing a scope that was never paused is not an error.
	if err := p.Clear("never-paused"); err != nil {
		t.Fatal(err)
	}

	if err := p.Set("s1", ManualPause{Reason: "r", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := p.Clear("s1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Get("s1"); ok {
		t.Error("s1 still paused after Clear")
	}

	reopened := OpenPauses(path)
	if _, ok := reopened.Get("s1"); ok {
		t.Error("cleared pause came back after reopen")
	}
}

func TestOpenPausesStartsEmptyOnAMalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pauses.json")
	if err := writeAtomic(path, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	p := OpenPauses(path)
	if n := len(p.All()); n != 0 {
		t.Errorf("All() = %d entries from a malformed file, want 0", n)
	}
}

package source

import (
	"path/filepath"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	cfg := &config.Config{
		Stages: []config.Stage{{Name: "ready"}},
	}
	_ = cfgPath
	src := config.Source{Name: "s1"}
	return New(cfg, src, runner.New(t.TempDir()))
}

// CONTRACTS.md §1: ref is REQUIRED, exactly like id — a source that cannot
// tell the engine what to pass back to itself has produced an item the
// engine cannot act on, and skipping it silently would be the harder bug to
// find.
func TestValidateRejectsAnEmptyRef(t *testing.T) {
	c := testClient(t)
	ok, warns := c.validate([]model.Item{
		{ID: "s1:1", Ref: "", Stage: "ready", Title: "no ref"},
		{ID: "s1:2", Ref: "2", Stage: "ready", Title: "fine"},
	})
	if len(ok) != 1 || ok[0].ID != "s1:2" {
		t.Fatalf("validated = %v, want only s1:2", ok)
	}
	found := false
	for _, w := range warns {
		if w.Reason == "items[0] has no ref; skipped" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one naming items[0] with no ref", warns)
	}
}

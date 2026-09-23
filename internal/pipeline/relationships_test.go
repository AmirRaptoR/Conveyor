package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func TestSharedRelationshipValidationFixtures(t *testing.T) {
	var corpus struct {
		Validation []struct {
			Name            string       `json:"name"`
			Items           []model.Item `json:"items"`
			WarningContains []string     `json:"warningContains"`
		} `json:"validation"`
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "relationships.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range corpus.Validation {
		t.Run(fixture.Name, func(t *testing.T) {
			got := strings.Join(RelationshipWarnings(fixture.Items), "\n")
			for _, want := range fixture.WarningContains {
				if !strings.Contains(got, want) {
					t.Errorf("warnings = %q, want substring %q", got, want)
				}
			}
			if len(fixture.WarningContains) == 0 && got != "" {
				t.Errorf("warnings = %q, want none", got)
			}
		})
	}
}

func TestValidRelationshipsAreInformationalAndQuiet(t *testing.T) {
	items := []model.Item{
		{ID: "s:1", Source: "s", Children: []string{"s:2"}},
		{ID: "s:2", Source: "s", Parent: "s:1", DependsOn: []string{"s:9"}},
	}
	if got := RelationshipWarnings(items); len(got) != 0 {
		t.Fatalf("warnings = %v, want none", got)
	}
	// Parentage is deliberately not copied into DependsOn: tracking structure
	// must never become execution policy by implication.
	if strings.Join(items[1].DependsOn, ",") != "s:9" {
		t.Fatalf("dependsOn changed: %+v", items[1])
	}
}

func TestRelationshipWarningsAreActionableAndDeterministic(t *testing.T) {
	items := []model.Item{
		{ID: "s:1", Source: "s", Parent: "s:404", Children: []string{"s:1", "other:2", "s:3", "s:3"}},
		{ID: "other:2", Source: "other", Parent: "s:9"},
		{ID: "s:3", Source: "s", Parent: "s:8"},
	}
	got := RelationshipWarnings(items)
	wantParts := []string{
		"s:1 names itself as its child",
		"s:1 has cross-source child other:2",
		"s:1 names child s:3 more than once",
		"s:1 names child s:3, but s:3 names parent s:8",
	}
	joined := strings.Join(got, "\n")
	for _, want := range wantParts {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings = %v, want %q", got, want)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("warnings are not sorted: %v", got)
		}
	}
}

func TestContradictoryDirectionsAndParentCyclesWarn(t *testing.T) {
	items := []model.Item{
		{ID: "s:1", Source: "s", Parent: "s:2", Children: []string{"s:2"}},
		{ID: "s:2", Source: "s", Parent: "s:1", Children: []string{"s:1", "s:3"}},
		{ID: "s:3", Source: "s"},
	}
	got := strings.Join(RelationshipWarnings(items), "\n")
	if !strings.Contains(got, "parent cycle:") {
		t.Fatalf("warnings = %s, want a parent cycle", got)
	}
	if !strings.Contains(got, "s:2 names child s:3, but s:3 names no parent") {
		t.Fatalf("warnings = %s, want contradictory direction", got)
	}
}

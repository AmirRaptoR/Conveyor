package pipeline

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func TestGitHubFixtureOutputPassesThroughEngineValidation(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(root, "testdata", "relationships.json")
	tmp := t.TempDir()
	stub := filepath.Join(tmp, "gh")
	stubBody := `#!/usr/bin/env bash
case "$*" in
  *"issue view 999 "*)
    echo 'GraphQL: Could not resolve to an Issue with the number of 999.' >&2
    exit 1 ;;
  *"api --paginate --slurp repos/owner/repo/issues/208/sub_issues"*)
    jq '[.paginatedSubIssues]' "$REL_FIXTURE" ;;
  *"--state open"*) jq '.githubIssues' "$REL_FIXTURE" ;;
  *"--state closed"*) echo '[]' ;;
  *) echo "stub gh: unhandled: $*" >&2; exit 97 ;;
esac
`
	if err := os.WriteFile(stub, []byte(stubBody), 0o755); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(tmp, "result.json")
	cmd := exec.Command("bash", filepath.Join(root, "providers", "github", "list.sh"))
	cmd.Dir = filepath.Join(root, "providers", "github")
	cmd.Stdin = strings.NewReader(`{"stages":["backlog","done"],"terminalStages":["done"]}`)
	cmd.Env = []string{
		"PATH=" + tmp + ":" + os.Getenv("PATH"),
		"REL_FIXTURE=" + fixture,
		"REPO=owner/repo",
		"CONVEYOR_SOURCE=fixture",
		"CONVEYOR_RESULT=" + result,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("list.sh: %v\n%s", err, out)
	}
	b, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Items []model.Item `json:"items"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Items) == 0 {
		t.Fatalf("provider emitted no items: %s", b)
	}
	byID := map[string]model.Item{}
	for _, item := range envelope.Items {
		byID[item.ID] = item
	}
	if byID["fixture:202"].Parent != "fixture:201" {
		t.Fatalf("mixed-case native repo URL lost its parent: %+v", byID["fixture:202"])
	}
	if got := strings.Join(byID["fixture:208"].Children, ","); got != "fixture:202,fixture:203" {
		t.Fatalf("paginated children = %q", got)
	}
	warnings := strings.Join(RelationshipWarnings(envelope.Items), "\n")
	for _, want := range []string{
		"fixture:204 names parent fixture:201, but fixture:201 does not name it as a child",
		"fixture:208 names child fixture:202, but fixture:202 names parent fixture:201",
	} {
		if !strings.Contains(warnings, want) {
			t.Errorf("engine warnings = %q, want provider-derived contradiction %q", warnings, want)
		}
	}
}

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

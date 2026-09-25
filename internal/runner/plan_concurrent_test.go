package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// Two items in two sources publishing plans at once must keep separate
// state: OnPlan is called from each run's own tailer goroutine, so a shared
// subscriber has to demultiplex by (runID, itemID) itself, under -race.
func TestConcurrentRunsKeepSeparatePlanState(t *testing.T) {
	r := New(t.TempDir())
	tickInterval = 15 * time.Millisecond
	defer func() { tickInterval = time.Second }()

	var mu sync.Mutex
	seen := map[string]string{} // itemID -> last text seen
	r.OnPlan = func(runID, itemID string, u PlanUpdate) {
		if !u.HasRevision || len(u.Revision.Todos) == 0 {
			return
		}
		mu.Lock()
		seen[itemID] = u.Revision.Todos[0].Text
		mu.Unlock()
	}

	body := func(text string) string {
		return `printf '{"v":1,"rev":1,"at":"2026-09-24T12:00:00Z","todos":[{"id":"a","text":"` + text + `","status":"in_progress"}]}\n' > "$CONVEYOR_PLAN"
sleep 0.1
`
	}

	var wg sync.WaitGroup
	for _, spec := range []struct {
		itemID, source, text string
	}{
		{"repo-a:1", "repo-a", "working on a"},
		{"repo-b:1", "repo-b", "working on b"},
	} {
		spec := spec
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Run(context.Background(), Spec{
				Script: script(t, body(spec.text)), Kind: "stage", Workdir: t.TempDir(),
				Source: spec.source, Item: &model.Item{ID: spec.itemID, Ref: "1"},
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if seen["repo-a:1"] != "working on a" {
		t.Errorf("repo-a:1 = %q, want %q", seen["repo-a:1"], "working on a")
	}
	if seen["repo-b:1"] != "working on b" {
		t.Errorf("repo-b:1 = %q, want %q", seen["repo-b:1"], "working on b")
	}
}

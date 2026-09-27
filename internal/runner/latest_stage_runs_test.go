package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

func writeHistoryRun(t *testing.T, root, day, dirName string, run model.Run) string {
	t.Helper()
	dir := filepath.Join(root, day, dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	run.Dir = dir
	if err := WriteMeta(&run); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLatestStageRunsIndexesEveryLaneInOneNewestFirstWalk(t *testing.T) {
	root := t.TempDir()
	writeHistoryRun(t, root, "2026-09-26", "235959.000-old", model.Run{
		ID: "235959.000-old", Kind: "stage", ItemID: "s:1", To: "review",
	})
	newest := writeHistoryRun(t, root, "2026-09-27", "000002.000-new", model.Run{
		ID: "000002.000-new", Kind: "stage", ItemID: "s:1", To: "review",
	})
	writeHistoryRun(t, root, "2026-09-27", "000003.000-move", model.Run{
		ID: "000003.000-move", Kind: "move", ItemID: "s:1", To: "review",
	})
	writeHistoryRun(t, root, "2026-09-27", "000001.000-implement", model.Run{
		ID: "000001.000-implement", Kind: "stage", ItemID: "s:1", To: "in-progress",
	})
	writeHistoryRun(t, root, "2026-09-27", "000000.000-other", model.Run{
		ID: "000000.000-other", Kind: "stage", ItemID: "s:2", To: "review",
	})
	broken := filepath.Join(root, "2026-09-27", "000004.000-broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "meta.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := LatestStageRuns(root)
	if len(got) != 3 {
		t.Fatalf("indexed lanes = %d, want 3: %#v", len(got), got)
	}
	review := got[StageRunKey{ItemID: "s:1", Stage: "review"}]
	if review.ID != "000002.000-new" || review.Dir != newest {
		t.Fatalf("s:1 review = %+v, want newest run in %s", review, newest)
	}
	if got[StageRunKey{ItemID: "s:1", Stage: "in-progress"}].ID != "000001.000-implement" {
		t.Fatalf("s:1 in-progress lane missing or wrong: %#v", got)
	}
	if got[StageRunKey{ItemID: "s:2", Stage: "review"}].ID != "000000.000-other" {
		t.Fatalf("s:2 review lane missing or wrong: %#v", got)
	}

	one, ok := LatestStageRun(root, "s:1", "review")
	if !ok || one.ID != review.ID || one.Dir != review.Dir {
		t.Fatalf("single-lane lookup = %+v, %v; batch = %+v", one, ok, review)
	}
}

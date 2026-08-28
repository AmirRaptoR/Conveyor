package pipeline

import "testing"

// The engine attaches no meaning to these keys — it only has to deliver them,
// with the narrower scope winning so a stage can override a repo-wide default.
func TestMergeEnv(t *testing.T) {
	src := map[string]string{"REPO": "o/r", "MAX_TURNS": "40"}
	stage := map[string]string{"MAX_TURNS": "200", "ALLOWED_TOOLS": "Bash"}

	got := mergeEnv(src, stage)
	for k, want := range map[string]string{
		"REPO":          "o/r", // source-only survives
		"MAX_TURNS":     "200", // stage wins
		"ALLOWED_TOOLS": "Bash",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if src["MAX_TURNS"] != "40" {
		t.Error("merge mutated the source's map")
	}
	if got := mergeEnv(src, nil); len(got) != 2 {
		t.Errorf("no stage env should pass the source through, got %v", got)
	}
}

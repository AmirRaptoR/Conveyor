package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// GET /api/runs and GET /api/runs/{id} both read meta.json off disk
// (walkRuns), so this is the same redaction runner_test.go proves, exercised
// through the actual API surface a browser reaches.
func TestRunHistoryAPIRedactsConfiguredEnv(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "providers", "fake", "list.sh"), `#!/bin/sh
cat > "$CONVEYOR_RESULT" <<'JSON'
[{"id":"s1:1","ref":"1","source":"s1","stage":"backlog","title":"x"}]
JSON
`)
	writeScript(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(dir, "work.sh"), `#!/bin/sh
echo '{}' > "$CONVEYOR_RESULT"
exit 0
`)
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "conveyor.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: 1
poll: 24h
stages:
  - name: backlog
  - name: working
    script: work
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo
    env:
      API_TOKEN: swordfish
    scripts:
      work:
        script: ./work.sh
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	s, h := newModeServer(t, cfg, runner.New(filepath.Join(dir, "runs")), ModeManual)
	s.refresh(s.ctx)
	if !s.advance(s.ctx) {
		t.Fatal("advance() did not launch a transition")
	}
	waitFor(t, "the run to finish", func() bool { return s.inFlight.Load() == 0 })

	for _, path := range []string{"/api/runs", "/api/runs?item=s1:1"} {
		_, body := doReq(t, h, "GET", path, nil)
		if strings.Contains(string(body), "swordfish") {
			t.Errorf("GET %s leaked the secret value: %s", path, body)
		}
		if !strings.Contains(string(body), "API_TOKEN") {
			t.Errorf("GET %s dropped the key entirely: %s", path, body)
		}
	}

	var runs []RunMeta
	_, body := doReq(t, h, "GET", "/api/runs", nil)
	if err := json.Unmarshal(body, &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("no runs recorded")
	}
	_, single := doReq(t, h, "GET", "/api/runs/"+runs[0].ID, nil)
	if strings.Contains(string(single), "swordfish") {
		t.Errorf("GET /api/runs/{id} leaked the secret value: %s", single)
	}
}
